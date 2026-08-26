package mai

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	defaultBashTimeout = 120 * time.Second
	maxBashTimeout     = 10 * time.Minute
	maxToolStreamBytes = 64 << 10
)

type approvalFunc func(command, reason string) (bool, error)

type bashRequest struct {
	Command   string
	TimeoutMS int
	CWD       string
	RepoRoot  string
	Approve   approvalFunc
}

type bashResult struct {
	OK           bool   `json:"ok"`
	Stdout       string `json:"stdout"`
	Stderr       string `json:"stderr"`
	ExitCode     int    `json:"exit_code"`
	TimedOut     bool   `json:"timed_out"`
	Truncated    bool   `json:"truncated"`
	DurationMS   int64  `json:"duration_ms"`
	StdoutBytes  int64  `json:"stdout_bytes"`
	StderrBytes  int64  `json:"stderr_bytes"`
	OmittedBytes int64  `json:"omitted_bytes"`
}

func runBash(parent context.Context, req bashRequest) string {
	if required, reason := requiresRMApproval(req.Command, req.CWD, req.RepoRoot); required {
		if req.Approve == nil {
			return toolError("rm approval required", errors.New(reason))
		}
		approved, err := req.Approve(req.Command, reason)
		if err != nil {
			return toolError("rm approval failed", err)
		}
		if !approved {
			return toolError("rm denied", errors.New(reason))
		}
	}

	timeout := defaultBashTimeout
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/bash", "-c", req.Command)
	cmd.Dir = req.CWD
	cmd.Env = cleanShellEnv(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr cappedBuffer
	stdout.max = maxToolStreamBytes
	stderr.max = maxToolStreamBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	err := cmd.Run()

	result := bashResult{
		OK: err == nil, Stdout: stdout.String(), Stderr: stderr.String(),
		ExitCode: 0, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
		Truncated:   stdout.Truncated() || stderr.Truncated(),
		DurationMS:  time.Since(started).Milliseconds(),
		StdoutBytes: stdout.TotalBytes(), StderrBytes: stderr.TotalBytes(),
		OmittedBytes: stdout.OmittedBytes() + stderr.OmittedBytes(),
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

func (a *agent) terminalApproval(command, reason string) (bool, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("cannot ask for approval without a terminal: %w", err)
	}
	defer tty.Close()
	fmt.Fprintf(tty, "\nmai wants to run rm outside the repository.\nReason: %s\nCommand: %s\nApprove? [y/N] ", reason, command)
	answer, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil {
		return false, err
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
