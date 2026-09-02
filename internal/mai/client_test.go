package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadSSEStreamsTextAndCollectsItems(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		"",
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"two","name":"bash","arguments":"{}"}}`,
		"",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"secret"}}`,
		"",
		`data: {"type":"response.completed","response":{"status":"completed","error":null,"output":[],"usage":{"total_tokens":1234}}}`,
		"",
	}, "\n")
	var out bytes.Buffer
	client := &codexClient{stdout: &out}
	result, err := client.readSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" || !result.wrote || len(result.items) != 2 || result.totalTokens != 1234 {
		t.Fatalf("unexpected stream result: out=%q result=%#v", out.String(), result)
	}
	if !bytes.Contains(result.items[0], []byte(`"type":"reasoning"`)) {
		t.Fatalf("items were not sorted by output index: %s", result.items[0])
	}
}

func TestCompactAddsTriggerAndSuppressesOtherOutput(t *testing.T) {
	writeTestCodexAuth(t)
	var input []map[string]any
	var betaFeatures, turnMetadata string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		input = body.Input
		betaFeatures = r.Header.Get("x-codex-beta-features")
		turnMetadata = r.Header.Get("x-codex-turn-metadata")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"must not print"}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","encrypted_content":"reasoning"}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"type":"response.output_item.done","output_index":1,"item":{"type":"compaction","encrypted_content":"summary"}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"type":"response.completed","response":{"status":"completed","usage":{"total_tokens":250000}}}`)
		fmt.Fprintln(w)
	}))
	defer server.Close()

	var stdout bytes.Buffer
	client := newCodexClient(&stdout, time.Second)
	client.endpoint = server.URL
	item, err := client.compact(context.Background(), &session{
		ID: "session", Model: "luna", Effort: "m",
		History: []json.RawMessage{json.RawMessage(`{"role":"user","content":"hello"}`)},
	}, "instructions")
	if err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("compaction printed output: %q", stdout.String())
	}
	if len(input) != 2 || input[1]["type"] != "compaction_trigger" {
		t.Fatalf("compaction input = %#v", input)
	}
	if betaFeatures != "remote_compaction_v2" || !strings.Contains(turnMetadata, `"request_kind":"compaction"`) {
		t.Fatalf("compaction headers: features=%q metadata=%q", betaFeatures, turnMetadata)
	}
	if historyItemType(item) != "compaction" {
		t.Fatalf("compaction item = %s", item)
	}
}

func TestReadSSEUsesCompletedResponseOutputAsFallback(t *testing.T) {
	stream := "data: " + `{"type":"response.completed","response":{"status":"completed","output":[{"type":"message"}]}}` + "\n\n"
	client := &codexClient{stdout: &bytes.Buffer{}}
	result, err := client.readSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.items) != 1 || !bytes.Contains(result.items[0], []byte(`"type":"message"`)) {
		t.Fatalf("unexpected fallback output: %#v", result.items)
	}
}

func TestReadSSERejectsIncompleteAndFailedStreams(t *testing.T) {
	client := &codexClient{stdout: &bytes.Buffer{}}
	if _, err := client.readSSE(strings.NewReader("data: {}\n\n")); err == nil || !strings.Contains(err.Error(), "before response.completed") {
		t.Fatalf("unexpected incomplete-stream error: %v", err)
	}
	failed := "data: " + `{"type":"response.failed","response":{"status":"failed","error":{"message":"bad"}}}` + "\n\n"
	if _, err := client.readSSE(strings.NewReader(failed)); err == nil || !strings.Contains(err.Error(), "response failed") {
		t.Fatalf("unexpected failed-stream error: %v", err)
	}
}

func TestCodexClientReportsTimeoutWithNextStep(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()
	client := newCodexClient(&bytes.Buffer{}, 10*time.Millisecond)
	client.endpoint = server.URL
	_, err := client.streamWithCredentials(context.Background(), &session{
		ID: "session", Model: "luna", Effort: "max",
	}, "instructions", credentials{AccessToken: "token", AccountID: "account"})
	if err == nil || !strings.Contains(err.Error(), "use --timeout") {
		t.Fatalf("unexpected error: %v", err)
	}
}
