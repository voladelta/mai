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

const (
	autoCompactPercent        = 90
	retainedMessageTokenLimit = 64_000
)

type agent struct {
	client        *codexClient
	stdout        io.Writer
	stderr        io.Writer
	sessionPath   string
	approve       approvalFunc
	skillsRoot    string
	skillsError   error
	customAgent   *customAgent
	customAgents  map[string]customAgent
	agentWarnings []string
	agentsError   error
	executable    string
	timeout       time.Duration
}

type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func newAgent(stdout, stderr io.Writer, sessionPath string, timeout time.Duration, inputAllowed bool, customRole *customAgent) *agent {
	root, err := defaultSkillsRoot()
	var customAgents map[string]customAgent
	var agentWarnings []string
	var agentsErr error
	var executable string
	if customRole == nil {
		var agentsRoot string
		agentsRoot, agentsErr = defaultAgentsRoot()
		if agentsErr == nil {
			customAgents, agentWarnings, agentsErr = loadCustomAgents(agentsRoot)
		}
		if agentsErr == nil && len(customAgents) > 0 {
			executable, agentsErr = os.Executable()
			if agentsErr != nil {
				agentsErr = fmt.Errorf("find mai executable: %w", agentsErr)
			}
		}
	}
	a := &agent{
		stdout: stdout, stderr: stderr, sessionPath: sessionPath,
		skillsRoot: root, skillsError: err,
		customAgent:  customRole,
		customAgents: customAgents, agentWarnings: agentWarnings, agentsError: agentsErr,
		executable: executable, timeout: timeout,
	}
	a.client = newCodexClient(stdout, timeout)
	a.client.allowSubagents = agentsErr == nil && len(customAgents) > 0
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
	instructions := systemInstructions(sess,
		customAgentInstructions(a.customAgent),
		a.loadSkillInstructions(userPrompt),
		a.loadSubagentInstructions(),
	)
	if sess.ContextTokens == 0 {
		sess.ContextTokens = estimateHistoryTokens(sess.History) + (int64(len(instructions))+3)/4
	}
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

func (a *agent) loadSubagentInstructions() string {
	if a.customAgent != nil {
		return ""
	}
	if a.agentsError != nil {
		fmt.Fprintf(a.stderr, "mai: custom agents unavailable: %v\n", a.agentsError)
		return ""
	}
	for _, warning := range a.agentWarnings {
		fmt.Fprintf(a.stderr, "mai: custom agent warning: %s\n", warning)
	}
	return renderSubagentInstructions(a.customAgents)
}

func (a *agent) loadSkillInstructions(userPrompt string) string {
	if a.customAgent != nil {
		return ""
	}
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
	if err := a.compactIfNeeded(ctx, sess, instructions); err != nil {
		return false, err
	}
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
	if result.totalTokens > 0 {
		sess.ContextTokens = result.totalTokens
	} else {
		sess.ContextTokens += estimateHistoryTokens(result.items)
	}
	if a.sessionPath != "" {
		if err := saveJSON(a.sessionPath, sess); err != nil {
			return false, fmt.Errorf("save assistant response: %w", err)
		}
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

func (a *agent) compactIfNeeded(ctx context.Context, sess *session, instructions string) error {
	contextWindow := modelContextWindows[sess.Model]
	if contextWindow == 0 || sess.ContextTokens < contextWindow*autoCompactPercent/100 {
		return nil
	}
	compaction, err := a.client.compact(ctx, sess, instructions)
	if err != nil {
		return fmt.Errorf("compact conversation: %w", err)
	}
	history, err := compactedHistory(sess.History, compaction)
	if err != nil {
		return err
	}
	next := *sess
	next.History = history
	next.ContextTokens = estimateHistoryTokens(history) + (int64(len(instructions))+3)/4
	if a.sessionPath != "" {
		if err := saveJSON(a.sessionPath, &next); err != nil {
			return fmt.Errorf("save compacted conversation: %w", err)
		}
	}
	*sess = next
	return nil
}

func compactedHistory(history []json.RawMessage, compaction json.RawMessage) ([]json.RawMessage, error) {
	remaining := int64(retainedMessageTokenLimit)
	retained := make([]json.RawMessage, 0, len(history)+1)
	for i := len(history) - 1; i >= 0; i-- {
		var item struct {
			Role string `json:"role"`
		}
		if err := json.Unmarshal(history[i], &item); err != nil {
			return nil, fmt.Errorf("parse history for compaction: %w", err)
		}
		if item.Role != "user" && item.Role != "developer" && item.Role != "system" {
			continue
		}
		tokens := estimateHistoryItemTokens(history[i])
		if tokens > remaining {
			continue
		}
		remaining -= tokens
		retained = append(retained, history[i])
	}
	for left, right := 0, len(retained)-1; left < right; left, right = left+1, right-1 {
		retained[left], retained[right] = retained[right], retained[left]
	}
	return append(retained, compaction), nil
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
		sess.appendEstimatedHistory(item)
		if a.sessionPath != "" {
			if err := saveJSON(a.sessionPath, sess); err != nil {
				return fmt.Errorf("save tool output: %w", err)
			}
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
	case "spawn_subagent":
		return a.executeSpawnSubagent(ctx, sess, call.Arguments)
	case "bash":
		return a.executeBash(ctx, sess, call.Arguments)
	case "apply_patch":
		return a.executePatch(sess, call.Arguments)
	default:
		return textToolOutput(toolError("unknown tool", fmt.Errorf("%s is not available", call.Name)))
	}
}

func (a *agent) executeSpawnSubagent(ctx context.Context, sess *session, arguments string) json.RawMessage {
	var args struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid spawn_subagent arguments", err))
	}
	if a.customAgent != nil {
		return textToolOutput(toolError("spawn_subagent failed", errors.New("a subagent cannot spawn another subagent")))
	}
	if err := validateSubagentName(args.Name); err != nil {
		return textToolOutput(toolError("spawn_subagent failed", err))
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return textToolOutput(toolError("spawn_subagent failed", errors.New("prompt is empty")))
	}
	if len(args.Prompt) > maxSubagentPromptBytes {
		return textToolOutput(toolError("spawn_subagent failed", fmt.Errorf("prompt exceeds %d bytes", maxSubagentPromptBytes)))
	}
	if _, ok := a.customAgents[args.Name]; !ok {
		return textToolOutput(toolError("spawn_subagent failed", fmt.Errorf("custom agent %q is not available", args.Name)))
	}
	fmt.Fprintf(a.stderr, "→ subagent: %s\n", args.Name)
	raw := runSubagentProcess(ctx, a.executable, a.timeout, sess.CWD, args.Name, args.Prompt)
	var result subagentResult
	if err := json.Unmarshal([]byte(raw), &result); err == nil {
		status := "completed"
		if !result.OK {
			status = "failed"
		}
		fmt.Fprintf(a.stderr, "← subagent: %s %s (%s)\n", args.Name, status, time.Duration(result.DurationMS)*time.Millisecond)
	}
	return textToolOutput(raw)
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
	return patchToolOutput(result, err)
}

func patchToolOutput(result string, err error) json.RawMessage {
	if err == nil {
		return textToolOutput(result)
	}
	var commitErr *patchCommitError
	if !errors.As(err, &commitErr) {
		return textToolOutput(toolError("apply_patch failed", err))
	}
	b, _ := json.Marshal(map[string]any{
		"ok":                      false,
		"outcome":                 "partial",
		"error":                   "apply_patch failed: " + commitErr.Error(),
		"applied":                 commitErr.applied,
		"failed":                  commitErr.failed,
		"pending":                 commitErr.pending,
		"reconciliation_required": true,
		"instruction":             "Inspect the repository and reconcile the requested patch with the current files before you retry apply_patch.",
	})
	return textToolOutput(string(b))
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
