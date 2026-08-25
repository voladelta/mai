package mai

import (
	"bytes"
	"strings"
	"testing"
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
