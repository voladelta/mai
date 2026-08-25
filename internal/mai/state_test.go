package mai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveJSONUsesStatePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("MAI_STATE_DIR", dir)
	paths, err := statePaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.config != filepath.Join(dir, "config.json") || paths.session != filepath.Join(dir, "session.json") {
		t.Fatalf("unexpected paths: %#v", paths)
	}
	if err := saveJSON(paths.config, config{Model: "luna", Effort: "max"}); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Stat(paths.config)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected permissions: dir=%o file=%o", dirInfo.Mode().Perm(), fileInfo.Mode().Perm())
	}
}

func TestStatePathsFollowXDG(t *testing.T) {
	home := t.TempDir()
	configHome := filepath.Join(home, "configuration")
	stateHome := filepath.Join(home, "state")
	t.Setenv("HOME", home)
	t.Setenv("MAI_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	paths, err := statePaths()
	if err != nil {
		t.Fatal(err)
	}
	if paths.config != filepath.Join(configHome, "mai", "config.json") {
		t.Fatalf("config path = %q", paths.config)
	}
	if paths.session != filepath.Join(stateHome, "mai", "session.json") {
		t.Fatalf("session path = %q", paths.session)
	}
}

func TestMigrateLegacyStateDoesNotOverwriteXDGFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MAI_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	paths, err := statePaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(paths.legacyConfig, config{Model: "sol", Effort: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(paths.legacySession, session{
		Version: stateVersion, ID: "session", CWD: home, RepoRoot: home,
		Model: "luna", Effort: "max",
	}); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyState(paths); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(paths.config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "sol" || cfg.Effort != "h" {
		t.Fatalf("migrated config = %#v", cfg)
	}
	sess, err := loadSession(paths.session)
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "session" || sess.CWD != home {
		t.Fatalf("migrated session = %#v", sess)
	}
	if err := saveJSON(paths.config, config{Model: "terra", Effort: "m"}); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyState(paths); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadConfig(paths.config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "terra" || cfg.Effort != "m" {
		t.Fatalf("migration overwrote XDG config: %#v", cfg)
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
