package mai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const maxAgentTurns = 64

type agent struct {
	client      *codexClient
	stdout      io.Writer
	stderr      io.Writer
	sessionPath string
	approve     approvalFunc
	skillsRoot  string
	skillsError error
}

type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func newAgent(stdout, stderr io.Writer, sessionPath string, timeout time.Duration, inputAllowed bool) *agent {
	root, err := defaultSkillsRoot()
	a := &agent{
		stdout: stdout, stderr: stderr, sessionPath: sessionPath,
		skillsRoot: root, skillsError: err,
	}
	a.client = newCodexClient(stdout, timeout)
	if inputAllowed {
		a.approve = a.terminalApproval
	}
	return a
}

func (a *agent) run(ctx context.Context, sess *session, userPrompt string) error {
	interactive := isTerminalWriter(a.stderr)
	if interactive {
		fmt.Fprintln(a.stderr, "→ thinking")
	}
	instructions := systemInstructions(sess, a.loadSkillInstructions(userPrompt))
	for turn := 0; turn < maxAgentTurns; turn++ {
		if interactive && turn > 0 {
			fmt.Fprintln(a.stderr, "→ thinking")
		}
		done, err := a.runTurn(ctx, sess, instructions)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
	return fmt.Errorf("agent stopped after %d model turns", maxAgentTurns)
}

func (a *agent) loadSkillInstructions(userPrompt string) string {
	if a.skillsError != nil {
		fmt.Fprintf(a.stderr, "mai: skills unavailable: %v\n", a.skillsError)
		return ""
	}
	skillContext, err := buildSkillContext(a.skillsRoot, userPrompt)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(a.stderr, "mai: skills unavailable: %v\n", err)
		}
		return ""
	}
	for _, warning := range skillContext.Warnings {
		fmt.Fprintf(a.stderr, "mai: skill warning: %s\n", warning)
	}
	return skillContext.Instructions
}

func (a *agent) runTurn(ctx context.Context, sess *session, instructions string) (bool, error) {
	result, err := a.client.stream(ctx, sess, instructions)
	if result.wrote {
		fmt.Fprintln(a.stdout)
	}
	if err != nil {
		return false, err
	}
	if len(result.items) == 0 {
		return false, errors.New("Codex response contained no output items")
	}
	sess.History = append(sess.History, result.items...)
	if err := saveJSON(a.sessionPath, sess); err != nil {
		return false, fmt.Errorf("save assistant response: %w", err)
	}
	calls, err := extractFunctionCalls(result.items)
	if err != nil {
		return false, err
	}
	if len(calls) == 0 {
		return true, nil
	}
	if err := a.executeCalls(ctx, sess, calls); err != nil {
		return false, err
	}
	return false, nil
}

func (a *agent) executeCalls(ctx context.Context, sess *session, calls []functionCall) error {
	for _, call := range calls {
		output := a.executeTool(ctx, sess, call)
		item, err := json.Marshal(struct {
			Type   string          `json:"type"`
			CallID string          `json:"call_id"`
			Output json.RawMessage `json:"output"`
		}{Type: "function_call_output", CallID: call.CallID, Output: output})
		if err != nil {
			return fmt.Errorf("encode tool output: %w", err)
		}
		sess.History = append(sess.History, item)
		if err := saveJSON(a.sessionPath, sess); err != nil {
			return fmt.Errorf("save tool output: %w", err)
		}
	}
	return nil
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func isTerminalWriter(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && isTerminal(file)
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

func (a *agent) executeTool(ctx context.Context, sess *session, call functionCall) json.RawMessage {
	switch call.Name {
	case "read_skill":
		return a.executeReadSkill(call.Arguments)
	case "read_skill_file":
		return a.executeReadSkillFile(call.Arguments)
	case "bash":
		return a.executeBash(ctx, sess, call.Arguments)
	case "apply_patch":
		return a.executePatch(sess, call.Arguments)
	default:
		return textToolOutput(toolError("unknown tool", fmt.Errorf("%s is not available", call.Name)))
	}
}

func (a *agent) executeReadSkill(arguments string) json.RawMessage {
	var args struct {
		Skill string `json:"skill"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid read_skill arguments", err))
	}
	if a.skillsError != nil {
		return textToolOutput(toolError("find skills directory", a.skillsError))
	}
	fmt.Fprintf(a.stderr, "→ read_skill: %s\n", args.Skill)
	result, err := readSkill(a.skillsRoot, args.Skill)
	if err != nil {
		return textToolOutput(toolError("read_skill failed", err))
	}
	return skillFileToolOutput(result)
}

func (a *agent) executeReadSkillFile(arguments string) json.RawMessage {
	var args struct {
		Skill string `json:"skill"`
		Path  string `json:"path"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid read_skill_file arguments", err))
	}
	if a.skillsError != nil {
		return textToolOutput(toolError("find skills directory", a.skillsError))
	}
	fmt.Fprintf(a.stderr, "→ read_skill_file: %s/%s\n", args.Skill, args.Path)
	result, err := readSkillFile(a.skillsRoot, args.Skill, args.Path)
	if err != nil {
		return textToolOutput(toolError("read_skill_file failed", err))
	}
	return skillFileToolOutput(result)
}

func (a *agent) executeBash(ctx context.Context, sess *session, arguments string) json.RawMessage {
	var args struct {
		Command   string `json:"command"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid bash arguments", err))
	}
	if strings.TrimSpace(args.Command) == "" {
		return textToolOutput(toolError("invalid bash arguments", errors.New("command is empty")))
	}
	fmt.Fprintf(a.stderr, "→ bash: %s\n", oneLine(args.Command, 180))
	return textToolOutput(runBash(ctx, bashRequest{
		Command: args.Command, TimeoutMS: args.TimeoutMS, CWD: sess.CWD,
		RepoRoot: sess.RepoRoot, Approve: a.approve,
	}))
}

func (a *agent) executePatch(sess *session, arguments string) json.RawMessage {
	var args struct {
		Patch string `json:"patch"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid apply_patch arguments", err))
	}
	fmt.Fprintln(a.stderr, "→ apply_patch")
	result, err := applyPatch(sess.RepoRoot, args.Patch)
	if err != nil {
		return textToolOutput(toolError("apply_patch failed", err))
	}
	return textToolOutput(result)
}

func textToolOutput(value string) json.RawMessage {
	b, _ := json.Marshal(value)
	return b
}

func skillFileToolOutput(file skillFileResult) json.RawMessage {
	metadata := marshalToolResult(file)
	if file.imageURL == "" {
		return textToolOutput(metadata)
	}
	b, _ := json.Marshal([]map[string]string{
		{"type": "input_text", "text": metadata},
		{"type": "input_image", "image_url": file.imageURL, "detail": "auto"},
	})
	return b
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
