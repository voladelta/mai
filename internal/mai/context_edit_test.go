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

func contextEditFixture(t *testing.T) *session {
	t.Helper()
	dir := t.TempDir()
	call, _ := json.Marshal(functionCall{Type: "function_call", CallID: "logs", Name: "bash", Arguments: `{"command":"printf logs"}`})
	output, _ := json.Marshal(map[string]any{
		"type": "function_call_output", "call_id": "logs", "future_envelope": "keep envelope",
		"output": string(mustJSONValue(t, map[string]any{
			"ok": true, "exit_code": 0, "timed_out": false,
			"stdout": "ORIGINAL-RECALL-FACT " + strings.Repeat("completed build log\n", 1000),
			"stderr": "warning to keep", "stdout_capture_path": "/private/example-capture",
			"future_output": map[string]any{"enabled": false, "count": 0},
		})),
	})
	sess := &session{
		Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef",
		CWD: dir, RepoRoot: dir, Model: "flash", Effort: "h",
		History: []json.RawMessage{
			json.RawMessage(`{"role":"user","content":"Preserve user requirements"}`),
			json.RawMessage(`{"type":"reasoning","content":[{"type":"reasoning_text","text":"private reasoning"}]}`),
			call, output,
		},
	}
	sess.ContextTokens = estimateHistoryTokens(sess.History)
	return sess
}

