package mai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// Opt-in eval: MAI_LIVE_HISTORY_EVAL=1 go test -v ./internal/mai -run TestLiveHistoryRecall -count=1
func TestLiveHistoryRecall(t *testing.T) {
	if os.Getenv("MAI_LIVE_HISTORY_EVAL") != "1" {
		t.Skip("set MAI_LIVE_HISTORY_EVAL=1 to run live model requests")
	}

	const attempts = 3
	var withHistory, withoutHistory int
	for attempt := 1; attempt <= attempts; attempt++ {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			t.Fatal(err)
		}
		code := "CR-" + strings.ToUpper(hex.EncodeToString(nonce[:]))
		ticket := "H-" + string(rune('0'+attempt))
		prompt := "Earlier in this task, the user gave the recovery code for ticket " + ticket + ". " +
			"Return the exact code and nothing else. If it is unavailable in task history, return UNKNOWN."
		oldMessage := "The recovery code for ticket " + ticket + " is " + code + "."

		for _, arm := range []struct {
			name       string
			transcript []transcriptEntry
		}{
			{name: "without-history"},
			{name: "with-history", transcript: []transcriptEntry{{Kind: "user", Text: oldMessage}}},
		} {
			answer, historyCalls, err := runLiveRecallAttempt(t, prompt, arm.transcript)
			if err != nil {
				t.Errorf("attempt %d %s: %v", attempt, arm.name, err)
				continue
			}
			correct := answer == code
			t.Logf("attempt=%d arm=%s exact_recall=%t history_calls=%d answer=%q", attempt, arm.name, correct, historyCalls, answer)
			if arm.name == "with-history" && correct {
				withHistory++
			}
			if arm.name == "without-history" && correct {
				withoutHistory++
			}
		}
	}

	t.Logf("exact recall: with history %d/%d, without history %d/%d", withHistory, attempts, withoutHistory, attempts)
	if withHistory <= withoutHistory {
		t.Errorf("searchable history did not improve exact recall in this sample")
	}
}

func runLiveRecallAttempt(t *testing.T, prompt string, transcript []transcriptEntry) (string, int, error) {
	t.Helper()
	id, err := newSessionID()
	if err != nil {
		return "", 0, err
	}
	workspace := t.TempDir()
	sess := &session{
		Version: stateVersion, ID: id, CWD: workspace, RepoRoot: workspace,
		Model: "luna", Effort: "m", RequestEffort: "m", Transcript: transcript,
	}
	if err := appendUserPrompt(sess, prompt); err != nil {
		return "", 0, err
	}

	a := newAgent(io.Discard, io.Discard, "", 2*time.Minute, false, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := a.run(ctx, sess, prompt); err != nil {
		return "", 0, err
	}

	var answer string
	var historyCalls int
	for _, raw := range sess.History {
		var call functionCall
		if json.Unmarshal(raw, &call) == nil && call.Type == "function_call" &&
			call.Name == "python" && strings.Contains(call.Arguments, "mai.history(") {
			historyCalls++
		}
		entry, visible, err := visibleTranscriptEntry(raw)
		if err != nil {
			return "", historyCalls, err
		}
		if visible && entry.Kind == "assistant" {
			answer = strings.TrimSpace(entry.Text)
		}
	}
	return answer, historyCalls, nil
}
