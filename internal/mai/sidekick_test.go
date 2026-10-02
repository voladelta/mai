package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSidekickDirectorFollowupAndPersistence(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	writeTestDeepSeekConfig(t)
	proTurns, flashTurns := 0, 0
	workerID := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
			Input []json.RawMessage `json:"input"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Instructions string `json:"instructions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		hasSidekick := false
		for _, tool := range body.Tools {
			hasSidekick = hasSidekick || tool.Name == "sidekick"
		}
		if body.Model == "deepseek-flash" {
			flashTurns++
			input := string(mustJSON(t, body.Input))
			if body.Reasoning.Effort != "high" || hasSidekick || strings.Contains(input, "parent-only-secret") || !strings.Contains(input, "explicit-context") || !strings.Contains(body.Instructions, "Do not delegate") {
				t.Errorf("worker effort, context or tool isolation failed: %+v", body)
			}
			switch flashTurns {
			case 1:
				call := functionCall{Type: "function_call", CallID: "worker-bash", Name: "bash", Arguments: `{"command":"printf observed-fact"}`}
				deepseekTestResponse(w, string(mustJSON(t, []functionCall{call})))
			case 2:
				deepseekTestResponse(w, `[{"type":"reasoning","content":[{"type":"reasoning_text","text":"worker-private-reasoning"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"observed-fact verified"}]}]`)
			case 3:
				if !strings.Contains(input, "observed-fact verified") || !strings.Contains(input, "Apply the fix") {
					t.Error("follow-up lost the worker conversation")
				}
				args := string(mustJSON(t, map[string]string{"patch": "*** Begin Patch\n*** Add File: worker.txt\n+fixed\n*** End Patch"}))
				deepseekTestResponse(w, string(mustJSON(t, []functionCall{{Type: "function_call", CallID: "worker-patch", Name: "apply_patch", Arguments: args}})))
			case 4:
				deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"worker.txt fixed"}]}]`)
			default:
				t.Errorf("unexpected worker turn %d", flashTurns)
			}
			return
		}

		proTurns++
		if body.Model != "deepseek-v4-pro" || body.Reasoning.Effort != "max" || !hasSidekick {
			t.Errorf("director configuration lost: %+v", body)
		}
		if proTurns == 1 {
			args := string(mustJSON(t, map[string]string{"task": "Inspect the issue", "context": "explicit-context"}))
			deepseekTestResponse(w, string(mustJSON(t, []functionCall{{Type: "function_call", CallID: "director-first", Name: "sidekick", Arguments: args}})))
			return
		}
		var output struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(body.Input[len(body.Input)-1], &output); err != nil {
			t.Error(err)
			return
		}
		var result sidekickResult
		if err := json.Unmarshal([]byte(output.Output), &result); err != nil {
			t.Error(err)
			return
		}
		if !result.OK || result.Status != "completed" || result.Model != "ds-flash" || result.Effort != "high" || strings.Contains(string(mustJSON(t, body.Input)), "worker-private-reasoning") {
			t.Errorf("invalid worker result: %+v", result)
		}
		if proTurns == 2 {
			workerID = result.WorkerID
			if !validSessionID(workerID) || result.Answer != "observed-fact verified" || result.Turns != 2 || result.UsageReports != 2 || result.Usage.InputTokens == nil || *result.Usage.InputTokens != 200 {
				t.Errorf("first result = %+v", result)
			}
			args := string(mustJSON(t, map[string]string{"task": "Apply the fix", "worker_id": workerID}))
			deepseekTestResponse(w, string(mustJSON(t, []functionCall{{Type: "function_call", CallID: "director-followup", Name: "sidekick", Arguments: args}})))
			return
		}
		if result.WorkerID != workerID || result.Answer != "worker.txt fixed" || result.Turns != 4 || result.UsageReports != 4 || result.Usage.InputTokens == nil || *result.Usage.InputTokens != 400 {
			t.Errorf("follow-up result = %+v", result)
		}
		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"director finished"}]}]`)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"parent-only-secret", "--max", "--persist", "--jsonl", "-s"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr.String())
	}
	if proTurns != 3 || flashTurns != 4 {
		t.Fatalf("requests: Pro=%d Flash=%d", proTurns, flashTurns)
	}
	data, err := os.ReadFile(filepath.Join(root, "worker.txt"))
	if err != nil || string(data) != "fixed\n" {
		t.Fatalf("worker patch = %q, %v", data, err)
	}

	decoder := json.NewDecoder(&stdout)
	workerDeltas, directorDeltas := 0, 0
	for decoder.More() {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event["type"] == "model.delta" {
			if event["worker_id"] == workerID {
				workerDeltas++
			} else {
				directorDeltas++
			}
		}
	}
	if workerDeltas != 2 || directorDeltas != 1 {
		t.Fatalf("JSONL deltas: worker=%d director=%d", workerDeltas, directorDeltas)
	}
	paths := projectSessionPaths(root)
	id, err := loadCurrentSessionID(paths)
	if err != nil || id == workerID {
		t.Fatalf("worker replaced current task: %s, %v", id, err)
	}
	saved, err := loadSession(sessionPath(paths, id))
	if err != nil || !bytes.Contains(mustJSON(t, saved.History), []byte("worker.txt fixed")) {
		t.Fatalf("director result was not saved: %v", err)
	}
	if _, err := os.Stat(sessionPath(paths, workerID)); !os.IsNotExist(err) {
		t.Fatalf("worker created a persisted session: %v", err)
	}
}