func mustJSONValue(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestContextHintsPreserveRequestPrefixAndAllowDirectShrink(t *testing.T) {
	sess := contextEditFixture(t)
	original := mustJSONValue(t, sess.History)
	var requests []struct {
		Instructions string            `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Instructions string            `json:"instructions"`
			Input        []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		requests = append(requests, request)
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer server.Close()
	client := &responsesClient{
		profile:    profileDeepSeek,
		models:     defaultProviderConfig().Models,
		httpClient: server.Client(),
		stdout:     io.Discard,
		endpoint:   server.URL,
	}

	for _, pressure := range []int64{0, modelContextWindow / 2} {
		sess.ContextTokens = pressure
		if _, err := client.request(context.Background(), sess, "Stable instructions", true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.request(context.Background(), sess, "Stable instructions", false); err != nil {
		t.Fatal(err)
	}

	for i, request := range requests {
		if request.Instructions != "Stable instructions" || !bytes.Equal(mustJSONValue(t, request.Input[:len(sess.History)]), original) {
			t.Fatalf("request %d changed the stable prefix", i)
		}
	}
	if len(requests[0].Input) != len(sess.History) || len(requests[2].Input) != len(sess.History) || len(requests[1].Input) != len(sess.History)+1 {
		t.Fatal("hints must appear only on ordinary requests under pressure")
	}
	hint := requests[1].Input[len(sess.History)]
	if !bytes.Contains(hint, []byte(contextDigest(sess.History[3]))) || !bytes.Contains(hint, []byte(`\"call_id\":\"logs\"`)) {
		t.Fatalf("missing fresh edit handle: %s", hint)
	}

	receipt := shrinkContext(t, &agent{}, sess, contextDigest(sess.History[3]), "Keep ORIGINAL-RECALL-FACT.")
	if len(sess.ContextEdits) != 1 || !bytes.Equal(mustJSONValue(t, sess.History), original) {
		t.Fatalf("direct shrink failed or mutated the source: %s", receipt)
	}
	projected, err := sess.requestHistory()
	if err != nil {
		t.Fatal(err)
	}
	if contextEditHint(projected) != nil {
		t.Fatal("small shortened output still produced a hint")
	}
}

func TestContextHintsBoundRecentHandlesAndExcludeFailedOutput(t *testing.T) {
	history := []json.RawMessage{}
	for i := 0; i < 20; i++ {
		callID := fmt.Sprintf("large-%02d", i)
		history = append(history, mustJSONValue(t, functionCall{Type: "function_call", CallID: callID, Name: "bash", Arguments: `{}`}))
		body := mustJSONValue(t, map[string]any{
			"ok": true, "exit_code": 0, "stdout": strings.Repeat("x", 16<<10),
		})
		history = append(history, mustJSONValue(t, map[string]any{"type": "function_call_output", "call_id": callID, "output": string(body)}))
	}
	hint := string(contextEditHint(history))
	if strings.Count(hint, `\"digest\":`) != 8 || !strings.Contains(hint, "large-19") || strings.Contains(hint, "large-11") {
		t.Fatalf("hints failed to bound the recent handles: %s", hint)
	}

	boundary := mustJSONValue(t, map[string]any{"role": "assistant", "content": "Previous turn finished."})
	history = append(append(append([]json.RawMessage(nil), history[:len(history)-2]...), boundary), history[len(history)-2:]...)
	hint = string(contextEditHint(history))
	if strings.Count(hint, `\"digest\":`) != 1 || !strings.Contains(hint, "large-19") || strings.Contains(hint, "large-18") {
		t.Fatalf("hints crossed the previous assistant answer: %s", hint)
	}

	history = history[len(history)-2:]
	var item map[string]any
	_ = json.Unmarshal(history[1], &item)
	item["output"] = string(mustJSONValue(t, map[string]any{
		"ok": false, "exit_code": 1, "stdout": strings.Repeat("x", 16<<10),
	}))
	history[1] = mustJSONValue(t, item)
	if contextEditHint(history) != nil {
		t.Fatal("failed output was advertised as editable")
	}

	if contextEditHint([]json.RawMessage{boundary}) != nil {
		t.Fatal("an assistant answer alone produced an edit hint")
	}
}

func shrinkContext(t *testing.T, a *agent, sess *session, digest, summary string) json.RawMessage {
	t.Helper()
	args := mustJSONValue(t, map[string]any{
		"action": "shrink", "edits": []stdoutEdit{{CallID: "logs", Digest: digest, Summary: summary}},
	})
	return a.executeTool(context.Background(), sess, functionCall{Name: "edit_context", Arguments: string(args)})
}

func TestContextEditPreservesOriginalsAndPersistsProjection(t *testing.T) {
	sess := contextEditFixture(t)
	original := mustJSONValue(t, sess.History)
	path := filepath.Join(sess.CWD, "saved", "session.json")
	a := &agent{sessionPath: path}
	inspected := a.executeContextEdit(sess, `{"action":"inspect"}`)
	var inspection string
	_ = json.Unmarshal(inspected, &inspection)
	if !strings.Contains(inspection, contextDigest(sess.History[3])) {
		t.Fatalf("inspect omitted source digest: %s", inspection)
	}
	receipt := shrinkContext(t, a, sess, contextDigest(sess.History[3]), "Build completed; release CODE-219.")
	if !bytes.Contains(receipt, []byte("accepted")) || len(sess.ContextEdits) != 1 {
		t.Fatalf("edit rejected: %s", receipt)
	}
	if !bytes.Equal(original, mustJSONValue(t, sess.History)) {
		t.Fatal("edit changed original history")
	}

	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.appendEstimatedHistory(json.RawMessage(`{"role":"user","content":"New appended instruction"}`))
	projected, err := loaded.requestHistory()
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{0, 1, 2, 4} {
		if !bytes.Equal(projected[index], loaded.History[index]) {
			t.Fatalf("uneditable item %d changed", index)
		}
	}
	if estimateHistoryTokens(projected) >= estimateHistoryTokens(loaded.History) {
		t.Fatal("projection did not reduce context")
	}
	var item struct {
		Output string `json:"output"`
		CallID string `json:"call_id"`
	}
	_ = json.Unmarshal(projected[3], &item)
	var body map[string]any
	_ = json.Unmarshal([]byte(item.Output), &body)
	if item.CallID != "logs" || body["stderr"] != "warning to keep" || body["stdout_capture_path"] != "/private/example-capture" || body["exit_code"] != float64(0) || body["stdout_context_summary"] != true {
		t.Fatalf("result metadata changed: %s", projected[3])
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(projected[3], &envelope); err != nil {
		t.Fatal(err)
	}
	if string(envelope["future_envelope"]) != `"keep envelope"` || !bytes.Equal(mustJSONValue(t, body["future_output"]), json.RawMessage(`{"count":0,"enabled":false}`)) {
		t.Fatalf("unknown metadata changed: %s", projected[3])
	}

	found := searchTranscript(loaded, json.RawMessage(`{"query":"ORIGINAL-RECALL-FACT","limit":20}`), "")
	if !bytes.Contains(found, []byte(`"total":1`)) {
		t.Fatalf("original stdout was not searchable: %s", found)
	}

	// An accepted edit consumes its old projected digest, even though raw
	// history did not change. Stale proposals cannot overwrite a newer summary.
	before := mustJSONValue(t, loaded)
	rejected := shrinkContext(t, a, loaded, contextDigest(loaded.History[3]), "Stale summary")
	if !bytes.Contains(rejected, []byte("stale")) || !bytes.Equal(before, mustJSONValue(t, loaded)) {
		t.Fatalf("stale edit mutated session: %s", rejected)
	}
}

func TestContextEditRejectsUnsafeOutputsAndAtomicBatchFailure(t *testing.T) {
	for _, output := range []string{
		`{"outcome":"unknown","error":"interrupted"}`,
		`{"ok":false,"exit_code":1,"stdout":"failed"}`,
		`{"ok":true,"exit_code":0,"timed_out":true,"stdout":"partial"}`,
	} {
		t.Run(output, func(t *testing.T) {
			sess := contextEditFixture(t)
			sess.History[3] = mustJSONValue(t, map[string]any{"type": "function_call_output", "call_id": "logs", "output": output})
			before := mustJSONValue(t, sess)
			got := shrinkContext(t, &agent{}, sess, contextDigest(sess.History[3]), "Done")
			if !bytes.Contains(got, []byte("error")) || !bytes.Equal(before, mustJSONValue(t, sess)) {
				t.Fatalf("unsafe edit accepted: %s", got)
			}
		})
	}

	t.Run("non-Bash tool", func(t *testing.T) {
		sess := contextEditFixture(t)
		sess.History[2] = mustJSONValue(t, functionCall{Type: "function_call", CallID: "logs", Name: "python", Arguments: "{}"})
		got := shrinkContext(t, &agent{}, sess, contextDigest(sess.History[3]), "Done")
		if !bytes.Contains(got, []byte("earlier Bash call")) || len(sess.ContextEdits) != 0 {
			t.Fatalf("other tool output accepted: %s", got)
		}
	})

	t.Run("typed image output", func(t *testing.T) {
		sess := contextEditFixture(t)
		sess.History[3] = mustJSONValue(t, map[string]any{"type": "function_call_output", "call_id": "logs", "output": []any{map[string]string{"type": "input_image", "image_url": "data:image/png;base64,example"}}})
		got := shrinkContext(t, &agent{}, sess, contextDigest(sess.History[3]), "Image summary")
		if !bytes.Contains(got, []byte("text tool output")) || len(sess.ContextEdits) != 0 {
			t.Fatalf("image output accepted: %s", got)
		}
	})

	sess := contextEditFixture(t)
	before := mustJSONValue(t, sess)
	args := mustJSONValue(t, map[string]any{
		"action": "shrink", "edits": []stdoutEdit{
			{CallID: "logs", Digest: contextDigest(sess.History[3]), Summary: "Build completed"},
			{CallID: "missing", Digest: "stale", Summary: "Do not apply first edit"},
		},
	})
	got := (&agent{}).executeContextEdit(sess, string(args))
	if !bytes.Contains(got, []byte("error")) || !bytes.Equal(before, mustJSONValue(t, sess)) {
		t.Fatalf("failed batch partially committed: %s", got)
	}

	got = shrinkContext(t, &agent{}, sess, contextDigest(sess.History[3]), strings.Repeat("large", 4000))
	if !bytes.Contains(got, []byte("error")) || len(sess.ContextEdits) != 0 {
		t.Fatalf("oversized edit accepted: %s", got)
	}

	sess.History[3] = mustJSONValue(t, map[string]any{
		"type": "function_call_output", "call_id": "logs",
		"output": `{"ok":true,"exit_code":0,"stdout":"short original"}`,
	})
	got = shrinkContext(t, &agent{}, sess, contextDigest(sess.History[3]), strings.Repeat("larger", 100))
	if !bytes.Contains(got, []byte("reduce estimated")) || len(sess.ContextEdits) != 0 {
		t.Fatalf("growing summary accepted: %s", got)
	}
}

func TestContextEditSaveFailureAndCorruptedResumeDoNotApply(t *testing.T) {
	sess := contextEditFixture(t)
	blocker := filepath.Join(sess.CWD, "file")
	if err := os.WriteFile(blocker, []byte("block directory creation"), 0600); err != nil {
		t.Fatal(err)
	}
	before := mustJSONValue(t, sess)
	got := shrinkContext(t, &agent{sessionPath: filepath.Join(blocker, "session.json")}, sess, contextDigest(sess.History[3]), "Build completed")
	if !bytes.Contains(got, []byte("error")) || !bytes.Equal(before, mustJSONValue(t, sess)) {
		t.Fatalf("save failure committed edit: %s", got)
	}

	sess.ContextEdits = []contextEdit{{Index: 3, Digest: "corrupted", Summary: "False context"}}
	path := filepath.Join(sess.CWD, "saved.json")
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(path); err == nil || !strings.Contains(err.Error(), "context edits") {
		t.Fatalf("corrupted projection restored: %v", err)
	}
}

func TestContextEditRejectsInvalidCallRelationships(t *testing.T) {
	for _, test := range []struct {
		name        string
		mutate      func(*session)
		outputIndex int
		wantError   string
	}{
		{
			name: "duplicate earlier calls",
			mutate: func(sess *session) {
				sess.History[1] = sess.History[2]
			},
			outputIndex: 3,
			wantError:   "ambiguous tool call relationship",
		},
		{
			name: "duplicate outputs",
			mutate: func(sess *session) {
				sess.History = append(sess.History, sess.History[3])
			},
			outputIndex: 3,
			wantError:   "ambiguous tool call relationship",
		},
		{
			name: "call after output",
			mutate: func(sess *session) {
				sess.History[2], sess.History[3] = sess.History[3], sess.History[2]
			},
			outputIndex: 2,
			wantError:   "earlier Bash call",
		},
		{
			name: "missing call",
			mutate: func(sess *session) {
				sess.History[2] = json.RawMessage(`{"role":"assistant","content":"No call"}`)
			},
			outputIndex: 3,
			wantError:   "ambiguous tool call relationship",
		},
		{
			name: "unrelated malformed item",
			mutate: func(sess *session) {
				sess.History = append(sess.History, json.RawMessage(`{"type":123}`))
			},
			outputIndex: 3,
			wantError:   "invalid history item",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sess := contextEditFixture(t)
			test.mutate(sess)
			before := mustJSONValue(t, sess)
			a := &agent{}

			inspection := a.executeContextEdit(sess, `{"action":"inspect"}`)
			var text string
			if err := json.Unmarshal(inspection, &text); err != nil || text != `{"candidates":[]}` {
				t.Fatalf("invalid relationship advertised: %s, %v", inspection, err)
			}
			if contextEditHint(sess.History) != nil {
				t.Fatal("invalid relationship advertised in context hint")
			}

			digest := contextDigest(sess.History[test.outputIndex])
			rejected := shrinkContext(t, a, sess, digest, "Build completed")
			if !bytes.Contains(rejected, []byte(test.wantError)) || !bytes.Equal(before, mustJSONValue(t, sess)) {
				t.Fatalf("invalid relationship accepted or state changed: %s", rejected)
			}

			sess.ContextEdits = []contextEdit{{Index: test.outputIndex, Digest: digest, Summary: "Build completed"}}
			if _, err := sess.requestHistory(); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("invalid saved projection accepted: %v", err)
			}
		})
	}
}

