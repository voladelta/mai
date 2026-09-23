package mai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultBashTimeout  = 120 * time.Second
	maxBashTimeout      = 10 * time.Minute
	maxToolStreamBytes  = 64 << 10
	maxBashCaptureBytes = 32 << 20
)

type approvalFunc func(context.Context, string, string) (bool, error)

type bashRequest struct {
	Command   string
	TimeoutMS int
	CWD       string
	RepoRoot  string
	Approve   approvalFunc
}

type bashResult struct {
	OK                bool   `json:"ok"`
	Stdout            string `json:"stdout"`
	Stderr            string `json:"stderr"`
	ExitCode          int    `json:"exit_code"`
	TimedOut          bool   `json:"timed_out"`
	Truncated         bool   `json:"truncated"`
	DurationMS        int64  `json:"duration_ms"`
	StdoutBytes       int64  `json:"stdout_bytes"`
	StderrBytes       int64  `json:"stderr_bytes"`
	OmittedBytes      int64  `json:"omitted_bytes"`
	StdoutCapturePath string `json:"stdout_capture_path,omitempty"`
	StderrCapturePath string `json:"stderr_capture_path,omitempty"`
	CaptureTruncated  bool   `json:"capture_truncated,omitempty"`
	CaptureError      string `json:"capture_error,omitempty"`
}

func runBash(parent context.Context, req bashRequest) string {
	timeout := defaultBashTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return toolError("bash cancelled before dispatch", err)
	}

	if required, reason := requiresRMApproval(req.Command, req.CWD, req.RepoRoot); required {
		if req.Approve == nil {
			return toolError("rm approval required", errors.New(reason))
		}
		approved, err := req.Approve(ctx, req.Command, reason)
		if err != nil {
			return toolError("rm approval failed", err)
		}
		if !approved {
			return toolError("rm denied", errors.New(reason))
		}
	}

	if err := ctx.Err(); err != nil {
		return toolError("bash cancelled before dispatch", err)
	}
	cmd, cleanup, err := ownedCommand(ctx, "/bin/bash", "-c", req.Command)
	if err != nil {
		return toolError("start bash", err)
	}
	defer cleanup()
	cmd.Dir = req.CWD
	cmd.Env = cleanShellEnv(cmd.Environ())
	captureDir, err := os.MkdirTemp("", "mai-bash-")
	if err != nil {
		return toolError("prepare bash capture", err)
	}
	stdoutPath := filepath.Join(captureDir, "stdout")
	stderrPath := filepath.Join(captureDir, "stderr")
	stdoutFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = os.RemoveAll(captureDir)
		return toolError("prepare bash capture", err)
	}
	stderrFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		_ = stdoutFile.Close()
		_ = os.RemoveAll(captureDir)
		return toolError("prepare bash capture", err)
	}
	var stdout, stderr cappedBuffer
	stdout.max = maxToolStreamBytes
	stderr.max = maxToolStreamBytes
	stdoutCapture := &boundedCapture{file: stdoutFile, limit: maxBashCaptureBytes}
	stderrCapture := &boundedCapture{file: stderrFile, limit: maxBashCaptureBytes}
	cmd.Stdout = io.MultiWriter(&stdout, stdoutCapture)
	cmd.Stderr = io.MultiWriter(&stderr, stderrCapture)
	started := time.Now()
	err = cmd.Run()
	stdoutCloseErr := stdoutFile.Close()
	stderrCloseErr := stderrFile.Close()

	result := bashResult{
		OK: err == nil, Stdout: stdout.String(), Stderr: stderr.String(),
		ExitCode: 0, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
		Truncated:   stdout.Truncated() || stderr.Truncated(),
		DurationMS:  time.Since(started).Milliseconds(),
		StdoutBytes: stdout.TotalBytes(), StderrBytes: stderr.TotalBytes(),
		OmittedBytes:     stdout.OmittedBytes() + stderr.OmittedBytes(),
		CaptureTruncated: stdoutCapture.truncated || stderrCapture.truncated,
	}
	if stdoutCapture.err != nil || stdoutCloseErr != nil || stderrCapture.err != nil || stderrCloseErr != nil {
		result.CaptureError = "full output capture failed"
	}
	if result.Truncated {
		if stdout.Truncated() && stdoutCapture.err == nil && stdoutCloseErr == nil {
			result.StdoutCapturePath = stdoutPath
		}
		if stderr.Truncated() && stderrCapture.err == nil && stderrCloseErr == nil {
			result.StderrCapturePath = stderrPath
		}
		if result.StdoutCapturePath == "" {
			_ = os.Remove(stdoutPath)
		}
		if result.StderrCapturePath == "" {
			_ = os.Remove(stderrPath)
		}
		if result.StdoutCapturePath == "" && result.StderrCapturePath == "" {
			_ = os.Remove(captureDir)
		}
	} else {
		_ = os.RemoveAll(captureDir)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
			if result.Stderr == "" {
				result.Stderr = err.Error()
			}
		}
	}
	b, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return toolError("encode bash result", marshalErr)
	}
	return string(b)
}

