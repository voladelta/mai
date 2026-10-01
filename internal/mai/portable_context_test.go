package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type checkpointStub struct {
	sources []string
	reply   string
	err     error
}

func (s *checkpointStub) stream(context.Context, *session, string) (streamResult, error) {
	return streamResult{}, errors.New("unexpected generation")
}
func (s *checkpointStub) summarize(_ context.Context, _ *session, source string) (string, *tokenUsage, error) {
	s.sources = append(s.sources, source)
	return s.reply, &tokenUsage{TotalTokens: 12}, s.err
}

func portableFixture(t *testing.T) *session {
	t.Helper()
	sess := contextEditFixture(t)
	_ = appendUserPrompt(sess, "Correction: batch is 128; preserve UNKNOWN.")
	sess.ContextTokens = modelContextWindow
	return sess
}

func TestPortableCheckpointArchivesOriginalAndRetainsActiveTurn(t *testing.T) {
	sess := portableFixture(t)
	before, _ := json.Marshal(sess)
	last := append(json.RawMessage(nil), sess.History[len(sess.History)-1]...)
	backend := &checkpointStub{reply: "Goal: batch 128. Deploy UNKNOWN. Retrieve original release with call ID call-a."}
	a := newAgent(io.Discard, io.Discard, filepath.Join(t.TempDir(), "session.json"), time.Second, false, nil)
	a.backend, a.portable = backend, true
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	if len(backend.sources) != 1 || !bytes.Equal(last, sess.History[len(sess.History)-1]) || len(sess.ContextEdits) != 0 {
		t.Fatal("active turn changed or summary not applied")
	}
	for _, item := range sess.History {
		if historyItemType(item) == "compaction" {
			t.Fatal("opaque checkpoint persisted")
		}
	}
	saved, err := loadSession(a.sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TranscriptEnd <= 0 || saved.ContextTokens >= modelContextWindow {
		t.Fatal("checkpoint or archive missing after resume")
	}
	var original session
	_ = json.Unmarshal(before, &original)
	entries, err := visibleHistoryEntries(original.History)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(transcriptPath(a.sessionPath))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var archived []transcriptEntry
	decoder := json.NewDecoder(file)
	for decoder.More() {
		var entry transcriptEntry
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		archived = append(archived, entry)
	}
	if len(archived) != len(entries) {
		t.Fatalf("original archive lost entries: %d vs %d", len(archived), len(entries))
	}
	if !bytes.Equal(mustJSONValue(t, archived), mustJSONValue(t, entries)) {
		t.Fatal("original archive content changed")
	}
}

func TestPortableCheckpointFailureLeavesSessionUnchanged(t *testing.T) {
	for _, reply := range []string{"", strings.Repeat("x", (16<<10)+1)} {
		sess := portableFixture(t)
		before, _ := json.Marshal(sess)
		a := newAgent(io.Discard, io.Discard, "", time.Second, false, nil)
		a.backend, a.portable = &checkpointStub{reply: reply}, true
		if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err == nil {
			t.Fatal("invalid summary accepted")
		}
		after, _ := json.Marshal(sess)
		if !bytes.Equal(before, after) {
			t.Fatal("failed compaction changed authoritative state")
		}
	}
}

func TestPortableCheckpointRefusesPendingCallsAndOpaqueState(t *testing.T) {
	for _, item := range []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"pending","name":"bash","arguments":"{}"}`),
		json.RawMessage(`{"type":"compaction","encrypted_content":"opaque"}`),
		json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"caption"},{"type":"input_image","image_url":"hidden"}]}`),
	} {
		sess := portableFixture(t)
		sess.History = append(sess.History[:len(sess.History)-1], item)
		_ = appendUserPrompt(sess, "next")
		backend := &checkpointStub{reply: "summary"}
		if _, _, err := portableHistory(context.Background(), sess, backend); err == nil || len(backend.sources) != 0 {
			t.Fatal("unsafe prefix reached generation")
		}
	}
}

