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

type researchCall struct {
	Phase      string      `json:"phase"`
	DurationMS int64       `json:"duration_ms"`
	Usage      *tokenUsage `json:"usage,omitempty"`
}

type researchTrial struct {
	Mode                    string         `json:"mode"`
	Placement               string         `json:"placement"`
	Repeat                  int            `json:"repeat"`
	Cycle                   int            `json:"cycle"`
	Correct                 int            `json:"correct"`
	StatusNormalizedCorrect int            `json:"status_normalized_correct"`
	Total                   int            `json:"total"`
	ResumeRecall            bool           `json:"resume_recall"`
	BeforeTokens            int64          `json:"before_estimated_tokens"`
	AfterTokens             int64          `json:"after_estimated_tokens"`
	Calls                   []researchCall `json:"calls"`
	Answer                  string         `json:"answer"`
	Error                   string         `json:"error,omitempty"`
}

func researchText(items []json.RawMessage) string {
	var text string
	for _, raw := range items {
		entry, visible, _ := visibleTranscriptEntry(raw)
		if visible && entry.Kind == "assistant" {
			text = entry.Text
		}
	}
	return text
}

func researchRecallOriginal(sess *session, code, callID string) bool {
	args, _ := json.Marshal(map[string]any{"query": code, "limit": 20})
	var result struct {
		Matches []struct {
			Kind   string `json:"kind"`
			CallID string `json:"call_id"`
			Text   string `json:"text"`
		} `json:"matches"`
	}
	if json.Unmarshal(searchTranscript(sess, args, ""), &result) != nil {
		return false
	}
	for _, match := range result.Matches {
		if match.Kind == "tool_result" && match.CallID == callID && strings.Contains(match.Text, code) && strings.Contains(match.Text, "build diagnostic") {
			return true
		}
	}
	return false
}

