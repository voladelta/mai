package mai

import (
	"context"
	"encoding/json"
	"fmt"
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
	t.Setenv("MAI_CONTEXT_WINDOW", "32768")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	for _, model := range []string{"ds-flash", "ds-pro"} {
		for _, effort := range []string{"l", "h", "max"} {
			t.Run(model+"/"+effort, func(t *testing.T) {
				started := time.Now()
				sess := deepseekTestSession(t)
				sess.Model, sess.Effort = model, effort
				sess.History = nil
				if err := appendUserPrompt(sess, "Run bash exactly once with command printf RESPONSES_OK. Then answer exactly RESPONSES_OK. Do not read or write files, use other tools or network, or access credentials."); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(t.TempDir(), "session.json")
				a := newAgent(io.Discard, io.Discard, path, 2*time.Minute, false)
				if err := a.configureBackend(sess); err != nil {
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
					if err := a.configureBackend(sess); err != nil {
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
				t.Logf("RESPONSES_CONFORMANCE model=%s effort=%s tool_resume=true reasoning_observed=%v checkpoint=true duration_ms=%d", modelID(model), effortIDs[effort], reasoning, time.Since(started).Milliseconds())
			})
		}
	}
}

// Opt-in paid director/worker integration in an empty temporary workspace.
func TestLiveDeepSeekSidekick(t *testing.T) {
	if os.Getenv("MAI_LIVE_DEEPSEEK_RESPONSES") != "1" {
		t.Skip("set MAI_LIVE_DEEPSEEK_RESPONSES=1")
	}
	t.Setenv("MAI_CONTEXT_WINDOW", "32768")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	for _, effort := range []string{"h", "max"} {
		t.Run(effort, func(t *testing.T) {
			started := time.Now()
			seed, err := newSessionID()
			if err != nil {
				t.Fatal(err)
			}
			first, second := "FIRST_"+seed[:8], "SECOND_"+seed[9:13]
			prompt := fmt.Sprintf(`Run this harmless live sidekick probe. You are authorized to delegate.
Call sidekick exactly twice, sequentially, and use no other tools yourself.
First assignment: tell the worker to call bash exactly once with command printf %s, then answer exactly %s. Do not read or write files, use other tools or network, or access credentials.
Second assignment: reuse the returned worker_id. Tell the worker to recall its previous answer from its own conversation, call bash exactly once with command printf %s, and answer with its previous answer followed by a space and the new Bash output. Do not repeat the first token in the follow-up task or context. Do not read or write files, use other tools or network, or access credentials.
After both assignments complete, return exactly the second worker answer.`, first, first, second)
			sess := deepseekTestSession(t)
			sess.Model, sess.Effort, sess.History = "ds-pro", effort, nil
			if err := appendUserPrompt(sess, prompt); err != nil {
				t.Fatal(err)
			}
			a := newAgent(io.Discard, io.Discard, "", 2*time.Minute, false)
			a.skipSkills, a.maxTurns = true, 6
			if err := a.configureBackend(sess); err != nil {
				t.Fatal(err)
			}
			if err := a.run(ctx, sess, prompt); err != nil {
				t.Fatal(err)
			}

			calls, results := 0, 0
			answered := false
			for _, raw := range sess.History {
				var item struct {
					Type      string `json:"type"`
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
					Output    string `json:"output"`
				}
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				if item.Type == "function_call" {
					calls++
					if item.Name != "sidekick" || (calls == 2 && strings.Contains(item.Arguments, first)) {
						t.Fatal("director used an unexpected tool or repeated the first token in the follow-up")
					}
				}
				if item.Type == "function_call_output" {
					results++
					var result sidekickResult
					if err := json.Unmarshal([]byte(item.Output), &result); err != nil {
						t.Fatal(err)
					}
					want := first
					if results == 2 {
						want += " " + second
					}
					if !result.OK || result.Model != "ds-flash" || result.Effort != "high" || strings.TrimSpace(result.Answer) != want || result.UsageReports == 0 {
						t.Fatalf("invalid live worker result: %+v", result)
					}
				}
				entry, visible, err := visibleTranscriptEntry(raw)
				if err != nil {
					t.Fatal(err)
				}
				answered = answered || (visible && entry.Kind == "assistant" && strings.TrimSpace(entry.Text) == first+" "+second)
			}
			if calls != 2 || results != 2 || !answered || a.sidekick == nil {
				t.Fatalf("live director: calls=%d results=%d answered=%v", calls, results, answered)
			}
			workerCalls := 0
			for _, raw := range a.sidekick.session.History {
				var item struct {
					Type string `json:"type"`
					Name string `json:"name"`
				}
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				if item.Type == "function_call" {
					workerCalls++
					if item.Name != "bash" {
						t.Fatalf("unexpected worker tool %s", item.Name)
					}
				}
			}
			if workerCalls != 2 {
				t.Fatalf("worker Bash calls=%d, want 2", workerCalls)
			}
			t.Logf("SIDEKICK_CONFORMANCE director=ds-pro/%s worker=ds-flash/high followup=true tools=true director_turns=%d worker_turns=%d duration_ms=%d", effortIDs[effort], a.modelTurns, a.sidekick.agent.modelTurns, time.Since(started).Milliseconds())
		})
	}
}
