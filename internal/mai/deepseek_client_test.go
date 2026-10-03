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

func deepseekTestSession(t *testing.T) *session {
	t.Helper()
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sess := &session{Version: stateVersion, ID: id, CWD: dir, RepoRoot: dir, Model: "flash", Effort: "h"}
	if err := appendUserPrompt(sess, "Use the tool, then answer."); err != nil {
		t.Fatal(err)
	}
	return sess
}

func deepseekTestResponse(w http.ResponseWriter, items string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":%s,\"usage\":{\"input_tokens\":100,\"output_tokens\":12,\"total_tokens\":112,\"input_tokens_details\":{\"cached_tokens\":64}}}}\n\n", items)
}

func TestDeepSeekResponsesToolReasoningAndResume(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "private-test-key")
	t.Setenv("MAI_CONTEXT_WINDOW", "")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/responses" || r.Header.Get("Authorization") != "Bearer private-test-key" || r.Header.Get("chatgpt-account-id") != "" {
			t.Error("wrong endpoint or credentials")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["model"]) != `"deepseek-flash"` || !bytes.Contains(body["reasoning"], []byte(`"high"`)) || string(body["stream"]) != "true" {
			t.Error("wrong model/effort/stream")
		}
		for _, field := range []string{"messages", "thinking", "include", "store", "prompt_cache_key", "previous_response_id"} {
			if _, exists := body[field]; exists {
				t.Errorf("unsupported field %s", field)
			}
		}
		if requests == 1 {
			deepseekTestResponse(w, `[{"type":"reasoning","encrypted_content":"compatibility-placeholder","content":[{"type":"reasoning_text","text":"inspect result"}]},{"type":"function_call","call_id":"call-1","name":"bash","arguments":"{\"command\":\"printf verified\"}"}]`)
			return
		}
		if !bytes.Contains(body["input"], []byte("reasoning_text")) || !bytes.Contains(body["input"], []byte("verified")) {
			t.Error("reasoning or tool result lost across resume")
		}
		if bytes.Contains(body["input"], []byte("encrypted_content")) {
			t.Error("opaque compatibility field was replayed")
		}
		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL+"/responses")
	sess := deepseekTestSession(t)
	path := filepath.Join(t.TempDir(), "session.json")
	a := newAgent(io.Discard, io.Discard, path, time.Second, false)
	if err := a.configureBackend(sess, defaultProviderConfig()); err != nil {
		t.Fatal(err)
	}
	if a.contextWindow != 1_000_000 {
		t.Fatal("wrong compaction configuration")
	}
	if terminalItems, err := a.runTurn(context.Background(), sess, "test"); err != nil || len(terminalItems) != 0 {
		t.Fatalf("tool turn: terminal=%v %v", terminalItems, err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil || bytes.Contains(encoded, []byte("private-test-key")) {
		t.Fatal("credentials persisted or no session")
	}
	sess, err = loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.configureBackend(sess, defaultProviderConfig()); err != nil {
		t.Fatal(err)
	}
	if terminalItems, err := a.runTurn(context.Background(), sess, "test"); err != nil || len(terminalItems) == 0 {
		t.Fatalf("final turn: %v %v", terminalItems, err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d", requests)
	}
}

func TestDeepSeekCheckpointAndProTools(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if string(body["model"]) != `"deepseek-v4-pro"` || !bytes.Contains(body["reasoning"], []byte(`"max"`)) {
			t.Error("Pro model/effort lost")
		}
		if string(body["tool_choice"]) != `"none"` && (!bytes.Contains(body["tools"], []byte("view_image")) || !bytes.Contains(body["tools"], []byte("Flash request"))) {
			t.Error("Pro did not advertise the Flash image fallback")
		}
		if string(body["tool_choice"]) == `"none"` {
			if _, exists := body["tools"]; exists {
				t.Error("summary contains tools")
			}
		}
		deepseekTestResponse(w, `[{"type":"reasoning","content":[{"type":"reasoning_text","text":"private reasoning"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Goal: preserve release REL-123"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":" and UNKNOWN outcome."}]}]`)
	}))
	defer server.Close()
	var output bytes.Buffer
	c := &responsesClient{
		profile:    profileDeepSeek,
		models:     defaultProviderConfig().Models,
		httpClient: server.Client(),
		endpoint:   server.URL,
		apiKey:     "test",
		stdout:     &output,
	}
	sess := deepseekTestSession(t)
	sess.Model, sess.Effort = "pro", "max"
	if _, err := c.stream(context.Background(), sess, "test"); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	text, usage, err := c.summarize(context.Background(), sess, "history")
	if err != nil || text != "Goal: preserve release REL-123 and UNKNOWN outcome." || usage == nil || *usage.InputTokensDetails.CachedTokens != 64 || output.Len() != 0 {
		t.Fatalf("invalid checkpoint: %q %+v %v", text, usage, err)
	}
}

