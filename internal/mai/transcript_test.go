package mai

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestVisibleTranscriptEntryExtractsAllowedTextParts(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		kind string
		text string
	}{
		{
			name: "message",
			raw:  json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first"},{"type":"input_image","image_url":"hidden"},{"type":"input_text","text":"second"}]}`),
			kind: "assistant",
			text: "first\nsecond",
		},
		{
			name: "tool result",
			raw:  json.RawMessage(`{"type":"function_call_output","call_id":"tool","output":[{"type":"input_text","text":"visible"},{"type":"output_text","text":"excluded"}]}`),
			kind: "tool_result",
			text: "visible",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entry, visible, err := visibleTranscriptEntry(test.raw)
			if err != nil {
				t.Fatal(err)
			}
			if !visible || entry.Kind != test.kind || entry.Text != test.text {
				t.Fatalf("entry = %#v, visible = %t", entry, visible)
			}
		})
	}
}

func TestTranscriptSearchBoundsLargeResult(t *testing.T) {
	sess := &session{History: []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"` + strings.Repeat("x", 100_000) + `target` + strings.Repeat("y", 100_000) + `"}`),
	}}
	result := searchTranscript(sess, json.RawMessage(`{"query":"target","limit":20}`), "")
	if len(result) > 10_000 || !strings.Contains(string(result), "target") {
		t.Fatalf("search result size=%d, result=%s", len(result), result)
	}
}

func TestTranscriptIgnoresUncommittedTailAndTruncatesBeforeAppend(t *testing.T) {
	path := transcriptPath(filepath.Join(t.TempDir(), "session.json"))
	end, err := appendTranscript(path, 0, []transcriptEntry{{Kind: "user", Text: "committed"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := appendTranscript(path, end, []transcriptEntry{{Kind: "user", Text: "uncommitted"}}); err != nil {
		t.Fatal(err)
	}

	sess := &session{TranscriptEnd: end, transcriptPath: path}
	result := searchTranscript(sess, json.RawMessage(`{"query":"uncommitted","limit":20}`), "")
	if !strings.Contains(string(result), `"total":0`) {
		t.Fatalf("uncommitted tail appeared in search: %s", result)
	}

	nextEnd, err := appendTranscript(path, end, []transcriptEntry{{Kind: "user", Text: "replacement"}})
	if err != nil {
		t.Fatal(err)
	}
	sess.TranscriptEnd = nextEnd
	result = searchTranscript(sess, json.RawMessage(`{"query":"uncommitted","limit":20}`), "")
	if !strings.Contains(string(result), `"total":0`) {
		t.Fatalf("uncommitted tail survived retry: %s", result)
	}
	result = searchTranscript(sess, json.RawMessage(`{"query":"replacement","limit":20}`), "")
	if !strings.Contains(string(result), `"total":1`) {
		t.Fatalf("replacement was not searchable: %s", result)
	}
}

func TestTranscriptExcerptKeepsMatchAfterCaseFoldingChangesByteLength(t *testing.T) {
	// U+212A KELVIN SIGN is three bytes and lowercases to the one-byte "k".
	sess := &session{History: []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"` + strings.Repeat("K", 3000) + `target"}`),
	}}
	var result struct {
		Matches []struct {
			Text string `json:"text"`
		} `json:"matches"`
	}
	if err := json.Unmarshal(searchTranscript(sess, json.RawMessage(`{"query":"TARGET","limit":1}`), ""), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || !strings.Contains(result.Matches[0].Text, "target") {
		t.Fatalf("excerpt lost the match: %#v", result)
	}
}