type boundedCapture struct {
	file      *os.File
	limit     int64
	written   int64
	truncated bool
	err       error
}

func (capture *boundedCapture) Write(p []byte) (int, error) {
	length := len(p)
	remaining := capture.limit - capture.written
	if remaining <= 0 {
		capture.truncated = true
		return length, nil
	}
	if int64(length) > remaining {
		p = p[:remaining]
		capture.truncated = true
	}
	if capture.err == nil {
		written, err := capture.file.Write(p)
		capture.written += int64(written)
		capture.err = err
		if written < len(p) && err == nil {
			capture.err = io.ErrShortWrite
		}
	}
	return length, nil
}

type cappedBuffer struct {
	mu    sync.Mutex
	head  []byte
	tail  []byte
	max   int
	total int64
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	original := len(p)
	b.total += int64(original)
	if b.max <= 0 {
		return original, nil
	}
	headLimit := b.max / 2
	if len(b.head) < headLimit {
		kept := min(len(p), headLimit-len(b.head))
		b.head = append(b.head, p[:kept]...)
		p = p[kept:]
	}
	tailLimit := b.max - headLimit
	if len(p) == 0 || tailLimit == 0 {
		return original, nil
	}
	if len(p) >= tailLimit {
		b.tail = append(b.tail[:0], p[len(p)-tailLimit:]...)
		return original, nil
	}
	overflow := len(b.tail) + len(p) - tailLimit
	if overflow > 0 {
		copy(b.tail, b.tail[overflow:])
		b.tail = b.tail[:len(b.tail)-overflow]
	}
	b.tail = append(b.tail, p...)
	return original, nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if omitted := b.omittedBytes(); omitted > 0 {
		return string(b.head) + fmt.Sprintf("\n... %d bytes omitted ...\n", omitted) + string(b.tail)
	}
	return string(b.head) + string(b.tail)
}

func (b *cappedBuffer) TotalBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

func (b *cappedBuffer) OmittedBytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.omittedBytes()
}

func (b *cappedBuffer) Truncated() bool {
	return b.OmittedBytes() > 0
}

func (b *cappedBuffer) omittedBytes() int64 {
	return max(0, b.total-int64(len(b.head)+len(b.tail)))
}

func cleanShellEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, item := range env {
		if strings.HasPrefix(item, "BASH_ENV=") || strings.HasPrefix(item, "ENV=") {
			continue
		}
		out = append(out, item)
	}
	return out
}

func (a *agent) terminalApproval(ctx context.Context, command, reason string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if err != nil {
		return false, fmt.Errorf("cannot ask for approval without a terminal: %w", err)
	}
	defer tty.Close()
	stop := context.AfterFunc(ctx, func() { _ = tty.Close() })
	defer stop()
	fmt.Fprintf(tty, "\nmai wants to run rm outside the repository.\nReason: %s\nCommand: %s\nApprove? [y/N] ", reason, command)
	// Some terminals cannot use Go's runtime poller. Nonblocking reads keep
	// cancellation bounded on those terminals without leaving a reader behind.
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var answer strings.Builder
	var buffer [256]byte
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, err := tty.Read(buffer[:])
		if n > 0 {
			part := string(buffer[:n])
			line, _, complete := strings.Cut(part, "\n")
			answer.WriteString(line)
			if answer.Len() > 1024 {
				return false, errors.New("approval response exceeds 1024 bytes")
			}
			if complete {
				value := strings.ToLower(strings.TrimSpace(answer.String()))
				return value == "y" || value == "yes", nil
			}
		}
		if err != nil && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-ticker.C:
		}
	}
}
