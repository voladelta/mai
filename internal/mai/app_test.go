package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failToolCompletedWriter struct {
	events bytes.Buffer
}

func (w *failToolCompletedWriter) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte(`"type":"tool.completed"`)) {
		return 0, errors.New("tool completion event failed")
	}
	return w.events.Write(p)
}

func TestMainWithoutPromptShowsBuiltInDefault(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Main(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Built-in default: ds-pro/high.") {
		t.Fatalf("stdout does not show the built-in default:\n%s", stdout.String())
	}
}

func TestMainEnforcesMaxTurns(t *testing.T) {
	for _, test := range []struct {
		name      string
		args      []string
		finalTurn int
		wantTurns int
		wantCode  int
	}{
		{name: "default stops at 64", args: []string{"work"}, wantTurns: 64, wantCode: 1},
		{name: "custom limit", args: []string{"work", "--max-turns=2"}, wantTurns: 2, wantCode: 1},
		{name: "completion at limit", args: []string{"work", "--max-turns=2"}, finalTurn: 2, wantTurns: 2},
		{name: "more than 64", args: []string{"work", "--max-turns=65"}, finalTurn: 65, wantTurns: 65},
		{name: "unlimited completes beyond default", args: []string{"work", "--max-turns=-1"}, finalTurn: 70, wantTurns: 70},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeTestDeepSeekConfig(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				if requests == test.finalTurn {
					deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`)
					return
				}

				call := functionCall{
					Type:      "function_call",
					CallID:    fmt.Sprintf("call-%d", requests),
					Name:      "edit_context",
					Arguments: `{"action":"inspect"}`,
				}
				deepseekTestResponse(w, string(mustJSON(t, []functionCall{call})))
			}))
			defer server.Close()
			t.Setenv("MAI_DEEPSEEK_URL", server.URL)

			var stdout, stderr bytes.Buffer
			if code := Main(test.args, &stdout, &stderr); code != test.wantCode {
				t.Fatalf("exit code = %d, want %d; stderr = %s", code, test.wantCode, stderr.String())
			}

			if requests != test.wantTurns {
				t.Fatalf("model requests = %d, want %d", requests, test.wantTurns)
			}

			if test.wantCode != 0 && !strings.Contains(stderr.String(), fmt.Sprintf("agent stopped after %d model turns", test.wantTurns)) {
				t.Fatalf("stderr = %s", stderr.String())
			}
		})
	}
}

func TestMaxTurnsKeepsCompletedToolResultForResume(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTestDeepSeekConfig(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			deepseekTestResponse(w, `[{"type":"function_call","call_id":"saved-call","name":"bash","arguments":"{\"command\":\"printf saved-result\"}"}]`)
			return
		}

		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}

		if len(body.Input) != 4 || !bytes.Contains(body.Input[2], []byte("saved-result")) || !bytes.Contains(body.Input[2], []byte("saved-call")) {
			t.Errorf("resume input lost completed tool result: %s", mustJSON(t, body.Input))
		}

		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"work", "--persist", "--max-turns=1"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "stopped after 1 model turns") {
		t.Fatalf("limited run: exit code = %d, stderr = %s", code, stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Main([]string{"continue", "--last", "--max-turns=2"}, &stdout, &stderr); code != 0 {
		t.Fatalf("resume: exit code = %d, stderr = %s", code, stderr.String())
	}

	if requests != 2 || stdout.String() != "done\n" {
		t.Fatalf("resume requests = %d, stdout = %q", requests, stdout.String())
	}
}

func TestMainJSONLProducesOnlyEventsOnStdout(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTestDeepSeekConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"hello"}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"total_tokens":7}}}`)
		fmt.Fprintln(w)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"hello", "--jsonl"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code=%d stderr=%s", code, stderr.String())
	}
	var types []string
	var modelTimed, taskTimed bool
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var event struct {
			Type       string `json:"type"`
			DurationMS *int64 `json:"duration_ms"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("non-JSONL stdout %q: %v", line, err)
		}
		types = append(types, event.Type)
		if event.Type == "model.completed" {
			modelTimed = event.DurationMS != nil
		}
		if event.Type == "task.completed" {
			taskTimed = event.DurationMS != nil
		}
	}
	if strings.Join(types, ",") != "task.started,model.started,model.delta,model.completed,task.completed" {
		t.Fatalf("event types=%v", types)
	}
	if !modelTimed || !taskTimed {
		t.Fatalf("missing elapsed times: model=%t task=%t", modelTimed, taskTimed)
	}
}

func TestMainJSONLReportsFailedModelDuration(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTestDeepSeekConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"hello", "--jsonl"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code=%d stderr=%s", code, stderr.String())
	}

	var types []string
	var failedTimed bool
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var event struct {
			Type       string `json:"type"`
			DurationMS *int64 `json:"duration_ms"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("non-JSONL stdout %q: %v", line, err)
		}

		types = append(types, event.Type)
		if event.Type == "model.failed" {
			failedTimed = event.DurationMS != nil
		}
	}
	if strings.Join(types, ",") != "task.started,model.started,model.failed,error" || !failedTimed {
		t.Fatalf("events=%v failed_timed=%t", types, failedTimed)
	}
}