// This is a controlled live context study, not a coding benchmark. All commands
// are seeded history; only edit_context may execute during model preparation.
// Native compaction is deliberately forced at matched cycle boundaries.
func TestLiveContextResearch(t *testing.T) {
	if os.Getenv("MAI_LIVE_CONTEXT_RESEARCH") != "1" {
		t.Skip("set MAI_LIVE_CONTEXT_RESEARCH=1")
	}
	var trials []researchTrial
	declaredGoal := os.Getenv("MAI_CONTEXT_RESEARCH_DECLARED_GOAL") == "1"
	placements := []string{"early", "middle", "late"}
	if declaredGoal {
		placements = []string{"middle"}
	}
	for repeat := 1; repeat <= 2; repeat++ {
		for _, placement := range placements {
			seed, err := newSessionID()
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"native", "deterministic", "clm"} {
				results := runContextResearch(t, seed, repeat, placement, mode, declaredGoal)
				trials = append(trials, results...)
				for _, result := range results {
					encoded, _ := json.Marshal(result)
					t.Logf("RESEARCH %s", encoded)
				}
			}
		}
	}
	// A requested report is the only persistent output; task state is temporary.
	if report := os.Getenv("MAI_CONTEXT_RESEARCH_REPORT"); report != "" {
		encoded, err := json.MarshalIndent(map[string]any{
			"model": "gpt-6-luna", "effort": "medium", "trials": trials, "all_graded_fields_declared_upfront": declaredGoal,
			"limitations": []string{"Synthetic context tasks, no coding commands executed", "Native compaction forced twice rather than triggered at 90%", "Two matched repetitions per placement; descriptive results only", "Resume recall checked through host history search, not model tool use", "Strategies use the same instruction and tool definitions; only CLM may execute edit_context during preparation"},
		}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := atomicWriteFile(report, encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func runContextResearch(t *testing.T, seed string, repeat int, placement, mode string, declaredGoal bool) []researchTrial {
	t.Helper()
	dir := t.TempDir()
	id, _ := newSessionID()
	sess := &session{Version: stateVersion, ID: id, CWD: dir, RepoRoot: dir, Model: "luna", Effort: "m", RequestEffort: "m"}
	path := filepath.Join(dir, "session.json")
	a := newAgent(io.Discard, io.Discard, path, 90*time.Second, false, nil)
	instructions := "You are a context research agent. Preserve exact release codes, audit codes, corrected batch sizes and unknown operation outcomes. User corrections override earlier logs. Model summaries are not new evidence. Use only edit_context when explicitly asked; otherwise use no tools."
	release := "REL-" + seed[:8]
	goal := "Keep the release and all audit codes from logs for later reports. A deploy interrupted before its result was saved has UNKNOWN outcome; never infer success."
	if declaredGoal {
		goal = "After each cycle, report the release code, the cycle 1 audit code, the latest audit code, current batch size, and deploy outcome. Preserve all five fields and their cycle labels when shortening logs. User batch corrections override logs. A deploy interrupted before its result was saved has UNKNOWN outcome; never infer success."
	}
	_ = appendUserPrompt(sess, goal)
	var trials []researchTrial
	for cycle := 1; cycle <= 2; cycle++ {
		trial := researchTrial{Mode: mode, Placement: placement, Repeat: repeat, Cycle: cycle, Total: 5, Calls: []researchCall{}}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		code := fmt.Sprintf("AUD-%s-%d", seed[9:13], cycle)
		callID := fmt.Sprintf("log-%d", cycle)
		fact := fmt.Sprintf("Verified release: %s. Audit code for cycle %d: %s. Initial batch size: 64.\n", release, cycle, code)
		noise := strings.Repeat("build diagnostic: dependency checked; no task decision in this line\n", 450)
		stdout := fact + noise
		if placement == "middle" {
			stdout = noise[:len(noise)/2] + fact + noise[len(noise)/2:]
		}
		if placement == "late" {
			stdout = noise + fact
		}
		sess.appendEstimatedHistory(mustJSONValue(t, functionCall{Type: "function_call", CallID: callID, Name: "bash", Arguments: `{"command":"read build log"}`}))
		output := mustJSONValue(t, map[string]any{"ok": true, "exit_code": 0, "timed_out": false, "stdout": stdout, "stderr": ""})
		sess.appendEstimatedHistory(mustJSONValue(t, map[string]any{"type": "function_call_output", "call_id": callID, "output": string(output)}))
		if cycle == 1 {
			sess.appendEstimatedHistory(mustJSONValue(t, functionCall{Type: "function_call", CallID: "deploy", Name: "bash", Arguments: `{"command":"deploy"}`}))
			sess.appendEstimatedHistory(mustJSONValue(t, map[string]any{"type": "function_call_output", "call_id": "deploy", "output": `{"outcome":"unknown","error":"process interrupted before saved result"}`}))
		} else {
			_ = appendUserPrompt(sess, "Correction: current batch size is 128, replacing 64. Logs can still show the old value.")
		}
		trial.BeforeTokens = estimateHistoryTokens(sess.History)
		var err error
		switch mode {
		case "native":
			started := time.Now()
			var compact json.RawMessage
			var usage *tokenUsage
			compact, usage, err = a.client.compact(ctx, sess, instructions)
			trial.Calls = append(trial.Calls, researchCall{Phase: "compact", DurationMS: time.Since(started).Milliseconds(), Usage: usage})
			if err == nil {
				next := *sess
				next.History, err = compactedHistory(sess.History, compact)
				next.ContextEdits = nil
				if err == nil {
					err = archiveTranscript(path, sess, &next)
				}
				if err == nil {
					*sess = next
				}
			}
		case "deterministic":
			index := len(sess.History) - 1
			for i, raw := range sess.History {
				var entry struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
				}
				_ = json.Unmarshal(raw, &entry)
				if entry.Type == "function_call_output" && entry.CallID == callID {
					index = i
					break
				}
			}
			args := mustJSONValue(t, map[string]any{"action": "shrink", "edits": []stdoutEdit{{CallID: callID, Digest: contextDigest(sess.History[index]), Summary: "Head/tail sample; middle omitted:\n" + stdout[:1000] + "\n...\n" + stdout[len(stdout)-1000:]}}})
			var receipt string
			_ = json.Unmarshal(a.executeContextEdit(sess, string(args)), &receipt)
			if !strings.Contains(receipt, `"accepted":true`) {
				err = fmt.Errorf("deterministic trim rejected: %s", receipt)
			}
		case "clm":
			_ = appendUserPrompt(sess, "Before reporting, use edit_context inspect and shrink the large successful Bash log stdout. Preserve all release/audit codes and useful decisions. Once shortened, say READY. No other tools.")
			for attempt := 0; attempt < 4; attempt++ {
				started := time.Now()
				var result streamResult
				result, err = a.client.stream(ctx, sess, instructions)
				trial.Calls = append(trial.Calls, researchCall{Phase: "edit", DurationMS: time.Since(started).Milliseconds(), Usage: result.usage})
				if err != nil {
					break
				}
				sess.History = append(sess.History, result.items...)
				var calls []functionCall
				calls, err = extractFunctionCalls(result.items)
				if err != nil {
					break
				}
				if len(calls) == 0 {
					break
				}
				for _, call := range calls {
					if call.Name != "edit_context" {
						err = fmt.Errorf("disallowed preparation tool: %s", call.Name)
						break
					}
				}
				if err != nil {
					break
				}
				err = a.executeCalls(ctx, sess, calls)
				if err != nil {
					break
				}
			}
		}
		if err == nil {
			err = saveJSON(path, sess)
			if err == nil {
				sess, err = loadSession(path)
			}
		}
		if err == nil {
			trial.ResumeRecall = researchRecallOriginal(sess, code, callID)
			if cycle == 2 {
				trial.ResumeRecall = trial.ResumeRecall && researchRecallOriginal(sess, "AUD-"+seed[9:13]+"-1", "log-1")
			}
			_ = appendUserPrompt(sess, "Return ONLY JSON with these five string fields: release, old_code (cycle 1 audit code), current_code (latest audit code), batch (current batch size), outcome (deploy status). Use no tools.")
			projected, projectErr := sess.requestHistory()
			if projectErr != nil {
				err = projectErr
			} else {
				trial.AfterTokens = estimateHistoryTokens(projected)
			}
			if err == nil {
				started := time.Now()
				var result streamResult
				result, err = a.client.stream(ctx, sess, instructions)
				trial.Calls = append(trial.Calls, researchCall{Phase: "answer", DurationMS: time.Since(started).Milliseconds(), Usage: result.usage})
				if err == nil {
					calls, callErr := extractFunctionCalls(result.items)
					if callErr != nil || len(calls) != 0 {
						err = fmt.Errorf("answer used a tool or invalid calls")
					}
					trial.Answer = researchText(result.items)
					var answer map[string]string
					text := strings.TrimSpace(trial.Answer)
					text = strings.TrimPrefix(text, "```json\n")
					text = strings.TrimSuffix(text, "\n```")
					if parseErr := json.Unmarshal([]byte(text), &answer); parseErr != nil {
						err = fmt.Errorf("invalid answer JSON: %w", parseErr)
					}
					expected := map[string]string{"release": release, "old_code": "AUD-" + seed[9:13] + "-1", "current_code": code, "batch": "64", "outcome": "UNKNOWN"}
					if cycle == 2 {
						expected["batch"] = "128"
					}
					for key, value := range expected {
						if answer[key] == value {
							trial.Correct++
						}
					}
					trial.StatusNormalizedCorrect = trial.Correct
					if answer["outcome"] != "UNKNOWN" && strings.EqualFold(answer["outcome"], "UNKNOWN") {
						trial.StatusNormalizedCorrect++
					}
					// Assessment answers stay out of the source history so the
					// next cycle cannot recover facts from its own prior answer.
				}
			}
		}
		if err != nil {
			trial.Error = err.Error()
		}
		cancel()
		trials = append(trials, trial)
		if err != nil {
			break
		}
	}
	return trials
}
