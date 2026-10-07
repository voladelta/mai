package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type checkpointStub struct {
	sources []string
	reply   string
	err     error
}

func (s *checkpointStub) stream(context.Context, *session, string) (streamResult, error) {
	return streamResult{}, errors.New("unexpected generation")
}
func (s *checkpointStub) summarize(_ context.Context, _ *session, source string) (string, *tokenUsage, error) {
	s.sources = append(s.sources, source)
	return s.reply, &tokenUsage{TotalTokens: 12}, s.err
}

func portableFixture(t *testing.T) *session {
	t.Helper()
	sess := contextEditFixture(t)
	_ = appendUserPrompt(sess, "Correction: batch is 128; preserve UNKNOWN.")
	sess.ContextTokens = modelContextWindow
	return sess
}

func TestPortableCheckpointArchivesOriginalAndRetainsActiveTurn(t *testing.T) {
	sess := portableFixture(t)
	before, _ := json.Marshal(sess)
	last := append(json.RawMessage(nil), sess.History[len(sess.History)-1]...)
	backend := &checkpointStub{reply: "Goal: batch 128. Deploy UNKNOWN. Retrieve original release with call ID call-a."}
	a := newAgent(io.Discard, io.Discard, filepath.Join(t.TempDir(), "session.json"), time.Second, false)
	a.backend = backend
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	// The last item is the compaction notice; the active turn precedes it.
	if len(backend.sources) != 1 || !bytes.Equal(last, sess.History[len(sess.History)-2]) || len(sess.ContextEdits) != 0 {
		t.Fatal("active turn changed or summary not applied")
	}
	for _, item := range sess.History {
		if historyItemType(item) == "compaction" {
			t.Fatal("opaque checkpoint persisted")
		}
	}
	saved, err := loadSession(a.sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TranscriptEnd <= 0 || saved.ContextTokens >= modelContextWindow {
		t.Fatal("checkpoint or archive missing after resume")
	}
	var original session
	_ = json.Unmarshal(before, &original)
	entries, err := visibleHistoryEntries(original.History)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(transcriptPath(a.sessionPath))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var archived []transcriptEntry
	decoder := json.NewDecoder(file)
	for decoder.More() {
		var entry transcriptEntry
		if err := decoder.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		archived = append(archived, entry)
	}
	if len(archived) != len(entries) {
		t.Fatalf("original archive lost entries: %d vs %d", len(archived), len(entries))
	}
	if !bytes.Equal(mustJSONValue(t, archived), mustJSONValue(t, entries)) {
		t.Fatal("original archive content changed")
	}
}

func TestPortableCheckpointFailureLeavesSessionUnchanged(t *testing.T) {
	for _, reply := range []string{"", strings.Repeat("x", (16<<10)+1)} {
		sess := portableFixture(t)
		before, _ := json.Marshal(sess)
		a := newAgent(io.Discard, io.Discard, "", time.Second, false)
		a.backend = &checkpointStub{reply: reply}
		if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err == nil {
			t.Fatal("invalid summary accepted")
		}
		after, _ := json.Marshal(sess)
		if !bytes.Equal(before, after) {
			t.Fatal("failed compaction changed authoritative state")
		}
	}
}

func TestPortableCheckpointPreservesToolOutputFormatting(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "stdout lines",
			output: `{"stdout":"first\nsecond\n","stderr":"warning","exit_code":0}`,
			want:   "{\"exit_code\":0,\"stderr\":\"warning\",\"stdout\":\"[stdout follows as line records]\"}\nfirst\nsecond\n",
		},
		{
			name:   "JSON without stdout",
			output: `{"ok":false,"details":{"count":0}}`,
			want:   "{\n \"details\": {\n  \"count\": 0\n },\n \"ok\": false\n}",
		},
		{
			name:   "JSON array",
			output: `[false,0]`,
			want:   "[\n false,\n 0\n]",
		},
		{
			name:   "plain text",
			output: "plain tool result",
			want:   "plain tool result",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sess := portableFixture(t)
			sess.History[3] = mustJSONValue(t, map[string]any{
				"type": "function_call_output", "call_id": "logs", "output": test.output,
			})
			backend := &checkpointStub{reply: "checkpoint"}

			_, _, _, err := portableHistory(context.Background(), sess, backend)
			if err != nil {
				t.Fatal(err)
			}
			if len(backend.sources) != 1 || !strings.Contains(backend.sources[0], test.want) {
				t.Fatalf("tool output formatting changed: want %q in sources %q", test.want, backend.sources)
			}
		})
	}
}