func TestSidekickInheritsApproval(t *testing.T) {
	for _, allowed := range []bool{false, true} {
		t.Run(fmt.Sprint(allowed), func(t *testing.T) {
			parent := deepseekTestSession(t)
			parent.Model = "ds-pro"
			victim := filepath.Join(t.TempDir(), "victim")
			if err := os.WriteFile(victim, []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
			requests, approvals := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if requests == 1 {
					args := string(mustJSON(t, map[string]string{"command": fmt.Sprintf("rm -- %q", victim)}))
					deepseekTestResponse(w, string(mustJSON(t, []functionCall{{Type: "function_call", CallID: "rm", Name: "bash", Arguments: args}})))
					return
				}
				deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`)
			}))
			defer server.Close()
			a := newAgent(io.Discard, io.Discard, "", time.Second, false)
			a.skipSkills = true
			a.backend = &deepseekClient{httpClient: server.Client(), endpoint: server.URL, stdout: io.Discard}
			a.approve = func(_ context.Context, command, reason string) (bool, error) {
				approvals++
				if !strings.Contains(command, victim) || reason == "" {
					t.Error("approval lost command context")
				}
				return allowed, nil
			}
			defer a.close()

			output := a.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: `{"task":"Remove the specified test file"}`})
			if !strings.Contains(string(output), "completed") || approvals != 1 {
				t.Fatalf("approval calls=%d, result=%s", approvals, output)
			}
			_, err := os.Stat(victim)
			if allowed != os.IsNotExist(err) {
				t.Fatalf("approval=%v, victim state=%v", allowed, err)
			}
		})
	}
}

func TestSidekickFailureDoesNotReplayEffects(t *testing.T) {
	parent := deepseekTestSession(t)
	parent.Model = "ds-pro"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			args := string(mustJSON(t, map[string]string{"patch": "*** Begin Patch\n*** Add File: partial.txt\n+partial\n*** End Patch"}))
			deepseekTestResponse(w, string(mustJSON(t, []functionCall{{Type: "function_call", CallID: "partial", Name: "apply_patch", Arguments: args}})))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	a := newAgent(io.Discard, io.Discard, "", time.Second, false)
	a.skipSkills = true
	a.backend = &deepseekClient{httpClient: server.Client(), endpoint: server.URL, stdout: io.Discard}
	defer a.close()

	var encoded string
	if err := json.Unmarshal(a.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: `{"task":"Apply the fix"}`}), &encoded); err != nil {
		t.Fatal(err)
	}
	var result sidekickResult
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Status != "failed" || result.Instruction == "" || requests != 2 {
		t.Fatalf("failure result = %+v, requests=%d", result, requests)
	}
	if _, err := os.Stat(filepath.Join(parent.CWD, "partial.txt")); err != nil {
		t.Fatalf("partial effect missing: %v", err)
	}

	args := string(mustJSON(t, map[string]string{"task": "Retry", "worker_id": result.WorkerID}))
	output := a.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: args})
	if !strings.Contains(string(output), "worker failed") || requests != 2 {
		t.Fatalf("failed worker replayed: %s, requests=%d", output, requests)
	}
	fresh := newAgent(io.Discard, io.Discard, "", time.Second, false)
	if output := fresh.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: args}); !strings.Contains(string(output), "expired") {
		t.Fatalf("restart reused expired worker: %s", output)
	}
}

func TestSidekickBudgetAndCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRun), func(t *testing.T) {
			parent := deepseekTestSession(t)
			parent.Model = "ds-pro"
			requests := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if cancelRun {
					cancel()
					return
				}
				call := functionCall{Type: "function_call", CallID: fmt.Sprintf("call-%d", requests), Name: "edit_context", Arguments: `{"action":"inspect"}`}
				deepseekTestResponse(w, string(mustJSON(t, []functionCall{call})))
			}))
			defer server.Close()
			a := newAgent(io.Discard, io.Discard, "", time.Second, false)
			a.skipSkills = true
			a.backend = &deepseekClient{httpClient: server.Client(), endpoint: server.URL, stdout: io.Discard}
			defer a.close()

			output := a.executeTool(ctx, parent, functionCall{Name: "sidekick", Arguments: `{"task":"Investigate"}`})
			wantRequests := maxSidekickTurns
			wantError := "stopped after 32"
			if cancelRun {
				wantRequests, wantError = 1, "context canceled"
			}
			if requests != wantRequests || !strings.Contains(string(output), wantError) || !strings.Contains(string(output), "Inspect effects") {
				t.Fatalf("requests=%d, result=%s", requests, output)
			}
		})
	}
}

func TestSidekickPythonNamespaceSurvivesFollowupAndCloses(t *testing.T) {
	a, parent := pythonTestAgent(t)
	parent.Model, parent.Effort, parent.RepoRoot = "ds-pro", "h", parent.CWD
	a.skipSkills = true
	t.Cleanup(a.close)
	if result := pythonCell(t, a, parent, "parent_only = 7"); !result.OK {
		t.Fatal(result.Error)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 || requests == 3 {
			code := "assert 'parent_only' not in globals()\nvalue = 42\nvalue"
			if requests == 3 {
				code = "value += 1\nvalue"
			}
			args := string(mustJSON(t, map[string]string{"code": code}))
			deepseekTestResponse(w, string(mustJSON(t, []functionCall{{Type: "function_call", CallID: fmt.Sprintf("python-%d", requests), Name: "python", Arguments: args}})))
			return
		}
		var body struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		var output struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(body.Input[len(body.Input)-1], &output); err != nil {
			t.Error(err)
			return
		}
		var result pythonResult
		if err := json.Unmarshal([]byte(output.Output), &result); err != nil {
			t.Error(err)
			return
		}
		want := "42\n"
		if requests == 4 {
			want = "43\n"
		}
		if !result.OK || result.Stdout != want || (requests == 4 && result.Fresh) {
			t.Errorf("worker Python state = %+v, want %q", result, want)
		}
		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"verified"}]}]`)
	}))
	defer server.Close()
	a.backend = &deepseekClient{httpClient: server.Client(), endpoint: server.URL, stdout: io.Discard}

	for turn := 0; turn < 2; turn++ {
		args := map[string]string{"task": "Check the worker namespace"}
		if turn == 1 {
			args["worker_id"] = a.sidekick.session.ID
		}
		output := a.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: string(mustJSON(t, args))})
		if !strings.Contains(string(output), "completed") {
			t.Fatalf("assignment %d: %s", turn, output)
		}
	}
	if result := pythonCell(t, a, parent, "assert 'value' not in globals()\nparent_only"); !result.OK || result.Stdout != "7\n" {
		t.Fatalf("worker changed parent namespace: %+v", result)
	}
	if a.sidekick.agent.python.cmd == nil {
		t.Fatal("worker namespace did not remain alive")
	}

	a.close()
	if a.python.cmd != nil || a.sidekick.agent.python.cmd != nil {
		t.Fatal("parent shutdown left Python processes alive")
	}
}