func TestDeepSeekRejectsIncompleteResponsesAndOpaqueHistory(t *testing.T) {
	for _, status := range []string{"incomplete", "failed", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, "data: {\"type\":\"response.%s\",\"response\":{\"status\":\"%s\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"x\",\"name\":\"bash\",\"arguments\":\"{}\"}],\"error\":{\"message\":\"SECRET\"}}}\n\n", status, status)
			}))
			defer server.Close()
			c := &responsesClient{
				profile:    profileDeepSeek,
				models:     defaultProviderConfig().Models,
				httpClient: server.Client(),
				endpoint:   server.URL,
				stdout:     io.Discard,
			}
			result, err := c.stream(context.Background(), deepseekTestSession(t), "test")
			if err == nil || len(result.items) != 0 || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("unsafe incomplete response: %+v %v", result, err)
			}
		})
	}
	for _, raw := range []string{`{"type":"reasoning","encrypted_content":"opaque"}`, `{"type":"compaction","encrypted_content":"opaque"}`, `{"type":"reasoning","summary":[]}`, `{"type":"function_call_output","call_id":"missing","output":"x"}`} {
		if err := validateResponsesHistory([]json.RawMessage{json.RawMessage(raw)}, "flash", profileDeepSeek); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	image := json.RawMessage(`{"role":"user","content":[{"type":"input_image","file_id":"file-api-example"}]}`)
	if err := validateResponsesHistory([]json.RawMessage{image}, "pro", profileDeepSeek); err == nil {
		t.Fatal("Pro accepted image")
	}
	if err := validateResponsesHistory([]json.RawMessage{image}, "flash", profileDeepSeek); err != nil {
		t.Fatal(err)
	}
}

func TestDeepSeekIdleTimeoutAndRedirectIsolation(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			destinationCalls := 0
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls++ }))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			t.Setenv("DEEPSEEK_API_KEY", "test")
			t.Setenv("MAI_CONTEXT_WINDOW", "")
			t.Setenv("MAI_DEEPSEEK_URL", server.URL)
			sess := deepseekTestSession(t)
			a := newAgent(io.Discard, io.Discard, "", 30*time.Millisecond, false)
			if err := a.configureBackend(sess, defaultProviderConfig()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := a.backend.stream(ctx, sess, "test"); err == nil {
				t.Fatal("accepted redirect or stalled stream")
			}
			if destinationCalls != 0 {
				t.Fatal("redirect forwarded credentials")
			}
		})
	}
}

