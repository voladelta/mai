package mai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sessionFixture(t *testing.T) *session {
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
		CWD: dir, RepoRoot: dir, Model: "deepseek-flash", Effort: "h",
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

func appendBudgetLog(t *testing.T, sess *session, prefix, stdout string) string {
	t.Helper()
	factCallID := ""
	for start, part := 0, 0; start < len(stdout); part++ {
		end := min(start+maxToolStreamBytes, len(stdout))
		if end < len(stdout) {
			if newline := strings.LastIndexByte(stdout[start:end], '\n'); newline >= 0 {
				end = start + newline + 1
			}
		}
		chunk := stdout[start:end]
		callID := fmt.Sprintf("%s-%03d", prefix, part)
		sess.appendEstimatedHistory(mustJSONValue(t, functionCall{Type: "function_call", CallID: callID, Name: "bash", Arguments: `{"command":"read build log"}`}))
		body := mustJSONValue(t, map[string]any{"ok": true, "exit_code": 0, "timed_out": false, "stdout": chunk, "stderr": ""})
		sess.appendEstimatedHistory(mustJSONValue(t, map[string]any{"type": "function_call_output", "call_id": callID, "output": string(body)}))
		if strings.Contains(chunk, "Retired audit:") {
			factCallID = callID
		}
		start = end
	}
	return factCallID
}
func researchRecallOriginal(sess *session, code, callID string) bool {
	args, _ := json.Marshal(map[string]any{"query": code, "limit": 20})
	var result struct {
		Matches []struct {
			Kind   string `json:"kind"`
			CallID string `json:"call_id"`
			Text   string `json:"text"`
		} `json:"matches"`
	}
	if json.Unmarshal(searchTranscript(sess, args, ""), &result) != nil {
		return false
	}
	for _, match := range result.Matches {
		if match.Kind == "tool_result" && match.CallID == callID && strings.Contains(match.Text, code) && strings.Contains(match.Text, "build diagnostic") {
			return true
		}
	}
	return false
}

func writeSSEItem(t *testing.T, w http.ResponseWriter, item string, tokens int64) {
	t.Helper()
	fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":%s}\n\n", item)
	fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"total_tokens\":%d}}}\n\n", tokens)
}
func historyItemType(item json.RawMessage) string {
	var value struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(item, &value)
	return value.Type
}
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeTestDefaultProviderConfig(t *testing.T) {
	t.Helper()
	t.Setenv("ENCLAVE_API_KEY", "test-key")
	t.Setenv("MAI_CONTEXT_WINDOW", "")
	// Keep the developer's real ~/.mai.config out of tests: an empty HOME
	// makes loadProviderConfig fall back to the built-in enclave default.
	t.Setenv("HOME", t.TempDir())
}

// deepseekProviderConfig is the DeepSeek provider preset for tests whose
// subject is the deepseek protocol profile, not the built-in default.
func deepseekProviderConfig() providerConfig {
	return providerConfig{
		BaseURL:   "https://api.deepseek.com",
		APIKeyEnv: "DEEPSEEK_API_KEY",
		Model:     "deepseek-v4-pro",
		Profile:   profileDeepSeek,
	}
}

func writeTestFailureServer(t *testing.T) {
	t.Helper()
	writeTestDefaultProviderConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(server.Close)
	t.Setenv("MAI_BASE_URL", server.URL)
}
