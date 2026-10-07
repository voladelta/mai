package mai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSessionPathsUsesPrivatePermissionsAndIgnoresState(t *testing.T) {
	paths := projectSessionPaths(t.TempDir())
	if err := prepareSessionPaths(paths); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(paths.dir)
	if err != nil {
		t.Fatal(err)
	}
	ignore, err := os.ReadFile(filepath.Join(paths.dir, ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 || string(ignore) != "*\n" {
		t.Fatalf("unexpected state setup: mode=%o ignore=%q", dirInfo.Mode().Perm(), ignore)
	}
}

func TestPrepareSessionPathsRejectsMaiSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	paths := projectSessionPaths(root)
	if err := os.Symlink(outside, paths.dir); err != nil {
		t.Fatal(err)
	}
	if err := prepareSessionPaths(paths); err == nil {
		t.Fatal(".mai symlink was accepted")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside directory was changed: entries=%v err=%v", entries, err)
	}
}

func TestCurrentSessionRoundTripRejectsInvalidID(t *testing.T) {
	paths := projectSessionPaths(t.TempDir())
	if err := prepareSessionPaths(paths); err != nil {
		t.Fatal(err)
	}
	id := "01234567-89ab-cdef-0123-456789abcdef"
	if err := saveCurrentSession(paths, id); err != nil {
		t.Fatal(err)
	}
	got, err := loadCurrentSessionID(paths)
	if err != nil || got != id {
		t.Fatalf("current session = %q, %v", got, err)
	}
	if err := atomicWriteFile(paths.current, []byte("../../outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCurrentSessionID(paths); err == nil {
		t.Fatal("invalid current session ID was accepted")
	}
}

func TestLoadSessionEstimatesTokensForOlderState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.json")
	sess := session{
		Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef",
		CWD: root, RepoRoot: root, Model: "deepseek-flash", Effort: "h",
		History: []json.RawMessage{json.RawMessage(`{"role":"user","content":"existing history"}`)},
	}
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ContextTokens == 0 {
		t.Fatal("older session history did not receive a token estimate")
	}
}

func TestLoadSessionRejectsInvalidState(t *testing.T) {
	root := t.TempDir()
	for name, test := range map[string]struct {
		mutate  func(*session)
		wantErr string
	}{
		"unsupported version": {mutate: func(s *session) { s.Version = stateVersion - 1 }, wantErr: "incomplete or unsupported"},
		"invalid id":          {mutate: func(s *session) { s.ID = "not-an-id" }, wantErr: "incomplete or unsupported"},
		"missing cwd":         {mutate: func(s *session) { s.CWD = "" }, wantErr: "incomplete or unsupported"},
		"negative tokens":     {mutate: func(s *session) { s.ContextTokens = -1 }, wantErr: "incomplete or unsupported"},
		"negative skip":       {mutate: func(s *session) { s.TranscriptSkip = -1 }, wantErr: "invalid transcript position"},
		"skip beyond history": {mutate: func(s *session) { s.TranscriptSkip = 1 }, wantErr: "invalid transcript position"},
		"negative transcript": {mutate: func(s *session) { s.TranscriptEnd = -1 }, wantErr: "invalid transcript position"},
		"invalid model":       {mutate: func(s *session) { s.Model = "two words" }, wantErr: `invalid model "two words"`},
		"empty model":         {mutate: func(s *session) { s.Model = "" }, wantErr: `invalid model ""`},
		"invalid effort":      {mutate: func(s *session) { s.Effort = "bogus" }, wantErr: `invalid effort "bogus"`},
		"missing transcript":  {mutate: func(s *session) { s.TranscriptEnd = 10 }, wantErr: "open saved transcript"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.json")
			sess := session{
				Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef",
				CWD: root, RepoRoot: root, Model: "deepseek-flash", Effort: "h",
			}
			test.mutate(&sess)
			if err := saveJSON(path, sess); err != nil {
				t.Fatal(err)
			}
			if _, err := loadSession(path); err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("loadSession error = %v, want %q", err, test.wantErr)
			}
		})
	}
}

