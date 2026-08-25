package mai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const maxAgentTurns = 64

type agent struct {
	client      *codexClient
	stdout      io.Writer
	stderr      io.Writer
	sessionPath string
	approve     approvalFunc
}

type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func newAgent(stdout, stderr io.Writer, sessionPath string) *agent {
	a := &agent{stdout: stdout, stderr: stderr, sessionPath: sessionPath}
	a.client = newCodexClient(stdout)
	a.approve = a.terminalApproval
	return a
}

func (a *agent) run(ctx context.Context, sess *session) error {
	for turn := 0; turn < maxAgentTurns; turn++ {
		result, err := a.client.stream(ctx, sess)
		if result.wrote {
			fmt.Fprintln(a.stdout)
		}
		if err != nil {
			return err
		}
		if len(result.items) == 0 {
			return errors.New("Codex response contained no output items")
		}
		sess.History = append(sess.History, result.items...)
		if err := saveJSON(a.sessionPath, sess); err != nil {
			return fmt.Errorf("save assistant response: %w", err)
		}

		calls, err := extractFunctionCalls(result.items)
		if err != nil {
			return err
		}
		if len(calls) == 0 {
			return nil
		}
		for _, call := range calls {
			output := a.executeTool(ctx, sess, call)
			item, marshalErr := json.Marshal(map[string]any{
				"type": "function_call_output", "call_id": call.CallID, "output": output,
			})
			if marshalErr != nil {
				return fmt.Errorf("encode tool output: %w", marshalErr)
			}
			sess.History = append(sess.History, item)
			if err := saveJSON(a.sessionPath, sess); err != nil {
				return fmt.Errorf("save tool output: %w", err)
			}
		}
	}
	return fmt.Errorf("agent stopped after %d model turns", maxAgentTurns)
}

func extractFunctionCalls(items []json.RawMessage) ([]functionCall, error) {
	var calls []functionCall
	for _, raw := range items {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return nil, fmt.Errorf("parse response output item: %w", err)
		}
		if head.Type != "function_call" {
			continue
		}
		var call functionCall
		if err := json.Unmarshal(raw, &call); err != nil {
			return nil, fmt.Errorf("parse function call: %w", err)
		}
		if call.CallID == "" || call.Name == "" {
			return nil, errors.New("Codex returned an incomplete function call")
		}
		calls = append(calls, call)
	}
	return calls, nil
}

func (a *agent) executeTool(ctx context.Context, sess *session, call functionCall) string {
	switch call.Name {
	case "bash":
		var args struct {
			Command   string `json:"command"`
			TimeoutMS int    `json:"timeout_ms"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return toolError("invalid bash arguments", err)
		}
		if strings.TrimSpace(args.Command) == "" {
			return toolError("invalid bash arguments", errors.New("command is empty"))
		}
		fmt.Fprintf(a.stderr, "→ bash: %s\n", oneLine(args.Command, 180))
		return runBash(ctx, bashRequest{
			Command: args.Command, TimeoutMS: args.TimeoutMS, CWD: sess.CWD,
			RepoRoot: sess.RepoRoot, Approve: a.approve,
		})
	case "apply_patch":
		var args struct {
			Patch string `json:"patch"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return toolError("invalid apply_patch arguments", err)
		}
		fmt.Fprintln(a.stderr, "→ apply_patch")
		result, err := applyPatch(sess.RepoRoot, args.Patch)
		if err != nil {
			return toolError("apply_patch failed", err)
		}
		return result
	default:
		return toolError("unknown tool", fmt.Errorf("%s is not available", call.Name))
	}
}

func toolError(message string, err error) string {
	b, _ := json.Marshal(map[string]any{"ok": false, "error": message + ": " + err.Error()})
	return string(b)
}

func oneLine(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= max {
		return value
	}
	return value[:max] + "…"
}
