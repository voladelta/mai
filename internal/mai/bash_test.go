package mai

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRunBashCapturesResult(t *testing.T) {
	root := t.TempDir()
	raw := runBash(context.Background(), bashRequest{Command: "printf out; printf err >&2; exit 7", CWD: root, RepoRoot: root})
	var result bashResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Stdout != "out" || result.Stderr != "err" || result.ExitCode != 7 {
		t.Fatalf("unexpected result: %#v", result)
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
	if !result.TimedOut || result.ExitCode == 0 {
		t.Fatalf("unexpected timeout result: %#v", result)
	}
}
