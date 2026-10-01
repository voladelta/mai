package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	a := newAgent(io.Discard, io.Discard, filepath.Join(t.TempDir(), "session.json"), time.Second, false, nil)
	a.backend = backend
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	if len(backend.sources) != 1 || !bytes.Equal(last, sess.History[len(sess.History)-1]) || len(sess.ContextEdits) != 0 {
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
		a := newAgent(io.Discard, io.Discard, "", time.Second, false, nil)
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
		if _, _, err := portableHistory(context.Background(), sess, backend); err == nil || len(backend.sources) != 0 {
			t.Fatal("unsafe prefix reached generation")
		}
	}
}

func TestPortableCheckpointFoldsAllUniqueInputInOrder(t *testing.T) {
	sess := portableFixture(t)
	var unique strings.Builder
	for i := 0; i < 6000; i++ {
		unique.WriteString(strings.Repeat("x", 25))
		unique.WriteString(time.Duration(i).String())
		unique.WriteByte('\n')
	}
	_ = appendUserPrompt(sess, unique.String()+"\nFINAL-ANCHOR")
	_ = appendUserPrompt(sess, "active turn")
	backend := &checkpointStub{reply: "checkpoint retained"}
	if _, _, err := portableHistory(context.Background(), sess, backend); err != nil {
		t.Fatal(err)
	}
	if len(backend.sources) < 2 || !strings.Contains(backend.sources[len(backend.sources)-1], "FINAL-ANCHOR") || !strings.Contains(backend.sources[1], "checkpoint retained") {
		t.Fatal("chunk folding discarded history or prior checkpoint")
	}
}

func TestPortableChunksPreserveMultibyteText(t *testing.T) {
	sess := portableFixture(t)
	_ = appendUserPrompt(sess, "a"+strings.Repeat("界", 40000))
	_ = appendUserPrompt(sess, "active")
	backend := &checkpointStub{reply: "checkpoint retained"}
	if _, _, err := portableHistory(context.Background(), sess, backend); err != nil {
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
