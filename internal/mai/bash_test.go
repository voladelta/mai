package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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
