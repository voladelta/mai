package mai

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainWithoutPromptShowsBuiltInDefault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Built-in default: luna/medium.") {
		t.Fatalf("stdout does not show the built-in default:\n%s", stdout.String())
	}
}

func TestStatelessTaskDoesNotCreateMaiDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CODEX_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"hello"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".mai")); !os.IsNotExist(err) {
		t.Fatalf("stateless task created .mai: %v", err)
	}
}

func TestPersistCreatesProjectSessionAndCurrentPointer(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("CODEX_HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"hello", "--persist", "-m", "sol", "-e", "h"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	paths := projectSessionPaths(root)
	id, err := loadCurrentSessionID(paths)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := loadSession(sessionPath(paths, id))
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != id || sess.Model != "sol" || sess.Effort != "h" || len(sess.History) != 1 {
		t.Fatalf("saved session = %#v", sess)
	}
}

func TestConcurrentPersistedTasksKeepSeparateHistory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	cfg := taskConfig{Model: "luna", Effort: "m"}

	first, err := startSession(cfg, options{persist: true})
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	if err := appendUserPrompt(first.session, "first task"); err != nil {
		t.Fatal(err)
	}
	if err := first.saveInitial(); err != nil {
		t.Fatal(err)
	}

	second, err := startSession(cfg, options{persist: true})
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	if err := appendUserPrompt(second.session, "second task"); err != nil {
		t.Fatal(err)
	}
	if err := second.saveInitial(); err != nil {
		t.Fatal(err)
	}

	if first.path == second.path {
		t.Fatalf("concurrent tasks share session file %s", first.path)
	}
	for _, task := range []*activeTask{first, second} {
		saved, err := loadSession(task.path)
		if err != nil {
			t.Fatal(err)
		}
		if saved.ID != task.session.ID || len(saved.History) != 1 {
			t.Fatalf("saved task = %#v", saved)
		}
	}
	paths := projectSessionPaths(root)
	current, err := loadCurrentSessionID(paths)
	if err != nil {
		t.Fatal(err)
	}
	if current != second.session.ID {
		t.Fatalf("current session = %s, want %s", current, second.session.ID)
	}
}

func TestLastRejectsSessionThatIsAlreadyRunning(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	active, err := startSession(taskConfig{Model: "luna", Effort: "m"}, options{persist: true})
	if err != nil {
		t.Fatal(err)
	}
	defer active.close()
	if err := appendUserPrompt(active.session, "running task"); err != nil {
		t.Fatal(err)
	}
	if err := active.saveInitial(); err != nil {
		t.Fatal(err)
	}
	if _, err := startSession(taskConfig{Model: "luna", Effort: "m"}, options{last: true}); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("concurrent --last error = %v", err)
	}
}

func TestLastRequiresSavedTaskInCurrentProject(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"continue", "--last"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "no saved task in this project") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".mai")); !os.IsNotExist(err) {
		t.Fatalf("failed resume created .mai: %v", err)
	}
}

func TestMainHelpDocumentsPersistenceOptions(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--unknown", "--help"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	for _, text := range []string{"--model", "--effort", "--persist", "--last", "--timeout", "--no-input", "Documentation and support"} {
		if !strings.Contains(stdout.String(), text) {
			t.Fatalf("help is missing %q:\n%s", text, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "--save-defaults") {
		t.Fatalf("help still contains removed global settings option:\n%s", stdout.String())
	}
}