func TestDeepSeekFlashPreservesToolImagesAndRejectsLossyCompaction(t *testing.T) {
	sess := deepseekTestSession(t)
	call := json.RawMessage(`{"type":"function_call","call_id":"image-call","name":"view_image","arguments":"{}"}`)
	image := json.RawMessage(`{"type":"function_call_output","call_id":"image-call","output":[{"type":"input_text","text":"local image"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo=","detail":"original"}]}`)
	sess.History = append(sess.History, call, image)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Contains(data, []byte("data:image/png;base64,iVBORw0KGgo=")) {
			t.Error("image was lost in Responses request")
		}
		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"image seen"}]}]`)
	}))
	defer server.Close()
	c := &responsesClient{
		profile:    profileDeepSeek,
		models:     defaultProviderConfig().Models,
		httpClient: server.Client(),
		endpoint:   server.URL,
		stdout:     io.Discard,
	}
	if _, err := c.stream(context.Background(), sess, "test"); err != nil {
		t.Fatal(err)
	}
	if err := appendUserPrompt(sess, "continue"); err != nil {
		t.Fatal(err)
	}
	before, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	backend := &checkpointStub{reply: "summary"}
	if _, _, err := portableHistory(context.Background(), sess, backend); err == nil {
		t.Fatal("image silently discarded by checkpoint")
	}
	after, err := json.Marshal(sess)
	if err != nil || !bytes.Equal(before, after) || len(backend.sources) != 0 {
		t.Fatal("failed media checkpoint changed state or called model")
	}
}

func TestDeepSeekRejectsMalformedCompletedToolItems(t *testing.T) {
	for _, items := range []string{
		`[{"type":"function_call","call_id":"duplicate","name":"bash","arguments":"{}"},{"type":"function_call","call_id":"duplicate","name":"bash","arguments":"{}"}]`,
		`[{"type":"function_call","call_id":"x","name":"bash","arguments":"{}","status":"incomplete"}]`,
		`[{"type":"reasoning","encrypted_content":"opaque","summary":[]}]`,
		`[{"type":"message","role":"user","content":[{"type":"output_text","text":"wrong role"}]}]`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { deepseekTestResponse(w, items) }))
		c := &responsesClient{
			profile:    profileDeepSeek,
			models:     defaultProviderConfig().Models,
			httpClient: server.Client(),
			endpoint:   server.URL,
			stdout:     io.Discard,
		}
		result, err := c.stream(context.Background(), deepseekTestSession(t), "test")
		server.Close()
		if err == nil || len(result.items) != 0 {
			t.Fatalf("accepted malformed items: %s", items)
		}
	}
}

func TestDeepSeekChecksToolCallIDAgainstHistoryBeforeEffects(t *testing.T) {
	for _, callID := range []string{"existing", "fresh"} {
		t.Run(callID, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := functionCall{
					Type: "function_call", CallID: callID, Name: "bash",
					Arguments: `{"command":"printf executed > marker"}`,
				}
				deepseekTestResponse(w, "["+string(mustJSON(t, call))+"]")
			}))
			defer server.Close()

			sess := deepseekTestSession(t)
			sess.appendEstimatedHistory(
				json.RawMessage(`{"type":"function_call","call_id":"existing","name":"bash","arguments":"{}"}`),
				json.RawMessage(`{"type":"function_call_output","call_id":"existing","output":"done"}`),
			)
			path := filepath.Join(t.TempDir(), "session.json")
			if err := saveJSON(path, sess); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			a := &agent{
				backend: &responsesClient{
					profile:    profileDeepSeek,
					models:     defaultProviderConfig().Models,
					httpClient: server.Client(),
					endpoint:   server.URL,
					stdout:     io.Discard,
				},
				stdout: io.Discard, stderr: io.Discard, sessionPath: path,
			}

			terminalItems, err := a.runTurn(context.Background(), sess, "test")
			marker := filepath.Join(sess.CWD, "marker")
			if callID == "fresh" {
				if err != nil || len(terminalItems) != 0 {
					t.Fatalf("fresh call: terminal=%v error=%v", terminalItems, err)
				}
				if content, err := os.ReadFile(marker); err != nil || string(content) != "executed" {
					t.Fatalf("fresh call did not execute: %q, %v", content, err)
				}
			} else {
				if err == nil {
					t.Error("accepted a tool call ID already in history")
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Errorf("reused call performed effects: %v", err)
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(before, after) {
					t.Error("rejected response changed saved history")
				}
			}

			if err := validateResponsesHistory(sess.History, sess.Model, profileDeepSeek); err != nil {
				t.Fatalf("left invalid history: %v", err)
			}
		})
	}
}

func TestDeepSeekRejectsNamelessHistoryBeforeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`)
	}))
	defer server.Close()

	sess := deepseekTestSession(t)
	sess.appendEstimatedHistory(
		json.RawMessage(`{"type":"function_call","call_id":"existing","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"existing","output":"done"}`),
	)
	before := mustJSON(t, sess)
	client := &responsesClient{
		profile:    profileDeepSeek,
		models:     defaultProviderConfig().Models,
		httpClient: server.Client(),
		endpoint:   server.URL,
		stdout:     io.Discard,
	}
	result, err := client.stream(context.Background(), sess, "test")

	if err == nil || !strings.Contains(err.Error(), "incomplete function call") || len(result.items) != 0 {
		t.Fatalf("accepted nameless history: result=%+v error=%v", result, err)
	}
	if requests != 0 || !bytes.Equal(before, mustJSON(t, sess)) {
		t.Fatal("invalid history made a request or changed session state")
	}
}
