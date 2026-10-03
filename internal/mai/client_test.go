package mai

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestFileToolsDescribeRepositoryRootPaths(t *testing.T) {
	found := make(map[string]bool)
	for _, definition := range toolDefinitions() {
		name := definition["name"].(string)
		if name == "apply_patch" {
			t.Fatal("retired patch tool is advertised")
		}
		if name != "read" && name != "write" && name != "edit" {
			continue
		}
		if !strings.Contains(definition["description"].(string), "Paths are relative to the repository root.") {
			t.Fatalf("%s path base is undocumented: %s", name, definition["description"])
		}
		found[name] = true
	}
	if len(found) != 3 {
		t.Fatalf("file tools missing: %v", found)
	}
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
