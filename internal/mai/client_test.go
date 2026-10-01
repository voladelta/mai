package mai

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestApplyPatchToolDescribesRepositoryRootPaths(t *testing.T) {
	for _, definition := range toolDefinitions() {
		if definition["name"] != "apply_patch" {
			continue
		}
		if !strings.Contains(definition["description"].(string), "Paths are relative to the repository root.") {
			t.Fatalf("apply_patch path base is undocumented: %s", definition["description"])
		}
		return
	}
	t.Fatal("apply_patch tool is missing")
}

func TestReadSSEStreamsTextAndCollectsItems(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"hello"}`,
		"",
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","call_id":"two","name":"bash","arguments":"{}"}}`,
		"",
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","content":[{"type":"reasoning_text","text":"secret"}]}}`,
		"",
		`data: {"type":"response.completed","response":{"status":"completed","error":null,"output":[],"usage":{"total_tokens":1234}}}`,
		"",
	}, "\n")
	var out bytes.Buffer
	client := &responseStream{stdout: &out}
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

func TestReadSSEPreservesOptionalUsageFields(t *testing.T) {
	stream := `data: {"type":"response.completed","response":{"status":"completed","output":[],"usage":{"total_tokens":456,"input_tokens":400,"output_tokens":56,"input_tokens_details":{"cached_tokens":0}}}}` + "\n\n"
	result, err := (&responseStream{stdout: io.Discard}).readSSE(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if result.usage == nil || result.usage.InputTokens == nil || *result.usage.InputTokens != 400 || result.usage.OutputTokens == nil || *result.usage.OutputTokens != 56 || result.usage.InputTokensDetails == nil || result.usage.InputTokensDetails.CachedTokens == nil || *result.usage.InputTokensDetails.CachedTokens != 0 {
		t.Fatalf("usage lost available values: %#v", result.usage)
	}
}