func TestLoadSessionChecksSavedTranscriptPosition(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.json")
	sess := session{
		Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef",
		CWD: root, RepoRoot: root, Model: "deepseek-flash", Effort: "h", TranscriptEnd: 100,
	}
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcriptPath(path), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSession(path); err == nil || !strings.Contains(err.Error(), "shorter than its committed position") {
		t.Fatalf("short transcript error = %v", err)
	}
	if err := os.WriteFile(transcriptPath(path), []byte(strings.Repeat("x", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil || loaded.TranscriptEnd != 100 {
		t.Fatalf("complete transcript rejected: %#v, %v", loaded, err)
	}
}

func TestEstimateHistoryItemTokensCountsImageAsImageAndKeepsText(t *testing.T) {
	imageURL := "data:image/png;base64," + strings.Repeat("A", 1<<20)
	imageOutput := imageContentToolOutput("metadata", imageURL)
	item, err := json.Marshal(struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}{Type: "function_call_output", CallID: "image", Output: imageOutput})
	if err != nil {
		t.Fatal(err)
	}

	imageTokens := estimateHistoryItemTokens(item)
	if imageTokens < 1800 || imageTokens > 2000 {
		t.Fatalf("image estimate = %d tokens, want an image-sized estimate", imageTokens)
	}

	withText := imageContentToolOutput(strings.Repeat("x", 4000), imageURL)
	textItem, err := json.Marshal(struct {
		Type   string          `json:"type"`
		CallID string          `json:"call_id"`
		Output json.RawMessage `json:"output"`
	}{Type: "function_call_output", CallID: "image", Output: withText})
	if err != nil {
		t.Fatal(err)
	}
	if delta := estimateHistoryItemTokens(textItem) - imageTokens; delta < 900 || delta > 1100 {
		t.Fatalf("text increased image estimate by %d tokens, want about 1000", delta)
	}

	message, err := json.Marshal(struct {
		Type    string          `json:"type"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{Type: "message", Role: "user", Content: imageOutput})
	if err != nil {
		t.Fatal(err)
	}
	if got := estimateHistoryItemTokens(message); got < 1800 || got > 2000 {
		t.Fatalf("message image estimate = %d tokens, want an image-sized estimate", got)
	}

	plainText := json.RawMessage(`{"type":"function_call_output","output":"` + strings.Repeat("A", 1<<20) + `"}`)
	if got := estimateHistoryItemTokens(plainText); got < 250_000 {
		t.Fatalf("ordinary text estimate = %d tokens, want payload bytes counted", got)
	}
}

func TestSessionLockRejectsConcurrentOwner(t *testing.T) {
	paths := projectSessionPaths(t.TempDir())
	if err := prepareSessionPaths(paths); err != nil {
		t.Fatal(err)
	}
	id := "01234567-89ab-cdef-0123-456789abcdef"
	first, err := acquireSessionLock(paths, id)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(paths.dir, "locks", id+".lock")); err != nil {
		first.Close()
		t.Fatalf("session lock missing from locks directory: %v", err)
	}

	if _, err := acquireSessionLock(paths, id); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second lock error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := acquireSessionLock(paths, id)
	if err != nil {
		t.Fatalf("lock remained held after close: %v", err)
	}
	second.Close()
}

func TestRepairInterruptedToolCalls(t *testing.T) {
	sess := &session{History: []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"done","name":"bash","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"done","output":"ok"}`),
		json.RawMessage(`{"type":"function_call","call_id":"pending","name":"write","arguments":"{}"}`),
	}}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	if len(sess.History) != 4 {
		t.Fatalf("history length = %d, want 4", len(sess.History))
	}
	var output struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(sess.History[3], &output); err != nil {
		t.Fatal(err)
	}
	if output.Type != "function_call_output" || output.CallID != "pending" || output.Output == "" {
		t.Fatalf("unexpected repair output: %#v", output)
	}
	var recovery struct {
		OK          *bool  `json:"ok"`
		Outcome     string `json:"outcome"`
		Error       string `json:"error"`
		Instruction string `json:"instruction"`
	}
	if err := json.Unmarshal([]byte(output.Output), &recovery); err != nil {
		t.Fatal(err)
	}
	if recovery.OK != nil || recovery.Outcome != "unknown" {
		t.Fatalf("recovery result does not state an unknown outcome: %#v", recovery)
	}
	if !strings.Contains(recovery.Error, "tool outcome is unknown") ||
		!strings.Contains(recovery.Instruction, "reconcile") {
		t.Fatalf("unsafe write recovery guidance: %#v", recovery)
	}

	repaired := mustJSON(t, sess)
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}

	if string(mustJSON(t, sess)) != string(repaired) {
		t.Fatal("repairing completed recovery changed the session")
	}
}

func TestRepairInterruptedUnknownToolRequiresInspection(t *testing.T) {
	sess := &session{History: []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"pending","name":"retired_tool","arguments":"{\"task\":\"apply fix\"}"}`),
	}}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	var output struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(sess.History[1], &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Output, `"outcome":"unknown"`) || !strings.Contains(output.Output, "Inspect the relevant state") {
		t.Fatalf("unsafe tool recovery: %s", output.Output)
	}
}

func TestRepairInterruptedBashRequiresConfirmationBeforeUnsafeRetry(t *testing.T) {
	sess := &session{History: []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"pending","name":"bash","arguments":"{\"command\":\"deploy\"}"}`),
	}}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	var output struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(sess.History[1], &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Output, `"outcome":"unknown"`) ||
		!strings.Contains(output.Output, "non-idempotent effects without user confirmation") {
		t.Fatalf("unsafe Bash recovery output: %s", output.Output)
	}
}

func TestInterruptedToolRecoveryPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	sess := &session{
		Version: stateVersion, ID: "01234567-89ab-cdef-0123-456789abcdef", CWD: t.TempDir(), RepoRoot: t.TempDir(),
		Model: "deepseek-flash", Effort: "max",
		History: []json.RawMessage{
			json.RawMessage(`{"type":"function_call","call_id":"pending","name":"write","arguments":"{}"}`),
		},
	}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	if err := saveJSON(path, sess); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(loaded.History[1], &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Output, `"outcome":"unknown"`) ||
		!strings.Contains(output.Output, "reconcile the intended change") {
		t.Fatalf("persisted recovery output is incomplete: %s", output.Output)
	}
}
