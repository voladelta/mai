package mai

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if !strings.Contains(stdout.String(), "Built-in default: astra/low.") {
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
	if code := Main([]string{"hello", "--persist", "-e", "h"}, &stdout, &stderr); code != 1 {
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
	if sess.ID != id || sess.Model != "astra" || sess.Effort != "h" || len(sess.History) != 1 {
		t.Fatalf("saved session = %#v", sess)
	}
}

func TestConcurrentPersistedTasksKeepSeparateHistory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	cfg := taskConfig{Effort: "m"}

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
	active, err := startSession(taskConfig{Effort: "m"}, options{persist: true})
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
	if _, err := startSession(taskConfig{Effort: "m"}, options{last: true}); err == nil || !strings.Contains(err.Error(), "already running") {
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
	for _, text := range []string{"--effort", "--persist", "--last", "--timeout", "--no-input", "--subagent", "Documentation and support"} {
		if !strings.Contains(stdout.String(), text) {
			t.Fatalf("help is missing %q:\n%s", text, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "--save-defaults") {
		t.Fatalf("help still contains removed global settings option:\n%s", stdout.String())
	}
}

func TestAstraResumePreservesRequestPrefix(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTestCodexAuth(t)
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, body)
		writeSSEItem(t, w, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`, 100)
	}))
	defer server.Close()
	t.Setenv("MAI_CODEX_URL", server.URL)
	for _, args := range [][]string{
		{"first", "--persist"},
		{"second", "--last", "-e", "h"},
		{"third", "--last"},
		{"fourth", "--last", "-e", "l"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Main(args, &stdout, &stderr); code != 0 {
			t.Fatalf("Main(%v) = %d: %s", args, code, stderr.String())
		}
	}
	for i, request := range requests {
		wantModel, wantEffort := "gpt-6-astra", "low"
		if request["model"] != wantModel || request["reasoning"].(map[string]any)["effort"] != wantEffort {
			t.Fatalf("request %d model/effort = %v / %v", i, request["model"], request["reasoning"])
		}
		if request["prompt_cache_key"] != requests[0]["prompt_cache_key"] {
			t.Fatalf("request %d changed cache key", i)
		}
		input := request["input"].([]any)
		var updates []string
		for index, raw := range input {
			item := raw.(map[string]any)
			if item["type"] == "configuration_update" {
				updates = append(updates, item["reasoning"].(map[string]any)["effort"].(string))
				if index+1 >= len(input) || input[index+1].(map[string]any)["role"] != "user" {
					t.Fatalf("update does not precede user: %v", input)
				}
			}
		}
		want := []string{"", "high", "high", "high,low"}[i]
		if strings.Join(updates, ",") != want {
			t.Fatalf("request %d updates = %v, want %s", i, updates, want)
		}
		if i > 0 && i < 4 {
			prior := requests[i-1]["input"].([]any)
			if !bytes.Equal(mustJSON(t, input[:len(prior)]), mustJSON(t, prior)) {
				t.Fatalf("request %d rewrote history prefix", i)
			}
		}
	}
}

func TestLastMigratesOlderModelToAstra(t *testing.T) {
	t.Chdir(t.TempDir())
	active, err := startSession(taskConfig{Effort: "m"}, options{persist: true})
	if err != nil {
		t.Fatal(err)
	}
	active.session.Model = "luna"
	if err := appendUserPrompt(active.session, "original"); err != nil {
		t.Fatal(err)
	}
	if err := active.saveInitial(); err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), active.session.History[0]...)
	active.close()
	resumed, err := startSession(configForTask(options{}), options{last: true, effortExplicit: true, effort: "h"})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.close()
	if err := appendUserPrompt(resumed.session, "continue"); err != nil {
		t.Fatal(err)
	}
	if err := resumed.saveInitial(); err != nil {
		t.Fatal(err)
	}
	saved, err := loadSession(resumed.path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Model != "astra" || saved.Effort != "h" || saved.RequestEffort != "h" || len(saved.History) != 2 || compactJSON(saved.History[0]) != compactJSON(original) {
		t.Fatalf("migrated session = %#v", saved)
	}
}