func TestPortableCheckpointTriggersAtEightyPercent(t *testing.T) {
	for _, window := range []int64{modelContextWindow, 32768} {
		for _, atThreshold := range []bool{false, true} {
			t.Run(fmt.Sprintf("window=%d/at_threshold=%t", window, atThreshold), func(t *testing.T) {
				sess := portableFixture(t)
				sess.ContextTokens = window * 80 / 100
				if !atThreshold {
					sess.ContextTokens--
				}
				before := mustJSON(t, sess)
				backend := &checkpointStub{reply: "Checkpoint retained."}
				a := newAgent(io.Discard, io.Discard, "", time.Second, false)
				a.backend = backend
				if window != modelContextWindow {
					a.contextWindow = window
				}

				if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
					t.Fatal(err)
				}

				if atThreshold {
					if len(backend.sources) == 0 || sess.ContextTokens >= window*80/100 {
						t.Fatal("context was not compacted at 80 percent")
					}
				} else if len(backend.sources) != 0 || !bytes.Equal(before, mustJSON(t, sess)) {
					t.Fatal("context changed below 80 percent")
				}
			})
		}
	}
}

func TestPortableCheckpointCompactsLargeTextHistory(t *testing.T) {
	sess := contextEditFixture(t)
	var log strings.Builder
	padding := strings.Repeat("x", 28)
	for i := 0; i < 90000; i++ {
		fmt.Fprintf(&log, "%06d %s\n", i, padding)
	}
	log.WriteString("FINAL-LARGE-HISTORY-ANCHOR\n")
	appendBudgetLog(t, sess, "large-history", log.String())
	if err := appendUserPrompt(sess, "Continue the active task."); err != nil {
		t.Fatal(err)
	}
	sess.ContextTokens = estimateHistoryTokens(sess.History)
	if sess.ContextTokens < modelContextWindow*80/100 || sess.ContextTokens >= modelContextWindow*90/100 {
		t.Fatalf("fixture is outside the 80-to-90 percent range: %d tokens", sess.ContextTokens)
	}
	activeTurn := append(json.RawMessage(nil), sess.History[len(sess.History)-1]...)
	backend := &checkpointStub{reply: "Large history checkpoint retained."}
	a := newAgent(io.Discard, io.Discard, "", time.Second, false)
	a.backend = backend

	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}

	if len(backend.sources) < 2 || sess.ContextTokens >= modelContextWindow*80/100 {
		t.Fatal("large text history was not folded and compacted")
	}
	if !bytes.Equal(activeTurn, sess.History[len(sess.History)-2]) {
		t.Fatal("compaction changed the active turn")
	}
	var folded strings.Builder
	for _, source := range backend.sources {
		if len(source) > (96<<10)+(16<<10)+128 {
			t.Fatalf("summary request exceeds the chunk and checkpoint bounds: %d bytes", len(source))
		}
		_, records, found := strings.Cut(source, "\nNext historical records:\n")
		if !found {
			t.Fatal("summary request has no historical records")
		}
		folded.WriteString(records)
	}
	lines := strings.Split(folded.String(), "\n")
	seen := 0
	for _, line := range lines {
		if !strings.HasSuffix(line, " "+padding) {
			continue
		}
		if want := fmt.Sprintf("%06d %s", seen, padding); line != want {
			t.Fatalf("historical records were lost or reordered: got %q, want %q", line, want)
		}
		seen++
	}
	if seen != 90000 || !strings.Contains(backend.sources[len(backend.sources)-1], "FINAL-LARGE-HISTORY-ANCHOR") {
		t.Fatalf("chunk folding lost historical records: retained %d of 90000", seen)
	}
	if !strings.Contains(backend.sources[1], backend.reply) {
		t.Fatal("chunk folding lost the previous checkpoint")
	}
}

