package mai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectSessionPaths(t *testing.T) {
	paths := projectSessionPaths(t.TempDir())
	if paths.current != filepath.Join(paths.dir, "current") ||
		paths.sessions != filepath.Join(paths.dir, "sessions") ||
		paths.locks != filepath.Join(paths.dir, "locks") {
		t.Fatalf("unexpected paths: %#v", paths)
	}
}

func TestPrepareSessionPathsUsesPrivatePermissionsAndIgnoresState(t *testing.T) {
	paths := projectSessionPaths(t.TempDir())
	if err := prepareSessionPaths(paths); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(paths.dir)
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile(filepath.Join(paths.dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 || string(ignore) != "*\n" {
		t.Fatalf("unexpected state setup: mode=%o ignore=%q", dirInfo.Mode().Perm(), ignore)
	}
}

func TestPrepareSessionPathsRejectsMaiSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	paths := projectSessionPaths(root)
	if err := os.Symlink(outside, paths.dir); err != nil {
		t.Fatal(err)
	}
	if err := prepareSessionPaths(paths); err == nil {
		t.Fatal(".mai symlink was accepted")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside directory was changed: entries=%v err=%v", entries, err)
	}
}

func TestCurrentSessionRoundTripRejectsInvalidID(t *testing.T) {
	paths := projectSessionPaths(t.TempDir())
	if err := prepareSessionPaths(paths); err != nil {
		t.Fatal(err)
	}
	id := "01234567-89ab-cdef-0123-456789abcdef"
	if err := saveCurrentSession(paths, id); err != nil {
		t.Fatal(err)
	}
	got, err := loadCurrentSessionID(paths)
	if err != nil || got != id {
		t.Fatalf("current session = %q, %v", got, err)
	}
	if err := atomicWriteFile(paths.current, []byte("../../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCurrentSessionID(paths); err == nil {
		t.Fatal("invalid current session ID was accepted")
	}
}

func TestSeparateSessionsUseSeparateFiles(t *testing.T) {
	root := t.TempDir()
	paths := projectSessionPaths(root)
	if err := prepareSessionPaths(paths); err != nil {
		t.Fatal(err)
	}
	ids := []string{"01234567-89ab-cdef-0123-456789abcdef", "fedcba98-7654-3210-fedc-ba9876543210"}
	for _, id := range ids {
		sess := session{Version: stateVersion, ID: id, CWD: root, RepoRoot: root, Model: "luna", Effort: "m"}
		if err := saveJSON(sessionPath(paths, id), sess); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		sess, err := loadSession(sessionPath(paths, id))
		if err != nil || sess.ID != id {
			t.Fatalf("load session %s: %#v, %v", id, sess, err)
		}
	}
}

func TestSessionLockRejectsConcurrentOwner(t *testing.T) {
	paths := projectSessionPaths(t.TempDir())
	if err := prepareSessionPaths(paths); err != nil {
		t.Fatal(err)
	}
	id := "01234567-89ab-cdef-0123-456789abcdef"
	first, err := acquireSessionLock(paths, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireSessionLock(paths, id); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireSessionLock(paths, id)
	if err != nil {
		t.Fatalf("lock remained held after close: %v", err)
	}
	second.Close()
}

func TestRepairInterruptedToolCalls(t *testing.T) {
	sess := &session{History: []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"done","name":"bash","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"done","output":"ok"}`),
		json.RawMessage(`{"type":"function_call","call_id":"pending","name":"apply_patch","arguments":"{}"}`),
	}}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	if len(sess.History) != 4 {
		t.Fatalf("history length = %d, want 4", len(sess.History))
	}
	var output struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(sess.History[3], &output); err != nil {
		t.Fatal(err)
	}
	if output.Type != "function_call_output" || output.CallID != "pending" || output.Output == "" {
		t.Fatalf("unexpected repair output: %#v", output)
	}
	var recovery struct {
		OK          *bool  `json:"ok"`
		Outcome     string `json:"outcome"`
		Error       string `json:"error"`
		Instruction string `json:"instruction"`
	}
	if err := json.Unmarshal([]byte(output.Output), &recovery); err != nil {
		t.Fatal(err)
	}
	if recovery.OK != nil || recovery.Outcome != "unknown" {
		t.Fatalf("recovery result does not state an unknown outcome: %#v", recovery)
	}
	if !strings.Contains(recovery.Error, "tool outcome is unknown") ||
		!strings.Contains(recovery.Instruction, "reconcile") ||
		!strings.Contains(recovery.Instruction, "before you retry apply_patch") {
		t.Fatalf("unsafe apply_patch recovery guidance: %#v", recovery)
	}
}

func TestRepairInterruptedBashRequiresConfirmationBeforeUnsafeRetry(t *testing.T) {
	sess := &session{History: []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"pending","name":"bash","arguments":"{\"command\":\"deploy\"}"}`),
	}}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	var output struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(sess.History[1], &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Output, `"outcome":"unknown"`) ||
		!strings.Contains(output.Output, "non-idempotent effects without user confirmation") {
		t.Fatalf("unsafe Bash recovery output: %s", output.Output)
	}
}

func TestInterruptedToolRecoveryPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	sess := &session{
		Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef", CWD: t.TempDir(), RepoRoot: t.TempDir(),
		Model: "luna", Effort: "max",
		History: []json.RawMessage{
			json.RawMessage(`{"type":"function_call","call_id":"pending","name":"apply_patch","arguments":"{}"}`),
		},
	}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(loaded.History[1], &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Output, `"outcome":"unknown"`) ||
		!strings.Contains(output.Output, "reconcile the requested patch") {
		t.Fatalf("persisted recovery output is incomplete: %s", output.Output)
	}
}

func TestRepairInterruptedToolCallsIsIdempotent(t *testing.T) {
	sess := &session{History: []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"call"}`),
	}}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	if len(sess.History) != 2 {
		t.Fatalf("history length = %d, want 2", len(sess.History))
	}
}
