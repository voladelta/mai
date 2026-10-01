package mai

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Controlled compatibility probe: obtain real backend call/reasoning items,
// supply a synthetic successful Bash result, and shorten it before resampling.
// No model-proposed command runs and no real task history is read.
func TestLiveContextEditCompatibility(t *testing.T) {
	if os.Getenv("MAI_LIVE_CONTEXT_EDIT_EVAL") != "1" {
		t.Skip("set MAI_LIVE_CONTEXT_EDIT_EVAL=1 to test edited context with Codex login")
	}
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	sess := &session{ID: id, Model: "luna", Effort: "m", RequestEffort: "m"}
	if err := appendUserPrompt(sess, "Call bash exactly once to inspect a build log. After the result, return its release code and nothing else. Do not call any other tool."); err != nil {
		t.Fatal(err)
	}
	client := newCodexClient(io.Discard, 2*time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result, err := client.stream(ctx, sess, "Use the bash tool once before answering. The host will supply its result.")
	if err != nil {
		t.Fatal(err)
	}
	calls, err := extractFunctionCalls(result.items)
	if err != nil || len(calls) != 1 || calls[0].Name != "bash" {
		t.Fatalf("expected one Bash call: calls=%v err=%v", calls, err)
	}
	sess.History = append(sess.History, result.items...)
	output := mustJSONValue(t, map[string]any{
		"ok": true, "exit_code": 0, "timed_out": false,
		"stdout": strings.Repeat("completed build output\n", 1000) + "release code: LIVE-219-Q7",
		"stderr": "",
	})
	sess.History = append(sess.History, mustJSONValue(t, map[string]any{
		"type": "function_call_output", "call_id": calls[0].CallID, "output": string(output),
	}))
	index := len(sess.History) - 1
	args := mustJSONValue(t, map[string]any{
		"action": "shrink", "edits": []stdoutEdit{{CallID: calls[0].CallID, Digest: contextDigest(sess.History[index]), Summary: "Build completed. Release code: LIVE-219-Q7."}},
	})
	receipt := (&agent{}).executeContextEdit(sess, string(args))
	if len(sess.ContextEdits) != 1 {
		t.Fatalf("edit failed: %s", receipt)
	}
	projected, err := sess.requestHistory()
	if err != nil {
		t.Fatal(err)
	}
	result, err = client.stream(ctx, sess, "Return the release code from the Bash result, and nothing else. Do not call tools.")
	if err != nil {
		t.Fatal(err)
	}
	if calls, err := extractFunctionCalls(result.items); err != nil || len(calls) != 0 {
		t.Fatalf("unexpected tool calls: %v %v", calls, err)
	}
	var answer string
	for _, raw := range result.items {
		entry, visible, err := visibleTranscriptEntry(raw)
		if err != nil {
			t.Fatal(err)
		}
		if visible && entry.Kind == "assistant" {
			answer = strings.TrimSpace(entry.Text)
		}
	}
	if answer != "LIVE-219-Q7" {
		t.Fatalf("edited context answer = %q", answer)
	}
	t.Logf("backend accepted edited stdout with original call/reasoning items; exact code retained; estimated history %d -> %d tokens", estimateHistoryTokens(sess.History), estimateHistoryTokens(projected))
	if result.usage != nil {
		usage, _ := json.Marshal(result.usage)
		t.Logf("answer request usage: %s", usage)
	}
}