func TestPortableCheckpointRefusesPendingCallsAndOpaqueState(t *testing.T) {
	for _, item := range []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"pending","name":"bash","arguments":"{}"}`),
		json.RawMessage(`{"type":"compaction","encrypted_content":"opaque"}`),
		json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"caption"},{"type":"input_image","image_url":"hidden"}]}`),
	} {
		sess := portableFixture(t)
		sess.History = append(sess.History[:len(sess.History)-1], item)
		_ = appendUserPrompt(sess, "next")
		backend := &checkpointStub{reply: "summary"}
		if _, _, _, err := portableHistory(context.Background(), sess, backend); err == nil || len(backend.sources) != 0 {
			t.Fatal("unsafe prefix reached generation")
		}
	}
}

func TestPortableChunksPreserveMultibyteText(t *testing.T) {
	sess := portableFixture(t)
	_ = appendUserPrompt(sess, "a"+strings.Repeat("界", 40000))
	_ = appendUserPrompt(sess, "active")
	backend := &checkpointStub{reply: "checkpoint retained"}
	if _, _, _, err := portableHistory(context.Background(), sess, backend); err != nil {
		t.Fatal(err)
	}
	if len(backend.sources) < 2 {
		t.Fatal("fixture did not cross chunk boundary")
	}
	for _, source := range backend.sources {
		if !utf8.ValidString(source) {
			t.Fatal("chunk split a UTF-8 character")
		}
	}
}

func TestCompactionReportNotifiesModelAndStaysOutOfTranscript(t *testing.T) {
	dir := t.TempDir()
	sess := portableFixture(t)
	var events bytes.Buffer
	a := newAgent(io.Discard, io.Discard, filepath.Join(dir, "session.json"), time.Second, false)
	a.events = &events
	a.backend = &checkpointStub{reply: "Goal: batch 128. Retrieve the original with call ID logs."}
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	notice := sess.History[len(sess.History)-1]
	var item struct {
		Role    string `json:"role"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(notice, &item); err != nil || item.Role != "developer" || len(item.Content) == 0 {
		t.Fatalf("last history item is not the developer notice: %s", notice)
	}
	text := item.Content[0].Text
	for _, want := range []string{"replaced 4 earlier records", "bash=1", "logs", "mai.history", "Verify exact facts"} {
		if !strings.Contains(text, want) {
			t.Fatalf("compaction notice missing %q:\n%s", want, text)
		}
	}
	if len(text) >= 4<<10 {
		t.Fatalf("notice exceeds 4 KiB: %d", len(text))
	}
	if sess.TranscriptSkip != len(sess.History) {
		t.Fatalf("TranscriptSkip = %d, want %d", sess.TranscriptSkip, len(sess.History))
	}
	var sawEvent bool
	for _, line := range strings.Split(strings.TrimSpace(events.String()), "\n") {
		var event struct {
			Type    string           `json:"type"`
			Changes []map[string]any `json:"changes"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil || event.Type != "compaction.completed" {
			continue
		}
		sawEvent = true
		change := event.Changes[0]
		if len(event.Changes) != 1 || change["kind"] != "compacted" ||
			change["source"] != "conversation history" || change["reason"] != "context budget reached" ||
			change["records"] != float64(4) {
			t.Fatalf("unexpected changes field: %v", event.Changes)
		}
		calls, _ := change["tool_calls"].(map[string]any)
		if calls["bash"] != float64(1) {
			t.Fatalf("tool call counts missing: %v", change)
		}
		ids, _ := change["call_ids"].([]any)
		if len(ids) != 1 || ids[0] != "logs" {
			t.Fatalf("call IDs missing: %v", change)
		}
	}
	if !sawEvent {
		t.Fatal("compaction.completed event missing")
	}
	transcript, err := os.ReadFile(transcriptPath(a.sessionPath))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(transcript, []byte("Context checkpoint replaced")) {
		t.Fatal("compaction notice reached the transcript archive")
	}
	// A second compaction must not duplicate the notice.
	_ = appendUserPrompt(sess, "Follow-up after first compaction.")
	sess.ContextTokens = modelContextWindow
	a.backend = &checkpointStub{reply: "Second checkpoint."}
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, raw := range sess.History {
		if isCompactionNotice(raw) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("compaction notice duplicated: %d", count)
	}
}
