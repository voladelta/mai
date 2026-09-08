package mai

import (
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed python_runner.py
var pythonRunner string

// pythonKernel belongs to one agent. The agent dispatcher serializes its calls.
type pythonKernel struct {
	cmd        *exec.Cmd
	request    *os.File
	response   *os.File
	stdout     *os.File
	stderr     *os.File
	lifetime   *os.File
	wait       chan error
	generation int
	cell       int
	runtime    *pythonRuntime
	messages   chan pythonMessage
	readStop   chan struct{}
	children   *childRegistry
}

type pythonResult struct {
	OK                bool                    `json:"ok"`
	Generation        int                     `json:"generation"`
	Fresh             bool                    `json:"fresh"`
	StateLost         bool                    `json:"state_lost"`
	Error             string                  `json:"error,omitempty"`
	Stdout            string                  `json:"stdout"`
	Stderr            string                  `json:"stderr"`
	Truncated         bool                    `json:"truncated"`
	StdoutBytes       int64                   `json:"stdout_bytes"`
	StderrBytes       int64                   `json:"stderr_bytes"`
	OmittedBytes      int64                   `json:"omitted_bytes"`
	TimedOut          bool                    `json:"timed_out"`
	DurationMS        int64                   `json:"duration_ms"`
	Cell              int                     `json:"cell"`
	Runtime           *pythonRuntime          `json:"runtime,omitempty"`
	Activities        []pythonActivitySummary `json:"activities,omitempty"`
	ActivitiesOmitted int                     `json:"activities_omitted,omitempty"`
}

func (k *pythonKernel) stop() {
	if k.children != nil {
		k.children.stop()
	}
	if k.cmd == nil {
		return
	}
	_ = syscall.Kill(-k.cmd.Process.Pid, syscall.SIGKILL)
	_ = k.lifetime.Close()
	close(k.readStop)
	_ = k.request.Close()
	_ = k.response.Close()
	<-k.wait
	k.cmd = nil
}

func (k *pythonKernel) close() {
	k.stop()
	if k.stdout != nil {
		_ = k.stdout.Close()
		_ = k.stderr.Close()
	}
}

func (k *pythonKernel) start(cwd string) error {
	python := os.Getenv("MAI_PYTHON")
	if python == "" {
		python = "python3"
	}
	path, err := exec.LookPath(python)
	if err != nil {
		return fmt.Errorf("Python is unavailable: install Python 3 or set MAI_PYTHON to its executable: %w", err)
	}
	// Dedicated request/response descriptors keep input() and printed output
	// separate from the control protocol. stdin defaults to the null device.
	var files []*os.File
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	pipe := func() (*os.File, *os.File, error) {
		r, w, err := os.Pipe()
		if err == nil {
			files = append(files, r, w)
		}
		return r, w, err
	}
	requestR, requestW, err := pipe()
	if err != nil {
		return err
	}
	responseR, responseW, err := pipe()
	if err != nil {
		return err
	}
	stdoutR, stdoutW, err := pipe()
	if err != nil {
		return err
	}
	stderrR, stderrW, err := pipe()
	if err != nil {
		return err
	}
	lifetimeR, lifetimeW, err := pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(path, "-u", "-c", pythonRunner, strconv.Itoa(k.generation+1))
	cmd.Dir = cwd
	cmd.ExtraFiles = []*os.File{requestR, responseW, lifetimeR}
	cmd.Stdout, cmd.Stderr = stdoutW, stderrW
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Python (check MAI_PYTHON): %w", err)
	}
	k.cmd, k.request, k.response = cmd, requestW, responseR
	k.stdout, k.stderr = stdoutR, stderrR
	k.lifetime = lifetimeW
	k.generation++
	k.runtime = nil
	k.messages = make(chan pythonMessage, maxPythonPending)
	k.readStop = make(chan struct{})
	go readPythonFrames(responseR, k.messages, k.readStop)
	wait := make(chan error, 1)
	k.wait = wait
	children := k.children
	go func() {
		err := cmd.Wait()
		if children != nil {
			children.stop()
		}
		wait <- err
		close(wait)
	}()
	files = []*os.File{requestR, responseW, stdoutW, stderrW, lifetimeR}
	return nil
}

// drainPythonOutput uses a per-cell random delimiter only as a stream barrier;
// status travels on a separate pipe. Memory stays bounded even without newlines.
func drainPythonOutput(file *os.File, marker []byte, output *cappedBuffer) error {
	pending := make([]byte, 0, 8192+len(marker))
	chunk := make([]byte, 8192)
	for {
		n, err := file.Read(chunk)
		pending = append(pending, chunk[:n]...)
		if index := bytes.Index(pending, marker); index >= 0 {
			_, _ = output.Write(pending[:index])
			return nil
		}
		if keep := len(marker) - 1; len(pending) > keep {
			_, _ = output.Write(pending[:len(pending)-keep])
			pending = append(pending[:0], pending[len(pending)-keep:]...)
		}
		if err != nil {
			_, _ = output.Write(pending)
			return err
		}
	}
}

