package mai

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

func writeTestDeepSeekConfig(t *testing.T) {
	t.Helper()
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("MAI_CONTEXT_WINDOW", "")
	// Keep the developer's real ~/.mai.config out of tests: an empty HOME
	// makes loadProviderConfig fall back to the built-in DeepSeek default.
	t.Setenv("HOME", t.TempDir())
}

func writeTestDeepSeekFailureServer(t *testing.T) {
	t.Helper()
	writeTestDeepSeekConfig(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	t.Cleanup(server.Close)
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)
}
