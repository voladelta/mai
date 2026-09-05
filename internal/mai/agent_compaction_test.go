package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunTurnCompactsBeforeSamplingAndSavesReplacement(t *testing.T) {
	writeTestCodexAuth(t)
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "text/event-stream")
		if hasCompactionTrigger(body["input"]) {
			writeSSEItem(t, w, `{"type":"compaction","encrypted_content":"summary"}`, 250_000)
			return
		}
		writeSSEItem(t, w, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`, 12_345)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "session.json")
	sess := &session{
		Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef",
		CWD: t.TempDir(), RepoRoot: t.TempDir(), Model: "luna", Effort: "m",
		ContextTokens: 244_800,
		History: []json.RawMessage{
			json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"keep me"}]}`),
			json.RawMessage(`{"type":"function_call","call_id":"old","name":"bash","arguments":"{}"}`),
			json.RawMessage(`{"type":"function_call_output","call_id":"old","output":"old output"}`),
		},
	}
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	a := newAgent(&bytes.Buffer{}, &bytes.Buffer{}, path, time.Second, false, nil)
	a.client.endpoint = server.URL
	done, err := a.runTurn(context.Background(), sess, "instructions")
	if err != nil {
		t.Fatal(err)
	}
	if !done || len(requests) != 2 || !hasCompactionTrigger(requests[0]["input"]) || hasCompactionTrigger(requests[1]["input"]) {
		t.Fatalf("done=%v requests=%#v", done, requests)
	}
	if len(sess.History) != 3 || historyItemType(sess.History[1]) != "compaction" || historyItemType(sess.History[2]) != "message" {
		t.Fatalf("replacement history = %s", mustJSON(t, sess.History))
	}
	if sess.ContextTokens != 12_345 {
		t.Fatalf("context tokens = %d", sess.ContextTokens)
	}
	saved, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.History) != 3 || historyItemType(saved.History[1]) != "compaction" || saved.ContextTokens != 12_345 {
		t.Fatalf("saved session = %#v", saved)
	}
}

func TestFailedCompactionLeavesHistoryUnchanged(t *testing.T) {
	writeTestCodexAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEItem(t, w, `{"type":"message","role":"assistant","content":[]}`, 250_000)
	}))
	defer server.Close()

	before := []json.RawMessage{json.RawMessage(`{"role":"user","content":"keep"}`)}
	sess := &session{ID: "session", Model: "luna", Effort: "m", ContextTokens: 244_800, History: append([]json.RawMessage(nil), before...)}
	a := newAgent(&bytes.Buffer{}, &bytes.Buffer{}, "", time.Second, false, nil)
	a.client.endpoint = server.URL
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err == nil {
		t.Fatal("invalid compaction response was accepted")
	}
	if string(sess.History[0]) != string(before[0]) || sess.ContextTokens != 244_800 {
		t.Fatalf("failed compaction changed session: %#v", sess)
	}
}

func writeTestCodexAuth(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(`{"tokens":{"access_token":"token","account_id":"account"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func hasCompactionTrigger(input any) bool {
	items, _ := input.([]any)
	if len(items) == 0 {
		return false
	}
	last, _ := items[len(items)-1].(map[string]any)
	return last["type"] == "compaction_trigger"
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

func TestAstraCompactionKeepsSelectedEffort(t *testing.T) {
	writeTestCodexAuth(t)
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, body)
		if hasCompactionTrigger(body["input"]) {
			writeSSEItem(t, w, `{"type":"compaction","encrypted_content":"summary"}`, 200)
		} else {
			writeSSEItem(t, w, `{"type":"message","role":"assistant","content":[]}`, 250)
		}
	}))
	defer server.Close()
	sess := &session{
		Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef",
		CWD: t.TempDir(), RepoRoot: t.TempDir(), Model: "astra", Effort: "h", RequestEffort: "l",
		ContextTokens: 244_800,
		History: []json.RawMessage{
			json.RawMessage(`{"role":"user","content":"first"}`),
			json.RawMessage(`{"type":"configuration_update","reasoning":{"effort":"high"}}`),
			json.RawMessage(`{"role":"user","content":"second"}`),
		},
	}
	path := filepath.Join(t.TempDir(), "session.json")
	a := newAgent(&bytes.Buffer{}, &bytes.Buffer{}, path, time.Second, false, nil)
	a.client.endpoint = server.URL
	if _, err := a.runTurn(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d", len(requests))
	}
	if requests[0]["reasoning"].(map[string]any)["effort"] != "low" || requests[1]["reasoning"].(map[string]any)["effort"] != "high" {
		t.Fatalf("compaction changed effective effort: %v", requests)
	}
	for _, raw := range requests[1]["input"].([]any) {
		if raw.(map[string]any)["type"] == "configuration_update" {
			t.Fatal("stale update survived compaction")
		}
	}
	saved, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.RequestEffort != "h" || saved.Effort != "h" {
		t.Fatalf("saved effort = %#v", saved)
	}
	before := len(saved.History)
	if err := appendUserPrompt(saved, "third"); err != nil {
		t.Fatal(err)
	}
	if len(saved.History) != before+1 {
		t.Fatal("unchanged effort added an update after compaction")
	}
}
