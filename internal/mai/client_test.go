package mai

import (
	"bytes"
	"context"
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
		`data: {"type":"response.completed","response":{"status":"completed","error":null,"output":[]}}`,
		"",
	}, "\n")
	var out bytes.Buffer
	client := &codexClient{stdout: &out}
	result, err := client.readSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" || !result.wrote || len(result.items) != 2 {
		t.Fatalf("unexpected stream result: out=%q result=%#v", out.String(), result)
	}
	if !bytes.Contains(result.items[0], []byte(`"type":"reasoning"`)) {
		t.Fatalf("items were not sorted by output index: %s", result.items[0])
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