func TestMainPrintsCompletedTextWithoutDeltas(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTestDeepSeekConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `data: {"type":"response.completed","response":{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}]}}`)
		fmt.Fprintln(w)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)

	var ordinary, ordinaryErr bytes.Buffer
	if code := Main([]string{"hello"}, &ordinary, &ordinaryErr); code != 0 || ordinary.String() != "hello\n" {
		t.Fatalf("default code=%d stdout=%q stderr=%q", code, ordinary.String(), ordinaryErr.String())
	}

	var output, stderr bytes.Buffer
	if code := Main([]string{"hello", "--jsonl"}, &output, &stderr); code != 0 {
		t.Fatalf("jsonl code=%d stderr=%q", code, stderr.String())
	}
	var texts []string
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var event struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("invalid JSONL line %q: %v", line, err)
		}
		if event.Type == "model.delta" {
			texts = append(texts, event.Text)
		}
	}
	if strings.Join(texts, "") != "hello" || len(texts) != 1 {
		t.Fatalf("model text events=%v, stream=%s", texts, output.String())
	}
}

func TestMainJSONLReportsToolCalls(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTestDeepSeekConfig(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			fmt.Fprintln(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call-1","name":"bash","arguments":"{\"command\":\"printf ok\"}"}}`)
			fmt.Fprintln(w)
		} else {
			fmt.Fprintln(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}}`)
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, `data: {"type":"response.completed","response":{"status":"completed"}}`)
		fmt.Fprintln(w)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"run tool", "--jsonl"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code=%d stderr=%s", code, stderr.String())
	}
	var started, completed bool
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("non-JSONL stdout %q: %v", line, err)
		}
		if event["type"] == "tool.started" {
			started = event["name"] == "bash" && event["call_id"] == "call-1"
		}
		if event["type"] == "tool.completed" {
			_, timed := event["duration_ms"].(float64)
			completed = event["name"] == "bash" && event["call_id"] == "call-1" && event["output"] != nil && timed
		}
	}
	if requests != 2 || !started || !completed {
		t.Fatalf("requests=%d started=%t completed=%t events=%s", requests, started, completed, stdout.String())
	}
}

func TestToolCompletedEventFailureKeepsSavedResult(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.json")
	call := functionCall{
		Name:      "bash",
		CallID:    "call-1",
		Arguments: `{"command":"printf complete > effect.txt"}`,
	}
	sess := &session{
		Version:  stateVersion,
		ID:       "01234567-89ab-cdef-0123-456789abcdef",
		CWD:      root,
		RepoRoot: root,
		Model:    "ds-flash",
		Effort:   "h",
		History: []json.RawMessage{
			json.RawMessage(`{"type":"function_call","call_id":"call-1","name":"bash","arguments":"{\"command\":\"printf complete > effect.txt\"}"}`),
		},
	}
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}

	events := &failToolCompletedWriter{}
	a := &agent{stderr: io.Discard, sessionPath: path, events: events}
	err := a.executeCalls(context.Background(), sess, []functionCall{call})
	if err == nil || !strings.Contains(err.Error(), "tool completion event failed") {
		t.Fatalf("executeCalls error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "effect.txt")); err != nil {
		t.Fatalf("tool effect was not completed: %v", err)
	}
	if !strings.Contains(events.events.String(), `"type":"tool.started"`) || strings.Contains(events.events.String(), `"type":"tool.completed"`) {
		t.Fatalf("unexpected events: %s", events.events.String())
	}

	saved, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := repairInterruptedToolCalls(saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.History) != 2 {
		t.Fatalf("history after resume has %d items, want call and saved result: %#v", len(saved.History), saved.History)
	}
	var output struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(saved.History[1], &output); err != nil {
		t.Fatal(err)
	}
	var result bashResult
	if err := json.Unmarshal([]byte(output.Output), &result); err != nil {
		t.Fatal(err)
	}
	if output.Type != "function_call_output" || output.CallID != call.CallID || !result.OK || strings.Contains(output.Output, `"outcome":"unknown"`) {
		t.Fatalf("saved tool output after resume = %#v", output)
	}
}

func TestStatelessTaskDoesNotCreateMaiDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	writeTestDeepSeekFailureServer(t)
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
	writeTestDeepSeekFailureServer(t)
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"hello", "--persist"}, &stdout, &stderr); code != 1 {
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
	if sess.ID != id || sess.Model != "ds-pro" || sess.Effort != "h" || len(sess.History) != 1 {
		t.Fatalf("saved session = %#v", sess)
	}
}

func TestConcurrentPersistedTasksKeepSeparateHistory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	cfg := taskConfig{Model: defaultModel, Effort: "h"}

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
	active, err := startSession(taskConfig{Model: defaultModel, Effort: "h"}, options{persist: true})
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
	if _, err := startSession(taskConfig{Effort: "h"}, options{last: true}); err == nil || !strings.Contains(err.Error(), "already running") {
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
	for _, text := range []string{"--f", "--max", "--model", "--persist", "--last", "--max-turns", "--timeout", "--cell-timeout", "--no-input", "Documentation and support"} {
		if !strings.Contains(stdout.String(), text) {
			t.Fatalf("help is missing %q:\n%s", text, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "--save-defaults") || strings.Contains(stdout.String(), "--effort") {
		t.Fatalf("help still contains removed global settings option:\n%s", stdout.String())
	}
}

func TestResumePreservesHistoryAndUsesCurrentEffort(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTestDeepSeekConfig(t)
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
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)
	for _, args := range [][]string{
		{"first", "--persist"},
		{"second", "--last", "--max"},
		{"third", "--last"},
		{"fourth", "--last", "--f"},
		{"fifth", "--last"},
		{"sixth", "--last", "-m", "ds-pro"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Main(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: %s", args, stderr.String())
		}
	}
	for i, request := range requests {
		wantEffort := []string{"high", "max", "max", "high", "high", "high"}[i]
		wantModel := []string{"deepseek-v4-pro", "deepseek-v4-pro", "deepseek-v4-pro", "deepseek-flash", "deepseek-flash", "deepseek-v4-pro"}[i]
		if request["model"] != wantModel || request["reasoning"].(map[string]any)["effort"] != wantEffort {
			t.Fatalf("model/effort %v", request)
		}
		if _, exists := request["prompt_cache_key"]; exists {
			t.Fatal("unsupported cache key")
		}
		input := request["input"].([]any)
		for _, raw := range input {
			if raw.(map[string]any)["type"] == "configuration_update" {
				t.Fatal("unsupported configuration item")
			}
		}
		if i > 0 {
			prior := requests[i-1]["input"].([]any)
			if !bytes.Equal(mustJSON(t, input[:len(prior)]), mustJSON(t, prior)) {
				t.Fatal("history prefix changed")
			}
		}
	}
}

func TestLastRejectsRemovedSubscriptionModel(t *testing.T) {
	t.Chdir(t.TempDir())
	active, err := startSession(taskConfig{Model: defaultModel, Effort: "h"}, options{persist: true})
	if err != nil {
		t.Fatal(err)
	}
	active.session.Model = "luna"
	if err := active.saveInitial(); err != nil {
		t.Fatal(err)
	}
	active.close()
	if _, err := startSession(configForTask(options{}), options{last: true}); err == nil {
		t.Fatal("removed model silently migrated")
	}
}

func TestLastPreservesLegacyLowEffort(t *testing.T) {
	t.Chdir(t.TempDir())
	active, err := startSession(taskConfig{Model: "ds-flash", Effort: "l"}, options{persist: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := active.saveInitial(); err != nil {
		active.close()
		t.Fatal(err)
	}
	active.close()

	opts, err := parseOptions([]string{"continue", "--last"})
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := startSession(configForTask(opts), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.close()
	if resumed.session.Model != "ds-flash" || resumed.session.Effort != "l" {
		t.Fatalf("legacy settings overwritten: %#v", resumed.session)
	}
}
