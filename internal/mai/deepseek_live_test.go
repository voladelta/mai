package mai

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Opt-in paid API conformance, not a coding-performance benchmark.
func TestLiveDeepSeekResponses(t *testing.T) {
	if os.Getenv("MAI_LIVE_DEEPSEEK_RESPONSES") != "1" {
		t.Skip("set MAI_LIVE_DEEPSEEK_RESPONSES=1")
	}
	requireDeepSeekLiveProvider(t)
	t.Setenv("MAI_CONTEXT_WINDOW", "32768")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	for _, effort := range []string{"l", "h", "max"} {
		t.Run(effort, func(t *testing.T) {
			started := time.Now()
			sess := deepseekTestSession(t)
			sess.Effort = effort
			sess.History = nil
			if err := appendUserPrompt(sess, "Run bash exactly once with command printf RESPONSES_OK. Then answer exactly RESPONSES_OK. Do not read or write files, use other tools or network, or access credentials."); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "session.json")
			a := newAgent(io.Discard, io.Discard, path, 2*time.Minute, false)
			a.skipSkills = true
			provider := liveToolProvider(t, sess)
			sess.Model = provider.Model
			if err := a.configureBackend(sess, provider); err != nil {
				t.Fatal(err)
			}
			done := false
			for turn := 0; turn < 4 && !done; turn++ {
				terminalItems, err := a.runTurn(ctx, sess, "Follow the requested harmless tool probe exactly.")
				if err != nil {
					t.Fatal(err)
				}
				done = len(terminalItems) > 0
				sess, err = loadSession(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := a.configureBackend(sess, provider); err != nil {
					t.Fatal(err)
				}
			}
			calls, reasoning, answered := 0, false, false
			for _, raw := range sess.History {
				var item struct {
					Type string `json:"type"`
					Name string `json:"name"`
				}
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				if item.Type == "function_call" {
					calls++
					if item.Name != "bash" {
						t.Fatal("unexpected tool")
					}
				}
				reasoning = reasoning || item.Type == "reasoning"
				entry, visible, err := visibleTranscriptEntry(raw)
				if err != nil {
					t.Fatal(err)
				}
				answered = answered || (visible && entry.Kind == "assistant" && strings.TrimSpace(entry.Text) == "RESPONSES_OK")
			}
			if !done || calls != 1 || !answered {
				t.Fatalf("probe: done=%v calls=%d reasoning=%v answered=%v", done, calls, reasoning, answered)
			}
			checkpoint, usage, err := a.backend.summarize(ctx, sess, "Goal: retain the exact audit code AUD-781 and deployment outcome UNKNOWN. The Bash probe returned RESPONSES_OK.")
			if err != nil || !strings.Contains(checkpoint, "AUD-781") || !strings.Contains(checkpoint, "UNKNOWN") || usage == nil {
				t.Fatalf("checkpoint did not preserve facts: %v", err)
			}
			t.Logf("RESPONSES_CONFORMANCE model=%s effort=%s tool_resume=true reasoning_observed=%v checkpoint=true duration_ms=%d", provider.Model, effortIDs[effort], reasoning, time.Since(started).Milliseconds())
		})
	}
}
