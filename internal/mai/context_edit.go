package mai

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// History remains the source for recovery and recall. Only request rendering
// applies these edits; opaque items and call relationships never change.
type contextEdit struct {
	Index   int    `json:"index"`
	Digest  string `json:"digest"`
	Summary string `json:"summary"`
}

type stdoutEdit struct {
	CallID  string `json:"call_id"`
	Digest  string `json:"digest"`
	Summary string `json:"stdout_summary"`
}

type contextCallRelationship struct {
	calls    int
	outputs  int
	lastCall int
	outputAt int
	nonBash  bool
}

type contextCallIndex struct {
	byID    map[string]contextCallRelationship
	invalid bool
}

// Index only relationship fields. Original and projected histories have the
// same relationships, but output text and digests must come from each source.
func indexContextCalls(history []json.RawMessage) contextCallIndex {
	index := contextCallIndex{byID: make(map[string]contextCallRelationship)}
	for i, raw := range history {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		}
		if json.Unmarshal(raw, &item) != nil {
			index.invalid = true
			continue
		}

		relationship := index.byID[item.CallID]
		switch item.Type {
		case "function_call":
			relationship.calls++
			relationship.lastCall = i
			relationship.nonBash = relationship.nonBash || item.Name != "bash"
		case "function_call_output":
			if relationship.outputs == 0 {
				relationship.outputAt = i
			}
			relationship.outputs++
		default:
			continue
		}
		index.byID[item.CallID] = relationship
	}
	return index
}

// Hints belong after the conversation, not in the stable instruction prefix.
// Bound their size and scan range. Stop at the last assistant answer so
// automatic hints do not offer outputs from the preceding cached turn.
func contextEditHint(history []json.RawMessage) json.RawMessage {
	relationships := indexContextCalls(history)
	candidates := []map[string]string{}
	start := len(history) - 32
	if start < 0 {
		start = 0
	}
	for i := len(history) - 1; i >= start && len(candidates) < 8; i-- {
		var boundary struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(history[i], &boundary)
		if boundary.Role == "assistant" {
			break
		}
		output, err := editableBashOutput(history, relationships, i)
		if err != nil {
			continue
		}
		if len(output.Stdout) < 16<<10 {
			continue
		}
		candidates = append(candidates, map[string]string{"call_id": output.CallID, "digest": contextDigest(history[i])})
	}
	if len(candidates) == 0 {
		return nil
	}
	handles, _ := json.Marshal(candidates)
	text := "Context pressure is high. Consider shortening large obsolete successful Bash stdout if future savings justify editing. Preserve exact facts and decisions still needed; originals remain searchable. You can shrink directly using these current call_id/digest handles, without inspecting first: " + string(handles) + ". Continue the task after editing; no separate acknowledgement is needed."
	message, _ := json.Marshal(map[string]any{
		"role": "developer", "content": []map[string]string{{"type": "input_text", "text": text}},
	})
	return message
}

