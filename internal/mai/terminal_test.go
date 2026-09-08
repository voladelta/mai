package mai

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTerminalApprovalHelper(t *testing.T) {
	if os.Getenv("MAI_TERMINAL_TEST_MODE") != "tty" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	approved, err := (&agent{}).terminalApproval(ctx, "rm /tmp/mai-test-only", "cancellation test; no command is run")
	if ctx.Err() == nil || err == nil || approved || time.Since(started) > time.Second {
		t.Fatalf("approval did not wait for and respond to cancellation: approved=%t err=%v ctx=%v duration=%s", approved, err, ctx.Err(), time.Since(started))
	}
}

func TestTerminalApprovalCancellation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("uses macOS script CLI to allocate a controlling terminal")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, heldWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	defer heldWriter.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/script", "-q", "/dev/null", executable, "-test.run=^TestTerminalApprovalHelper$", "-test.timeout=3s")
	cmd.Env = append(os.Environ(), "MAI_TERMINAL_TEST_MODE=tty")
	cmd.Stdin = input
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "PASS") {
		t.Fatalf("terminal cancellation failed: %v\n%s", err, out)
	}
}
