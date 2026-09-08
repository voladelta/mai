package mai

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// These are Go-dispatched operations inside a Python call, not model-issued
// function calls. Pending entries remain durable until the outer output saves.
type pythonActivity struct {
	OuterCallID string          `json:"outer_call_id"`
	Generation  int             `json:"generation"`
	Cell        int             `json:"cell"`
	Call        int             `json:"call"`
	Name        string          `json:"name"`
	Arguments   json.RawMessage `json:"arguments"`
	Status      string          `json:"status"`
	Result      json.RawMessage `json:"result,omitempty"`
}

type pythonActivitySummary struct {
	Generation int    `json:"generation"`
	Cell       int    `json:"cell"`
	Call       int    `json:"call"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Arguments  string `json:"arguments_summary"`
	Result     string `json:"result_summary,omitempty"`
}

func summarizePythonActivities(activities []pythonActivity) ([]pythonActivitySummary, int) {
	clip := func(raw json.RawMessage) string {
		if len(raw) <= 160 {
			return string(raw)
		}
		end := 160
		for !utf8.Valid(raw[:end]) {
			end--
		}
		return string(raw[:end]) + "..."
	}
	var summaries []pythonActivitySummary
	budget := 32 << 10
	// Keep unknown outcomes visible first if unusual saved data exhausts the
	// summary budget. Full entries live only in the active durable journal.
	for _, unknown := range []bool{true, false} {
		for _, activity := range activities {
			if (activity.Status == "unknown" || activity.Status == "pending") != unknown {
				continue
			}
			summary := pythonActivitySummary{
				Generation: activity.Generation, Cell: activity.Cell, Call: activity.Call,
				Name: activity.Name, Status: activity.Status,
				Arguments: clip(activity.Arguments), Result: clip(activity.Result),
			}
			data, _ := json.Marshal(summary)
			if len(data)+1 > budget {
				continue
			}
			budget -= len(data) + 1
			summaries = append(summaries, summary)
		}
	}
	return summaries, len(activities) - len(summaries)
}

func (a *agent) pythonHost(sess *session, outerCall string) pythonHostHandler {
	return func(ctx context.Context, generation, cell, call int, name string, arguments json.RawMessage) (json.RawMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		index := len(sess.PythonActivities)
		sess.PythonActivities = append(sess.PythonActivities, pythonActivity{
			OuterCallID: outerCall, Generation: generation, Cell: cell, Call: call,
			Name: name, Arguments: arguments, Status: "pending",
		})
		if a.sessionPath != "" {
			if err := saveJSON(a.sessionPath, sess); err != nil {
				sess.PythonActivities[index].Status = "not_started"
				return nil, fmt.Errorf("save Python activity before dispatch: %w", err)
			}
		}

		var raw json.RawMessage
		status := "completed"
		if err := ctx.Err(); err != nil {
			raw = textToolOutput(toolError("Python host call cancelled before dispatch", err))
			status = "not_started"
		} else {
			switch name {
			case "bash", "apply_patch", "spawn_subagent":
				raw = a.executeTool(ctx, sess, functionCall{Name: name, Arguments: string(arguments)})
			default:
				raw = textToolOutput(toolError("Python host call rejected", fmt.Errorf("%s is not available through the bridge", name)))
				status = "rejected"
			}
		}
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, fmt.Errorf("invalid Python host tool output: %w", err)
		}
		result := json.RawMessage(text)
		if !json.Valid(result) {
			result = raw
		}
		var outcome struct {
			OK    *bool  `json:"ok"`
			Error string `json:"error"`
		}
		if status == "completed" && json.Unmarshal(result, &outcome) == nil && (outcome.OK != nil && !*outcome.OK || outcome.Error != "") {
			status = "failed"
		}
		if ctx.Err() != nil && status != "not_started" {
			status = "unknown"
		}
		sess.PythonActivities[index].Status = status
		sess.PythonActivities[index].Result = result
		if a.sessionPath != "" {
			if err := saveJSON(a.sessionPath, sess); err != nil {
				// The saved entry is still pending. Do not acknowledge durable
				// completion or cause a retry when the result cannot be saved.
				sess.PythonActivities[index].Status = "unknown"
				return nil, fmt.Errorf("save Python activity result; outcome is unknown: %w", err)
			}
		}
		return result, nil
	}
}
