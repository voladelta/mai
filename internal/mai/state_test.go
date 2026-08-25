package mai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestStatePathsOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAI_STATE_DIR", dir)
	got, err := statePaths()
	if err != nil {
		t.Fatal(err)
	}
	if got.config != filepath.Join(dir, "config.json") || got.session != filepath.Join(dir, "session.json") {
		t.Fatalf("unexpected paths: %#v", got)
	}
}

func TestSaveJSONUsesStatePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, "config.json")
	if err := saveJSON(path, config{Model: "luna", Effort: "max"}); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected permissions: dir=%o file=%o", dirInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
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
