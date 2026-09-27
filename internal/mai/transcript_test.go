package mai

import (
	"context"
	"encoding/json"
	"os"
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

func TestPythonHistoryIncludesVisibleItemsAndExcludesOpaqueItems(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.History = []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"The passphrase is copper heron"}]}`),
		json.RawMessage(`{"type":"reasoning","encrypted_content":"private-reasoning-marker"}`),
		json.RawMessage(`{"type":"compaction","encrypted_content":"private-compaction-marker"}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"I found the date"}]}`),
		json.RawMessage(`{"type":"function_call","name":"bash","call_id":"call-1","arguments":"{\"command\":\"date\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"2026-09-27"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"image-1","output":[{"type":"input_text","text":"image is 10 by 10"},{"type":"input_image","image_url":"data:image/png;base64,private-image-marker"}]}`),
	}
	got := pythonCell(t, a, sess, `found = await mai.history("copper heron")
assert found["total"] == 1
assert found["matches"][0]["kind"] == "user"
assert "copper heron" in found["matches"][0]["text"]
found["matches"][0]["text"] = "changed"
assert "copper heron" in (await mai.history("copper heron"))["matches"][0]["text"]
assert (await mai.history("private-reasoning-marker"))["total"] == 0
assert (await mai.history("private-compaction-marker"))["total"] == 0
assert (await mai.history("private-image-marker"))["total"] == 0
assert (await mai.history("10 by 10"))["matches"][0]["kind"] == "tool_result"
assert (await mai.history("2026-09-27"))["matches"][0]["kind"] == "tool_result"`)
	if !got.OK {
		t.Fatalf("visible history search failed: %#v", got)
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

func TestLoadSessionRejectsMissingCommittedTranscript(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.json")
	sess := session{
		Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef",
		CWD: root, RepoRoot: root, Model: "luna", Effort: "m",
		TranscriptEnd: 1,
	}
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(path); err == nil {
		t.Fatal("missing committed transcript was accepted")
	}
	if err := os.WriteFile(transcriptPath(path), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sess.TranscriptEnd = 2
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(path); err == nil {
		t.Fatal("short committed transcript was accepted")
	}
}

func TestPythonHistoryExcludesActiveCellCode(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.History = []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"python","call_id":"active","arguments":"{\"code\":\"await mai.history('absent-needle')\"}"}`),
	}
	output := a.executePython(context.Background(), sess, `{"code":"assert (await mai.history('absent-needle'))['total'] == 0"}`, "active")
	var payload string
	if err := json.Unmarshal(output, &payload); err != nil {
		t.Fatal(err)
	}
	var result pythonResult
	if err := json.Unmarshal([]byte(payload), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("active Python code matched its own query: %#v", result)
	}
}

func TestPythonHistoryPagesPastEarlierMatches(t *testing.T) {
	a, sess := pythonTestAgent(t)
	for i := 0; i < 25; i++ {
		sess.History = append(sess.History, json.RawMessage(`{"role":"user","content":"needle earlier"}`))
	}
	sess.History = append(sess.History, json.RawMessage(`{"role":"user","content":"needle final fact is 42"}`))

	got := pythonCell(t, a, sess, `page = await mai.history("needle")
assert len(page["matches"]) == 20
assert page["total"] == 26
seen = list(page["matches"])
while page.get("next") is not None:
    page = await mai.history("needle", start=page["next"])
    seen.extend(page["matches"])
assert len(seen) == 26
assert seen[-1]["index"] == 25
assert "final fact is 42" in seen[-1]["text"]`)
	if !got.OK {
		t.Fatalf("later match was not reachable: %#v", got)
	}
}
