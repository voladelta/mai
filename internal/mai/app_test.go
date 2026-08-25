package mai

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainWithoutPromptShowsCurrentDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAI_STATE_DIR", dir)
	if err := saveJSON(filepath.Join(dir, "config.json"), config{Model: "sol", Effort: "h"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Main(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Current defaults: sol/high.") {
		t.Fatalf("stdout does not show current defaults:\n%s", stdout.String())
	}
}

func TestMainWithoutPromptReadsLegacyDefaultsWithoutMigrating(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MAI_STATE_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
	legacy := filepath.Join(home, ".mai", "config.json")
	if err := saveJSON(legacy, config{Model: "sol", Effort: "h"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Main(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Current defaults: sol/high.") {
		t.Fatalf("stdout does not show legacy defaults:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "mai", "config.json")); !os.IsNotExist(err) {
		t.Fatalf("read-only invocation migrated config: %v", err)
	}
}

func TestMainSaveDefaultsDoesNotRequirePrompt(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAI_STATE_DIR", dir)
	var stdout, stderr bytes.Buffer
	args := []string{"-m", "sol", "-e", "h", "--save-defaults"}
	if code := Main(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	cfg, err := loadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "sol" || cfg.Effort != "h" {
		t.Fatalf("saved config = %#v", cfg)
	}
	if !strings.Contains(stdout.String(), "Saved defaults: sol/high.") {
		t.Fatalf("unexpected stdout: %s", stdout.String())
	}
}

func TestMainSaveDefaultsPreservesUnspecifiedValue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAI_STATE_DIR", dir)
	if err := saveJSON(filepath.Join(dir, "config.json"), config{Model: "luna", Effort: "h"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"-m", "terra", "--save-defaults"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	cfg, err := loadConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "terra" || cfg.Effort != "h" {
		t.Fatalf("saved config = %#v", cfg)
	}
}

func TestTaskOptionsDoNotChangeDefaultsWithoutSaveFlag(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MAI_STATE_DIR", dir)
	t.Setenv("CODEX_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"hello", "-m", "sol", "-e", "h"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("task options unexpectedly wrote config: %v", err)
	}
}

func TestMainHelpDocumentsFullOptions(t *testing.T) {
	t.Setenv("MAI_STATE_DIR", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--unknown", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	for _, text := range []string{"--model", "--effort", "--save-defaults", "--timeout", "--no-input", "Documentation and support"} {
		if !strings.Contains(stdout.String(), text) {
			t.Fatalf("help is missing %q:\n%s", text, stdout.String())
		}
	}
}