func (k *pythonKernel) execute(parent context.Context, cwd, code string, reset bool, timeout time.Duration, hosts ...pythonHostHandler) pythonResult {
	started := time.Now()
	result := pythonResult{Generation: k.generation}
	if reset {
		result.StateLost = k.cmd != nil
		k.close()
		result.OK = true
		return result
	}
	if err := parent.Err(); err != nil {
		result.StateLost = k.cmd != nil
		k.close()
		result.Error = err.Error()
		return result
	}
	if k.cmd != nil {
		select {
		case <-k.wait:
			k.close()
			result.StateLost = true
			result.Error = "Python exited between cells; state was lost. This cell was not executed."
			return result
		default:
		}
	}
	if k.cmd == nil {
		if err := k.start(cwd); err != nil {
			result.Error = err.Error()
			return result
		}
		result.Fresh = true
		result.Generation = k.generation
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	k.cell++
	result.Cell = k.cell
	var host pythonHostHandler
	if len(hosts) > 0 {
		host = hosts[0]
	}
	var random [24]byte
	_, _ = rand.Read(random[:])
	marker := "\x00mai-" + hex.EncodeToString(random[:]) + "\x00"
	var stdout, stderr cappedBuffer
	stdout.max, stderr.max = maxToolStreamBytes, maxToolStreamBytes
	done := make(chan error, 3)
	go func() { done <- drainPythonOutput(k.stdout, []byte(marker), &stdout) }()
	go func() { done <- drainPythonOutput(k.stderr, []byte(marker), &stderr) }()
	var status bool
	go func() {
		var err error
		status, err = k.runProtocol(ctx, code, marker, host)
		done <- err
	}()
	var failure error
	stop := func() {
		cancel()
		k.stop()
		// Keep buffered output after a crash. A descendant outside the group
		// can hold these pipes open, so draining must have a deadline.
		deadline := time.Now().Add(time.Second)
		_ = k.stdout.SetReadDeadline(deadline)
		_ = k.stderr.SetReadDeadline(deadline)
	}
	for remaining := 3; remaining > 0; remaining-- {
		select {
		case err := <-done:
			if err != nil && failure == nil {
				failure = err
				stop()
			}
		case <-ctx.Done():
			if failure == nil {
				failure = ctx.Err()
				stop()
			}
			// Collect all workers after stopping the process and bounding reads.
			<-done
		}
	}
	if failure != nil {
		k.close()
	}
	result.OK = failure == nil && status
	result.Runtime = k.runtime
	result.Stdout, result.Stderr = stdout.String(), stderr.String()
	result.StdoutBytes, result.StderrBytes = stdout.TotalBytes(), stderr.TotalBytes()
	result.OmittedBytes = stdout.OmittedBytes() + stderr.OmittedBytes()
	result.Truncated = result.OmittedBytes > 0
	result.DurationMS = time.Since(started).Milliseconds()
	result.TimedOut = errors.Is(failure, context.DeadlineExceeded)
	if failure != nil {
		result.StateLost = true
		result.Error = fmt.Sprintf("Python stopped: %v. State was lost; external effects may have completed. Do not replay the cell automatically.", failure)
	} else if !status {
		result.Error = "Python cell failed; partial changes remain in the namespace."
	}
	return result
}

func (a *agent) executePython(ctx context.Context, sess *session, arguments string, outerCall string) json.RawMessage {
	var args map[string]json.RawMessage
	err := json.Unmarshal([]byte(arguments), &args)
	var code string
	var reset bool
	if err == nil && len(args) == 1 {
		if raw, ok := args["code"]; ok {
			err = json.Unmarshal(raw, &code)
			if strings.TrimSpace(code) == "" {
				err = errors.New("code must be a nonempty string")
			}
		} else if raw, ok := args["reset"]; ok && string(raw) == "true" {
			reset = true
		} else {
			err = errors.New("expected code or reset:true")
		}
	} else if err == nil {
		err = errors.New("provide exactly one of code or reset:true")
	}
	if err != nil {
		return textToolOutput(toolError("invalid python arguments", err))
	}
	fmt.Fprintln(a.stderr, "→ python")
	if err := a.prepareChildren(); err != nil {
		a.children = &childRegistry{runs: make(map[string]*childRun), failure: err}
	}
	if a.python.cmd == nil && a.children.isClosed() {
		a.children = a.children.nextGeneration()
	}
	a.python.children = a.children
	start := len(sess.PythonActivities)
	host := a.pythonHost(sess, outerCall)
	result := a.python.execute(ctx, sess.CWD, code, reset, a.timeout, func(cellCtx context.Context, generation, cell, call int, name string, args json.RawMessage) (json.RawMessage, error) {
		if name == "spawn" || name == "child_status" || name == "child_cancel" {
			activity := pythonActivity{OuterCallID: outerCall, Generation: generation, Cell: cell, Call: call, Name: name, Arguments: args}
			raw, err := a.childHost(ctx, sess, activity)
			if name != "child_status" {
				activity.Status = "admitted"
				if name == "child_cancel" {
					activity.Status = "cancel_requested"
				}
				var outcome struct {
					Error string `json:"error"`
				}
				if err != nil || json.Unmarshal(raw, &outcome) != nil || outcome.Error != "" {
					activity.Status = "failed"
				}
				activity.Result = raw
				sess.PythonActivities = append(sess.PythonActivities, activity)
			}
			return raw, err
		}
		return host(cellCtx, generation, cell, call, name, args)
	})
	result.Activities, result.ActivitiesOmitted = summarizePythonActivities(sess.PythonActivities[start:])
	return textToolOutput(marshalToolResult(result))
}