func TestPortableCheckpointFoldsAllUniqueInputInOrder(t *testing.T) {
	sess := portableFixture(t)
	var unique strings.Builder
	for i := 0; i < 6000; i++ {
		unique.WriteString(strings.Repeat("x", 25))
		unique.WriteString(time.Duration(i).String())
		unique.WriteByte('\n')
	}
	_ = appendUserPrompt(sess, unique.String()+"\nFINAL-ANCHOR")
	_ = appendUserPrompt(sess, "active turn")
	backend := &checkpointStub{reply: "checkpoint retained"}
	if _, _, err := portableHistory(context.Background(), sess, backend); err != nil {
		t.Fatal(err)
	}
	if len(backend.sources) < 2 || !strings.Contains(backend.sources[len(backend.sources)-1], "FINAL-ANCHOR") || !strings.Contains(backend.sources[1], "checkpoint retained") {
		t.Fatal("chunk folding discarded history or prior checkpoint")
	}
}

func TestChatAdapterToolRoundTripAndSummaryHasNoTools(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("missing provider auth")
		}
		if calls == 1 {
			var messages []chatMessage
			_ = json.Unmarshal(body["messages"], &messages)
			if len(messages) < 3 || messages[len(messages)-1].Role != "tool" || messages[len(messages)-1].ToolCallID != "logs" || body["tools"] == nil {
				t.Error("tool history/schema did not translate")
			}
		} else if body["tools"] != nil {
			t.Error("summary could execute tools")
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,"prompt_tokens_details":{"cached_tokens":40}}}`)
	}))
	defer server.Close()
	client := &chatClient{httpClient: server.Client(), endpoint: server.URL, model: "test-model", apiKey: "test-key", stdout: io.Discard}
	sess := contextEditFixture(t)
	sess.History = append(sess.History[:1], sess.History[2:]...)
	result, err := client.stream(context.Background(), sess, "instructions")
	if err != nil || result.totalTokens != 105 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	text, _, err := client.summarize(context.Background(), nil, "records")
	if err != nil || text != "done" {
		t.Fatalf("summary=%q err=%v", text, err)
	}
}

func TestPortableChunksPreserveMultibyteText(t *testing.T) {
	sess := portableFixture(t)
	_ = appendUserPrompt(sess, "a"+strings.Repeat("界", 40000))
	_ = appendUserPrompt(sess, "active")
	backend := &checkpointStub{reply: "checkpoint retained"}
	if _, _, err := portableHistory(context.Background(), sess, backend); err != nil {
		t.Fatal(err)
	}
	if len(backend.sources) < 2 {
		t.Fatal("fixture did not cross chunk boundary")
	}
	for _, source := range backend.sources {
		if !utf8.ValidString(source) {
			t.Fatal("chunk split a UTF-8 character")
		}
	}
}

func TestChatUsageCanBeMissingAndThinkingStateCannotDisappear(t *testing.T) {
	for _, thinking := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if thinking {
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done","reasoning_content":"required state"},"finish_reason":"stop"}]}`)
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
		}))
		client := &chatClient{httpClient: server.Client(), endpoint: server.URL, model: "test", stdout: io.Discard}
		result, err := client.stream(context.Background(), &session{}, "instructions")
		if thinking && err == nil {
			t.Fatal("required thinking state silently dropped")
		}
		if !thinking && (err != nil || result.usage != nil) {
			t.Fatalf("missing usage became zero or failed: %+v %v", result, err)
		}
		server.Close()
	}
}

func TestChatAdapterRejectsTruncationAndRedactsProviderErrors(t *testing.T) {
	for _, status := range []int{200, 401} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			io.WriteString(w, `{"secret":"test-key","choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}]}`)
		}))
		client := &chatClient{httpClient: server.Client(), endpoint: server.URL, model: "test-model", apiKey: "test-key", stdout: io.Discard}
		if _, _, err := client.summarize(context.Background(), nil, "records"); err == nil || strings.Contains(err.Error(), "test-key") {
			t.Fatal("partial or secret-bearing error accepted")
		}
		server.Close()
	}
}

