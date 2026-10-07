package mai

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPythonHostSavesOnlyEffectfulCalls(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(t.TempDir(), "session.json")
	a := newAgent(io.Discard, io.Discard, sessionFile, 0, false)
	sess := &session{Version: stateVersion, RepoRoot: repo, CWD: repo}
	host := a.pythonHost(sess, "outer")

	if _, err := host(context.Background(), 1, 1, 1, "read", json.RawMessage(`{"file_path":"f.txt"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Fatalf("read host call saved the session: %v", err)
	}
	if _, err := host(context.Background(), 1, 1, 2, "bash", json.RawMessage(`{"command":"true"}`)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatalf("bash host call did not save the session: %v", err)
	}
	var saved session
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.PythonActivities) != 2 || saved.PythonActivities[1].Status != "completed" {
		t.Fatalf("saved activities = %#v", saved.PythonActivities)
	}
}
