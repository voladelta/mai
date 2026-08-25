package mai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	defaultBashTimeout = 120 * time.Second
	maxBashTimeout     = 10 * time.Minute
	maxToolStreamBytes = 1 << 20
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
	OK        bool   `json:"ok"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code"`
	TimedOut  bool   `json:"timed_out"`
	Truncated bool   `json:"truncated"`
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
	err := cmd.Run()

	result := bashResult{
		OK: err == nil, Stdout: stdout.String(), Stderr: stderr.String(),
		ExitCode: 0, TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded),
		Truncated: stdout.truncated || stderr.truncated,
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
	bytes.Buffer
	max       int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.max - b.Len()
	if remaining <= 0 {
		b.truncated = true
		return original, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
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