func BenchmarkContextEditInspect(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			sess := &session{}
			for i := 0; i < count; i++ {
				callID := fmt.Sprintf("logs-%d", i)
				call, err := json.Marshal(functionCall{Type: "function_call", CallID: callID, Name: "bash", Arguments: `{}`})
				if err != nil {
					b.Fatal(err)
				}
				body, err := json.Marshal(map[string]any{"ok": true, "exit_code": 0, "stdout": strings.Repeat("x", 1024)})
				if err != nil {
					b.Fatal(err)
				}
				output, err := json.Marshal(map[string]any{"type": "function_call_output", "call_id": callID, "output": string(body)})
				if err != nil {
					b.Fatal(err)
				}
				sess.History = append(sess.History, call, output)
			}
			a := &agent{}
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				a.executeContextEdit(sess, `{"action":"inspect"}`)
			}
		})
	}
}

func TestAgentExecutesContextEditWithoutReplayingBash(t *testing.T) {
	writeTestDeepSeekConfig(t)
	sess := contextEditFixture(t)
	marker := filepath.Join(sess.CWD, "bash-effects")
	mustWrite(t, marker, "original\n")
	sess.History[2] = mustJSONValue(t, functionCall{
		Type: "function_call", CallID: "logs", Name: "bash",
		Arguments: `{"command":"printf replayed >> bash-effects"}`,
	})

	digest := contextDigest(sess.History[3])
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		switch requests {
		case 1:
			writeSSEItem(t, w, `{"type":"function_call","call_id":"inspect","name":"edit_context","arguments":"{\"action\":\"inspect\"}"}`, 5000)
		case 2:
			args := mustJSONValue(t, map[string]any{"action": "shrink", "edits": []stdoutEdit{{CallID: "logs", Digest: digest, Summary: "Build completed"}}})
			call := mustJSONValue(t, functionCall{Type: "function_call", CallID: "shrink", Name: "edit_context", Arguments: string(args)})
			writeSSEItem(t, w, string(call), 5000)
		case 3:
			if !bytes.Contains(request.Input[3], []byte("Build completed")) || bytes.Contains(request.Input[3], []byte("ORIGINAL-RECALL-FACT")) {
				t.Error("model request did not use accepted edit")
			}
			if !bytes.Equal(request.Input[2], sess.History[2]) {
				t.Error("Bash call changed")
			}
			writeSSEItem(t, w, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`, 600)
		default:
			t.Error("unexpected model request")
		}
	}))
	defer server.Close()
	a := newAgent(io.Discard, io.Discard, filepath.Join(sess.CWD, "session.json"), time.Second, false)
	a.backend = &responsesClient{
		profile:    profileDeepSeek,
		models:     defaultProviderConfig().Models,
		httpClient: server.Client(),
		stdout:     io.Discard,
		endpoint:   server.URL,
	}
	for turn := 0; turn < 3; turn++ {
		terminalItems, err := a.runTurn(context.Background(), sess, "instructions")
		if err != nil {
			t.Fatal(err)
		}
		if (len(terminalItems) > 0) != (turn == 2) {
			t.Fatalf("unexpected completion at turn %d", turn)
		}
	}
	if len(sess.ContextEdits) != 1 || !bytes.Contains(sess.History[3], []byte("ORIGINAL-RECALL-FACT")) {
		t.Fatal("original output or accepted projection lost")
	}

	assertContent(t, marker, "original\n")
}

func TestPortableCompactionReceivesProjectionButArchivesOriginal(t *testing.T) {
	sess := contextEditFixture(t)
	path := filepath.Join(sess.CWD, "session.json")
	a := newAgent(io.Discard, io.Discard, path, time.Second, false)
	shrinkContext(t, a, sess, contextDigest(sess.History[3]), "Build completed")
	if err := appendUserPrompt(sess, "continue"); err != nil {
		t.Fatal(err)
	}
	backend := &checkpointStub{reply: "Goal: build completed. Retrieve original logs with call ID logs."}
	a.backend = backend
	sess.ContextTokens = modelContextWindow
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	if len(backend.sources) != 1 || !strings.Contains(backend.sources[0], "Build completed") || strings.Contains(backend.sources[0], "ORIGINAL-RECALL-FACT") {
		t.Fatal("checkpoint did not receive projection")
	}
	saved, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	result := searchTranscript(saved, mustJSONValue(t, map[string]any{"query": "ORIGINAL-RECALL-FACT", "limit": 20}), "")
	if !bytes.Contains(result, []byte("ORIGINAL-RECALL-FACT")) {
		t.Fatal("original log not archived")
	}
	if len(saved.ContextEdits) != 0 {
		t.Fatal("superseded projection retained")
	}
}
