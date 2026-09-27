package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCappedBufferKeepsHeadAndTailThroughIOCopy(t *testing.T) {
	buffer := &cappedBuffer{max: 8}
	written, err := io.Copy(buffer, bytes.NewBufferString("abcdefghijkl"))
	if err != nil {
		t.Fatal(err)
	}
	if written != 12 || buffer.TotalBytes() != 12 || buffer.OmittedBytes() != 4 || !buffer.Truncated() {
		t.Fatalf("unexpected buffer metrics: written=%d total=%d omitted=%d truncated=%t", written, buffer.TotalBytes(), buffer.OmittedBytes(), buffer.Truncated())
	}
	result := buffer.String()
	if !strings.HasPrefix(result, "abcd") || !strings.HasSuffix(result, "ijkl") || !strings.Contains(result, "4 bytes omitted") {
		t.Fatalf("unexpected bounded output: %q", result)
	}
}

func TestCappedBufferUpdatesTailAcrossWrites(t *testing.T) {
	buffer := &cappedBuffer{max: 8}
	for _, part := range []string{"abc", "def", "ghijkl"} {
		if _, err := buffer.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if result := buffer.String(); !strings.HasPrefix(result, "abcd") || !strings.HasSuffix(result, "ijkl") || buffer.OmittedBytes() != 4 {
		t.Fatalf("unexpected multi-write result: %q, omitted=%d", result, buffer.OmittedBytes())
	}
}

func TestBoundedCaptureStopsAtDiskLimit(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "capture")
	if err != nil {
		t.Fatal(err)
	}
	capture := &boundedCapture{file: file, limit: 4}
	if written, err := capture.Write([]byte("abcdef")); written != 6 || err != nil {
		t.Fatalf("write=%d err=%v", written, err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abcd" || !capture.truncated {
		t.Fatalf("capture=%q truncated=%t", data, capture.truncated)
	}
}

func TestRunBashCapturesResult(t *testing.T) {
	root := t.TempDir()
	raw := runBash(context.Background(), bashRequest{Command: "printf out; printf err >&2; exit 7", CWD: root, RepoRoot: root})
	var result bashResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Stdout != "out" || result.Stderr != "err" || result.ExitCode != 7 ||
		result.StdoutBytes != 3 || result.StderrBytes != 3 || result.OmittedBytes != 0 || result.Truncated {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestRunBashBoundsLargeOutputAndKeepsBothEnds(t *testing.T) {
	root := t.TempDir()
	raw := runBash(context.Background(), bashRequest{
		Command: "printf HEAD; head -c 70000 /dev/zero | tr '\\000' x; printf TAIL",
		CWD:     root, RepoRoot: root,
	})
	var result bashResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.Truncated || result.StdoutBytes != 70008 || result.OmittedBytes != 70008-maxToolStreamBytes {
		t.Fatalf("unexpected large output metrics: %#v", result)
	}
	if !strings.HasPrefix(result.Stdout, "HEAD") || !strings.HasSuffix(result.Stdout, "TAIL") || !strings.Contains(result.Stdout, "bytes omitted") {
		t.Fatalf("large output lost its boundaries: %q", result.Stdout)
	}
	if result.StdoutCapturePath == "" || result.CaptureTruncated {
		t.Fatalf("missing complete capture: %#v", result)
	}
	captured, err := os.ReadFile(result.StdoutCapturePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(captured) != 70008 || !bytes.HasPrefix(captured, []byte("HEAD")) || !bytes.HasSuffix(captured, []byte("TAIL")) {
		t.Fatalf("capture lost output: length=%d", len(captured))
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(result.StdoutCapturePath)) })
}

func TestRunBashRejectsApprovalWhenInputIsUnavailable(t *testing.T) {
	root := t.TempDir()
	raw := runBash(context.Background(), bashRequest{
		Command: "rm /tmp/mai-outside-repository",
		CWD:     root, RepoRoot: root,
	})
	if !strings.Contains(raw, "rm approval required") {
		t.Fatalf("unexpected result: %s", raw)
	}
}

func TestRunBashPreservesTargetAfterWrapperDirectoryChange(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "victim")
	mustWrite(t, target, "keep")

	raw := runBash(context.Background(), bashRequest{
		Command: "env --chdir='" + outside + "' rm victim",
		CWD:     root, RepoRoot: root,
	})
	if !strings.Contains(raw, "rm approval required") {
		t.Fatalf("expected approval rejection: %s", raw)
	}

	assertContent(t, target, "keep")
}

func TestRunBashDoesNotExecutePrefixedExternalRMWithoutApproval(t *testing.T) {
	root := t.TempDir()
	for _, prefix := range []string{"!", "time", "exec"} {
		t.Run(prefix, func(t *testing.T) {
			outside := t.TempDir()
			target := filepath.Join(outside, "keep.txt")
			if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
				t.Fatal(err)
			}
			raw := runBash(context.Background(), bashRequest{
				Command: prefix + " rm -f " + target,
				CWD:     root, RepoRoot: root,
			})
			if !strings.Contains(raw, "rm approval required") {
				t.Fatalf("unexpected result: %s", raw)
			}
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("rm command ran without approval: %v", err)
			}
		})
	}
}

