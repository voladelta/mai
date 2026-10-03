package mai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContextEditEvalRejectsClaimsWithoutEvidence(t *testing.T) {
	for _, test := range []struct {
		name        string
		fakeRecall  bool
		wantError   string
		wantCorrect bool
	}{
		{
			name:      "claims editing without tool calls",
			wantError: "did not successfully inspect then shrink",
		},
		{
			name:        "correct answer without history retrieval",
			fakeRecall:  true,
			wantError:   "failed verification",
			wantCorrect: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeTestDeepSeekConfig(t)
			seed := "01234567-89ab-cdef-0123-456789abcdef"
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var payload struct {
					Input []json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}

				var item any = map[string]any{
					"type": "message", "role": "assistant",
					"content": []map[string]string{{"type": "output_text", "text": "EDITED"}},
				}
				if test.fakeRecall {
					switch requests {
					case 1:
						item = functionCall{Type: "function_call", CallID: "inspect", Name: "edit_context", Arguments: `{"action":"inspect"}`}
					case 2:
						var envelope struct {
							Output string `json:"output"`
						}
						var inspection struct {
							Candidates []stdoutEdit `json:"candidates"`
						}
						if json.Unmarshal(payload.Input[len(payload.Input)-1], &envelope) != nil || json.Unmarshal([]byte(envelope.Output), &inspection) != nil || len(inspection.Candidates) != 1 {
							t.Error("inspection did not return the real source handle")
							w.WriteHeader(http.StatusBadRequest)
							return
						}
						edit := inspection.Candidates[0]
						edit.Summary = "Current release: REL-01234567. Deployment outcome: UNKNOWN."
						args := mustJSONValue(t, map[string]any{"action": "shrink", "edits": []stdoutEdit{edit}})
						item = functionCall{Type: "function_call", CallID: "shrink", Name: "edit_context", Arguments: string(args)}
					case 4:
						// This provider pretends to recall the fact but never invokes
						// history. The interpreter returns an empty search result.
						item = functionCall{Type: "function_call", CallID: "fake-recall", Name: "python", Arguments: `{"code":"print('{}')"}`}
					case 5:
						answer := mustJSONValue(t, map[string]string{
							"retired_audit": "OLD-" + seed, "current_release": "REL-01234567", "deploy_outcome": "UNKNOWN",
						})
						item = map[string]any{
							"type": "message", "role": "assistant",
							"content": []map[string]string{{"type": "output_text", "text": string(answer)}},
						}
					}
				}
				writeSSEItem(t, w, string(mustJSONValue(t, item)), 5000)
			}))
			defer server.Close()
			t.Setenv("MAI_DEEPSEEK_URL", server.URL)

			trial := runContextEditTrial(t, "ds-flash", seed)
			if !strings.Contains(trial.Error, test.wantError) || trial.Correct != test.wantCorrect || trial.HistoryRetrieved {
				t.Fatalf("unearned grade: %#v", trial)
			}
			if test.fakeRecall && (!trial.Inspected || !trial.Shrunk || !trial.ResumedProjection || !trial.FactOmitted) {
				t.Fatalf("negative recall control failed before reaching its intended boundary: %#v", trial)
			}
		})
	}
}