func TestSidekickReturnsTerminalMessagesAfterFollowup(t *testing.T) {
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compaction=%v", compact), func(t *testing.T) {
			parent := deepseekTestSession(t)
			parent.Model = "ds-pro"
			turns, checkpoints := 0, 0
			previousAnswer := strings.Repeat("previous assignment ", 1024)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					ToolChoice string `json:"tool_choice"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}

				if body.ToolChoice == "none" {
					checkpoints++
					deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Previous assignment completed."}]}]`)
					return
				}

				turns++
				switch turns {
				case 1:
					message := map[string]any{
						"type": "message", "role": "assistant",
						"content": []map[string]string{{"type": "output_text", "text": previousAnswer}},
					}
					deepseekTestResponse(w, string(mustJSON(t, []any{message})))
				case 2:
					deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"intermediate answer"}]},{"type":"function_call","call_id":"inspect","name":"edit_context","arguments":"{\"action\":\"inspect\"}"}]`)
				case 3:
					deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"First final message."}]},{"type":"reasoning","content":[{"type":"reasoning_text","text":"hidden reasoning"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Second final message."}]}]`)
				default:
					t.Errorf("unexpected model turn %d", turns)
				}
			}))
			defer server.Close()

			a := newAgent(io.Discard, io.Discard, "", time.Second, false)
			a.skipSkills = true
			a.backend = &deepseekClient{httpClient: server.Client(), endpoint: server.URL, stdout: io.Discard}
			defer a.close()

			output := a.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: `{"task":"First assignment"}`})
			if !strings.Contains(string(output), "previous assignment") {
				t.Fatalf("first assignment failed: %s", output)
			}

			if compact {
				a.sidekick.session.ContextTokens = modelContextWindow
			}
			args := string(mustJSON(t, map[string]string{"task": "Follow-up assignment", "worker_id": a.sidekick.session.ID}))
			output = a.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: args})
			var encoded string
			if err := json.Unmarshal(output, &encoded); err != nil {
				t.Fatal(err)
			}
			var result sidekickResult
			if err := json.Unmarshal([]byte(encoded), &result); err != nil {
				t.Fatal(err)
			}

			if !result.OK || result.Answer != "First final message.\nSecond final message." {
				t.Fatalf("follow-up returned other response content: %+v", result)
			}
			if turns != 3 || (checkpoints > 0) != compact {
				t.Fatalf("turns=%d checkpoints=%d, compaction=%v", turns, checkpoints, compact)
			}
		})
	}
}

func TestSidekickCannotReuseStaleAnswer(t *testing.T) {
	parent := deepseekTestSession(t)
	parent.Model = "ds-pro"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"earlier answer"}]}]`)
			return
		}
		deepseekTestResponse(w, `[{"type":"reasoning","content":[{"type":"reasoning_text","text":"no answer"}]}]`)
	}))
	defer server.Close()
	a := newAgent(io.Discard, io.Discard, "", time.Second, false)
	a.skipSkills = true
	a.backend = &deepseekClient{httpClient: server.Client(), endpoint: server.URL, stdout: io.Discard}
	defer a.close()
	if output := a.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: `{"task":"First assignment"}`}); !strings.Contains(string(output), "earlier answer") {
		t.Fatalf("first assignment = %s", output)
	}

	args := string(mustJSON(t, map[string]string{"task": "Second assignment", "worker_id": a.sidekick.session.ID}))
	output := a.executeTool(context.Background(), parent, functionCall{Name: "sidekick", Arguments: args})
	if !strings.Contains(string(output), "without an assistant answer") || strings.Contains(string(output), "earlier answer") {
		t.Fatalf("stale answer admitted: %s", output)
	}
}