func TestChatConfigurationPinsBackendWithoutSavingCredentials(t *testing.T) {
	t.Setenv("MAI_PROVIDER", "chat")
	t.Setenv("MAI_COMPACTION", "")
	t.Setenv("MAI_CHAT_URL", "http://127.0.0.1:1234/chat/completions")
	t.Setenv("MAI_CHAT_MODEL", "model-one")
	t.Setenv("MAI_CHAT_KEY_ENV", "MAI_TEST_KEY")
	t.Setenv("MAI_TEST_KEY", "private-test-key")
	t.Setenv("MAI_CONTEXT_WINDOW", "65536")
	sess := contextEditFixture(t)
	sess.History = append(sess.History[:1], sess.History[2:]...)
	a := newAgent(io.Discard, io.Discard, "", time.Second, false, nil)
	if err := a.configureBackend(sess); err != nil {
		t.Fatal(err)
	}
	if a.backend == nil || !a.portable || a.contextWindow != 65536 || sess.Model != "model-one" {
		t.Fatal("chat configuration not applied")
	}
	if strings.Contains(string(mustJSONValue(t, sess)), "private-test-key") {
		t.Fatal("credential persisted")
	}
	path := filepath.Join(t.TempDir(), "session.json")
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadSession(path)
	if err != nil || reloaded.Model != "model-one" || reloaded.Backend != sess.Backend {
		t.Fatalf("chat state failed to resume: %+v %v", reloaded, err)
	}

	t.Setenv("MAI_CHAT_MODEL", "model-two")
	if err := a.configureBackend(sess); err == nil {
		t.Fatal("silent model change on resume")
	}
	t.Setenv("MAI_CHAT_MODEL", "model-one")
	t.Setenv("MAI_PROVIDER", "codex")
	if err := a.configureBackend(sess); err == nil {
		t.Fatal("silent provider change on resume")
	}
}

func TestChatHistoryPreservesParallelToolPairing(t *testing.T) {
	history := []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"a","name":"bash","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call","call_id":"b","name":"bash","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"a","output":"result-a"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"b","output":"result-b"}`),
	}
	messages, err := chatHistory(history, "")
	if err != nil || len(messages) != 3 || len(messages[0].ToolCalls) != 2 || messages[1].ToolCallID != "a" || messages[2].ToolCallID != "b" {
		t.Fatalf("tool grouping changed: %+v %v", messages, err)
	}
}

func TestChatCLIExecutesToolsAndResumesWithoutCodexLogin(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("MAI_PROVIDER", "chat")
	t.Setenv("MAI_COMPACTION", "portable")
	t.Setenv("MAI_CHAT_MODEL", "test-chat-model")
	t.Setenv("MAI_CHAT_KEY_ENV", "MAI_TEST_KEY")
	t.Setenv("MAI_TEST_KEY", "private-test-key")
	t.Setenv("MAI_CONTEXT_WINDOW", "65536")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body struct {
			Model    string        `json:"model"`
			Messages []chatMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "test-chat-model" {
			t.Error("Codex model alias leaked to chat backend")
		}
		if calls == 1 {
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"tool-one","type":"function","function":{"name":"bash","arguments":"{\"command\":\"printf backend-tool-ok\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10,"total_tokens":20}}`)
			return
		}
		found := false
		for _, message := range body.Messages {
			if message.Role == "tool" && strings.Contains(message.Content, "backend-tool-ok") {
				found = true
			}
		}
		if !found {
			t.Error("tool result lost during generation or resume")
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105}}`)
	}))
	defer server.Close()
	t.Setenv("MAI_CHAT_URL", server.URL)
	for _, args := range [][]string{
		{"start", "--persist", "--jsonl", "--no-input", "--skip-skills"},
		{"continue", "--last", "--jsonl", "--no-input", "--skip-skills"},
	} {
		var stdout, stderr bytes.Buffer
		if status := Main(args, &stdout, &stderr); status != 0 {
			t.Fatalf("CLI failed: %d %s", status, stderr.String())
		}
		decoder := json.NewDecoder(&stdout)
		for decoder.More() {
			var event map[string]any
			if err := decoder.Decode(&event); err != nil {
				t.Fatal("invalid JSONL output: ", err)
			}
		}
	}
	if calls != 3 {
		t.Fatalf("unexpected provider request count %d", calls)
	}
}

func TestGPT61SolUsesExactModelAndMediumEffort(t *testing.T) {
	options, err := parseOptions([]string{"test", "-m", "gpt-6.1-sol", "-e", "medium"})
	if err != nil {
		t.Fatal(err)
	}
	sess := contextEditFixture(t)
	sess.Model, sess.Effort, sess.RequestEffort = options.model, options.effort, options.effort
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "gpt-6.1-sol" || body.Reasoning.Effort != "medium" {
			t.Errorf("wrong comparison model or effort: %+v", body)
		}
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]}]}}\n\n")
	}))
	defer server.Close()
	client := newCodexClient(io.Discard, time.Second)
	client.endpoint = server.URL
	if _, err := client.requestOnce(context.Background(), sess, "instructions", credentials{}, false); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "session.json")
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(path); err != nil {
		t.Fatal("new model could not resume: ", err)
	}
}