func TestRunBashDoesNotExecuteUnclassifiedRMWithoutApproval(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	raw := runBash(context.Background(), bashRequest{
		Command: "nice -n 1 rm -f " + target,
		CWD:     root, RepoRoot: root,
	})
	if !strings.Contains(raw, "rm approval required") {
		t.Fatalf("unexpected result: %s", raw)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("rm command ran without approval: %v", err)
	}
}

func TestRunBashRejectsPipedRMWithoutApproval(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "keep.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := "printf '%s\\n' " + strconv.Quote("rm -f "+strconv.Quote(target)) + " | sh"
	raw := runBash(context.Background(), bashRequest{Command: command, CWD: root, RepoRoot: root})
	if !strings.Contains(raw, "rm approval required") {
		t.Fatalf("piped rm was not rejected: %s", raw)
	}
	assertContent(t, target, "keep")
}

type approvalReadStep struct {
	data string
	err  error
	do   func()
}

type scriptedApprovalReader struct {
	steps   []approvalReadStep
	pending string
	err     error
}

func (r *scriptedApprovalReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		if r.err != nil {
			err := r.err
			r.err = nil
			return 0, err
		}
		if len(r.steps) == 0 {
			return 0, syscall.EAGAIN
		}
		step := r.steps[0]
		r.steps = r.steps[1:]
		if step.do != nil {
			step.do()
		}
		r.pending = step.data
		r.err = step.err
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func TestReadApproval(t *testing.T) {
	for name, test := range map[string]struct {
		steps   []approvalReadStep
		want    bool
		wantErr string
	}{
		"approve":            {steps: []approvalReadStep{{data: "y\n"}}, want: true},
		"approve yes padded": {steps: []approvalReadStep{{data: "  YES \n"}}, want: true},
		"deny":               {steps: []approvalReadStep{{data: "n\n"}}, want: false},
		"empty denies":       {steps: []approvalReadStep{{data: "\n"}}, want: false},
		"split answer":       {steps: []approvalReadStep{{data: "y"}, {data: "es\n"}}, want: true},
		"eagain retries":     {steps: []approvalReadStep{{err: syscall.EAGAIN}, {data: "y\n"}}, want: true},
		"eintr retries":      {steps: []approvalReadStep{{err: syscall.EINTR}, {data: "y\n"}}, want: true},
		"overlong answer": {
			steps: []approvalReadStep{
				{data: strings.Repeat("x", 300)}, {data: strings.Repeat("x", 300)},
				{data: strings.Repeat("x", 300)}, {data: strings.Repeat("x", 300)},
			},
			wantErr: "exceeds 1024 bytes",
		},
		"read error": {steps: []approvalReadStep{{err: errors.New("read failed")}}, wantErr: "read failed"},
	} {
		t.Run(name, func(t *testing.T) {
			approved, err := readApproval(context.Background(), &scriptedApprovalReader{steps: test.steps}, time.Millisecond)
			if test.wantErr == "" {
				if err != nil || approved != test.want {
					t.Fatalf("readApproval = %t, %v; want %t", approved, err, test.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("readApproval error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestReadApprovalReturnsContextErrorWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	approved, err := readApproval(ctx, &scriptedApprovalReader{}, time.Millisecond)
	if approved || !errors.Is(err, context.Canceled) {
		t.Fatalf("readApproval = %t, %v; want context.Canceled", approved, err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reader := &scriptedApprovalReader{steps: []approvalReadStep{{err: errors.New("read failed"), do: cancel}}}
	approved, err = readApproval(ctx, reader, time.Millisecond)
	if approved || !errors.Is(err, context.Canceled) {
		t.Fatalf("readApproval after cancellation = %t, %v; want context.Canceled", approved, err)
	}
}

func TestRunBashTimesOutProcessGroup(t *testing.T) {
	root := t.TempDir()
	start := time.Now()
	raw := runBash(context.Background(), bashRequest{Command: "sleep 5", TimeoutMS: 50, CWD: root, RepoRoot: root})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("timeout took too long: %v", elapsed)
	}
	var result bashResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.ExitCode == 0 || result.DurationMS < 40 {
		t.Fatalf("unexpected timeout result: %#v", result)
	}
}