func contextDigest(raw json.RawMessage) string {
	// Session formatting changes whitespace and escaping. Hash canonical JSON
	// so a checkpoint survives save/load without weakening source validation.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) == nil {
		if canonical, err := json.Marshal(value); err == nil {
			raw = canonical
		}
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

type editableBashResult struct {
	CallID string
	Stdout string
	Output string
}

func editableBashOutput(history []json.RawMessage, relationships contextCallIndex, index int) (editableBashResult, error) {
	if index < 0 || index >= len(history) {
		return editableBashResult{}, errors.New("context edit index is outside history")
	}

	var item struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(history[index], &item); err != nil || item.Type != "function_call_output" || item.CallID == "" {
		return editableBashResult{}, errors.New("context edit requires a text tool output")
	}

	// Require one earlier Bash call and exactly one output for this ID. Other
	// tools, pending operations, and recovered unknown outcomes are ineligible.
	if relationships.invalid {
		return editableBashResult{}, errors.New("invalid history item")
	}
	relationship := relationships.byID[item.CallID]
	if relationship.calls > 0 && (relationship.lastCall >= index || relationship.nonBash) {
		return editableBashResult{}, errors.New("context edit requires an earlier Bash call")
	}
	if relationship.calls != 1 || relationship.outputs != 1 {
		return editableBashResult{}, errors.New("ambiguous tool call relationship")
	}

	var result struct {
		OK       bool   `json:"ok"`
		ExitCode *int   `json:"exit_code"`
		TimedOut bool   `json:"timed_out"`
		Stdout   string `json:"stdout"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal([]byte(item.Output), &result); err != nil || !result.OK || result.ExitCode == nil || *result.ExitCode != 0 || result.TimedOut || result.Error != "" || result.Stdout == "" {
		return editableBashResult{}, errors.New("only successful Bash stdout can be shortened")
	}

	return editableBashResult{CallID: item.CallID, Stdout: result.Stdout, Output: item.Output}, nil
}

func renderStdoutEdit(history []json.RawMessage, relationships contextCallIndex, edit contextEdit) (json.RawMessage, error) {
	if edit.Index < 0 || edit.Index >= len(history) || contextDigest(history[edit.Index]) != edit.Digest {
		return nil, errors.New("context edit source no longer matches history")
	}
	if strings.TrimSpace(edit.Summary) == "" || len(edit.Summary) > 16<<10 {
		return nil, errors.New("stdout summary must contain 1 to 16384 bytes")
	}
	result, err := editableBashOutput(history, relationships, edit.Index)
	if err != nil {
		return nil, err
	}

	var envelope, body map[string]json.RawMessage
	if err := json.Unmarshal(history[edit.Index], &envelope); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(result.Output), &body); err != nil {
		return nil, err
	}

	body["stdout"], _ = json.Marshal("[Model-authored context summary; original stdout remains in task history]\n" + edit.Summary)
	body["stdout_context_summary"] = json.RawMessage("true")
	output, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	envelope["output"], _ = json.Marshal(string(output))
	return json.Marshal(envelope)
}

func (sess *session) requestHistory() ([]json.RawMessage, error) {
	var relationships contextCallIndex
	if len(sess.ContextEdits) > 0 {
		relationships = indexContextCalls(sess.History)
	}

	history, err := sess.projectHistory(relationships)
	if err != nil {
		return nil, err
	}
	if sess.ReasoningStart == 0 {
		return history, nil
	}

	// Reasoning can be tied to its provider. Preserve original history and edit
	// indexes, but exclude old reasoning when replaying to an overridden backend.
	filtered := make([]json.RawMessage, 0, len(history))
	for index, raw := range history {
		if index < sess.ReasoningStart {
			var item struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(raw, &item); err != nil {
				return nil, err
			}
			if item.Type == "reasoning" {
				continue
			}
		}
		filtered = append(filtered, raw)
	}
	return filtered, nil
}

func (sess *session) projectHistory(relationships contextCallIndex) ([]json.RawMessage, error) {
	history := append([]json.RawMessage(nil), sess.History...)
	seen := make(map[int]bool)
	for _, edit := range sess.ContextEdits {
		if seen[edit.Index] {
			return nil, errors.New("duplicate context edit index")
		}
		seen[edit.Index] = true
		rendered, err := renderStdoutEdit(sess.History, relationships, edit)
		if err != nil {
			return nil, err
		}
		if estimateHistoryItemTokens(rendered) >= estimateHistoryItemTokens(sess.History[edit.Index]) {
			return nil, errors.New("saved context edit does not reduce estimated size")
		}
		history[edit.Index] = rendered
	}
	return history, nil
}

func (a *agent) executeContextEdit(sess *session, arguments string) json.RawMessage {
	var args struct {
		Action string       `json:"action"`
		Edits  []stdoutEdit `json:"edits"`
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return textToolOutput(toolError("invalid edit_context arguments", err))
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return textToolOutput(toolError("invalid edit_context arguments", errors.New("expected one JSON object")))
	}
	relationships := indexContextCalls(sess.History)
	history, err := sess.projectHistory(relationships)
	if err != nil {
		return textToolOutput(toolError("invalid context projection", err))
	}

	if args.Action == "inspect" && len(args.Edits) == 0 {
		candidates := []map[string]any{}
		for i, raw := range history {
			output, err := editableBashOutput(history, relationships, i)
			if err != nil {
				continue
			}
			// Small outputs cannot repay summary framing or another model call.
			if len(output.Stdout) < 1024 {
				continue
			}
			candidates = append(candidates, map[string]any{
				"call_id": output.CallID, "digest": contextDigest(raw),
				"estimated_tokens": estimateHistoryItemTokens(raw),
			})
		}
		data, _ := json.Marshal(map[string]any{"candidates": candidates})
		return textToolOutput(string(data))
	}
	if args.Action != "shrink" || len(args.Edits) == 0 || len(args.Edits) > 8 {
		return textToolOutput(toolError("invalid edit_context action", errors.New("inspect without edits, or shrink with 1 to 8 edits")))
	}

	next := *sess
	next.ContextEdits = append([]contextEdit(nil), sess.ContextEdits...)
	used := make(map[string]bool)
	var totalSavings int64
	for _, proposed := range args.Edits {
		if used[proposed.CallID] {
			return textToolOutput(toolError("context edit rejected", errors.New("duplicate call ID")))
		}
		used[proposed.CallID] = true
		relationship := relationships.byID[proposed.CallID]
		index := relationship.outputAt
		if relationship.outputs == 0 || contextDigest(history[index]) != proposed.Digest {
			return textToolOutput(toolError("context edit rejected", errors.New("stale or missing output; inspect context again")))
		}
		edit := contextEdit{Index: index, Digest: contextDigest(sess.History[index]), Summary: proposed.Summary}
		rendered, err := renderStdoutEdit(sess.History, relationships, edit)
		if err != nil {
			return textToolOutput(toolError("context edit rejected", err))
		}
		savings := estimateHistoryItemTokens(history[index]) - estimateHistoryItemTokens(rendered)
		if savings <= 0 {
			return textToolOutput(toolError("context edit rejected", errors.New("summary must reduce estimated request size")))
		}
		replaced := false
		for i := range next.ContextEdits {
			if next.ContextEdits[i].Index == index {
				next.ContextEdits[i] = edit
				replaced = true
				break
			}
		}
		if !replaced {
			next.ContextEdits = append(next.ContextEdits, edit)
		}
		next.ContextTokens = max(0, next.ContextTokens-savings)
		totalSavings += savings
	}

	if a.sessionPath != "" {
		if err := saveJSON(a.sessionPath, &next); err != nil {
			return textToolOutput(toolError("save context edit", err))
		}
	}
	*sess = next
	data, _ := json.Marshal(map[string]any{
		"accepted": true, "edited_outputs": len(args.Edits),
		"estimated_tokens_saved": totalSavings,
	})
	return textToolOutput(string(data))
}

// Keep invalid persisted projections from reaching the network. Old sessions
// have no edits and retain their existing behavior.
func validateContextEdits(sess *session) error {
	if len(sess.ContextEdits) == 0 {
		return nil
	}
	if _, err := sess.requestHistory(); err != nil {
		return fmt.Errorf("invalid saved context edits: %w", err)
	}
	return nil
}
