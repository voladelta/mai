package mai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultMaxTurns = 64

const defaultCellTimeout = 10 * time.Minute

const (
	autoCompactPercent = 80
)

type agent struct {
	backend        modelBackend
	contextWindow  int64
	maxTurns       int
	modelOutput    io.Writer
	stdout         io.Writer
	stderr         io.Writer
	sessionPath    string
	approve        approvalFunc
	skillsRoots    []string
	skillsError    error
	skipSkills     bool
	requestTimeout time.Duration
	cellTimeout    time.Duration
	python         pythonKernel
	events         io.Writer
}

type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func newAgent(stdout, stderr io.Writer, sessionPath string, requestTimeout time.Duration, inputAllowed bool) *agent {
	root, err := defaultSkillsRoot()
	var roots []string
	if err == nil {
		var cwd string
		cwd, err = currentDir()
		if err == nil {
			roots = []string{filepath.Join(findRepoRoot(cwd), "agents", "skills"), root}
		}
	}

	a := &agent{
		stdout: stdout, modelOutput: stdout, stderr: stderr, sessionPath: sessionPath,
		skillsRoots: roots, skillsError: err,
		requestTimeout: requestTimeout,
		cellTimeout:    defaultCellTimeout,
		maxTurns:       defaultMaxTurns,
	}
	if inputAllowed {
		a.approve = a.terminalApproval
	}
	return a
}

func (a *agent) run(ctx context.Context, sess *session, userPrompt string) error {
	defer a.python.close()
	interactive := isTerminalWriter(a.stderr)
	if interactive {
		fmt.Fprintln(a.stderr, "→ thinking")
	}
	instructions := systemInstructions(sess, a.loadSkillInstructions(userPrompt))
	if sess.ContextTokens == 0 {
		sess.ContextTokens = estimateHistoryTokens(sess.History) + estimateInstructionTokens(instructions)
	}
	for turn := 0; a.maxTurns == -1 || turn < a.maxTurns; turn++ {
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
	return fmt.Errorf("agent stopped after %d model turns", a.maxTurns)
}

func (a *agent) loadSkillInstructions(userPrompt string) string {
	if a.skipSkills {
		return ""
	}
	if a.skillsError != nil {
		fmt.Fprintf(a.stderr, "mai: skills unavailable: %v\n", a.skillsError)
		return ""
	}
	skillContext, err := buildSkillContext(a.skillsRoots, userPrompt)
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

func (a *agent) emit(event map[string]any) error {
	if a.events == nil {
		return nil
	}
	return writeJSONLEvent(a.events, event)
}

func (a *agent) runTurn(ctx context.Context, sess *session, instructions string) (bool, error) {
	if err := a.compactIfNeeded(ctx, sess, instructions); err != nil {
		return false, err
	}
	if err := a.emit(map[string]any{"type": "model.started"}); err != nil {
		return false, err
	}
	modelStarted := time.Now()
	backend := a.backend
	if backend == nil {
		return false, errors.New("model backend is not configured")
	}
	result, err := backend.stream(ctx, sess, instructions)
	modelDuration := time.Since(modelStarted).Milliseconds()
	if result.wrote && a.events == nil {
		fmt.Fprintln(a.stdout)
	}
	if err == nil && len(result.items) == 0 {
		err = errors.New("model response contained no output items")
	}
	if err != nil {
		if eventErr := a.emit(map[string]any{"type": "model.failed", "duration_ms": modelDuration}); eventErr != nil {
			return false, eventErr
		}
		return false, err
	}
	completed := map[string]any{"type": "model.completed", "total_tokens": result.totalTokens, "duration_ms": modelDuration}
	if result.usage != nil {
		if result.usage.InputTokens != nil {
			completed["input_tokens"] = *result.usage.InputTokens
		}
		if result.usage.OutputTokens != nil {
			completed["output_tokens"] = *result.usage.OutputTokens
		}
		if details := result.usage.InputTokensDetails; details != nil && details.CachedTokens != nil {
			completed["cached_input_tokens"] = *details.CachedTokens
		}
	}
	if err := a.emit(completed); err != nil {
		return false, err
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
	window := a.contextWindow
	if window == 0 {
		window = modelContextWindow
	}
	if sess.ContextTokens < window*autoCompactPercent/100 {
		return nil
	}
	if a.backend == nil {
		return errors.New("model backend is not configured")
	}
	started := time.Now()
	history, usage, err := portableHistory(ctx, sess, a.backend)
	if err != nil {
		return fmt.Errorf("compact conversation: %w", err)
	}
	next := *sess
	next.History = history
	next.ContextEdits = nil
	if err := archiveTranscript(a.sessionPath, sess, &next); err != nil {
		return err
	}
	next.ContextTokens = estimateHistoryTokens(history) + estimateInstructionTokens(instructions)
	if a.sessionPath != "" {
		if err := saveJSON(a.sessionPath, &next); err != nil {
			return fmt.Errorf("save compacted conversation: %w", err)
		}
	}
	*sess = next
	completed := map[string]any{"type": "compaction.completed", "strategy": "portable", "duration_ms": time.Since(started).Milliseconds()}
	if usage != nil {
		completed["usage"] = usage
	}
	return a.emit(completed)
}

func estimateInstructionTokens(instructions string) int64 {
	return (int64(len(instructions)) + 3) / 4
}

func (a *agent) executeCalls(ctx context.Context, sess *session, calls []functionCall) error {
	for _, call := range calls {
		if err := a.emit(map[string]any{"type": "tool.started", "name": call.Name, "call_id": call.CallID}); err != nil {
			return err
		}
		toolStarted := time.Now()
		output := a.executeTool(ctx, sess, call)
		toolDuration := time.Since(toolStarted).Milliseconds()

		item, err := json.Marshal(struct {
			Type   string          `json:"type"`
			CallID string          `json:"call_id"`
			Output json.RawMessage `json:"output"`
		}{Type: "function_call_output", CallID: call.CallID, Output: output})
		if err != nil {
			return fmt.Errorf("encode tool output: %w", err)
		}
		sess.appendEstimatedHistory(item)
		if call.Name == "python" {
			kept := sess.PythonActivities[:0]
			for _, activity := range sess.PythonActivities {
				if activity.OuterCallID != call.CallID {
					kept = append(kept, activity)
				}
			}
			sess.PythonActivities = kept
		}
		if a.sessionPath != "" {
			if err := saveJSON(a.sessionPath, sess); err != nil {
				return fmt.Errorf("save tool output: %w", err)
			}
		}
		event := map[string]any{"type": "tool.completed", "name": call.Name, "call_id": call.CallID, "duration_ms": toolDuration}
		if len(output) <= 256<<10 {
			event["output"] = output
		} else {
			event["output_bytes"] = len(output)
			event["output_omitted"] = true
		}
		if err := a.emit(event); err != nil {
			return err
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
			return nil, errors.New("Model returned an incomplete function call")
		}
		calls = append(calls, call)
	}
	return calls, nil
}

func (a *agent) executeTool(ctx context.Context, sess *session, call functionCall) json.RawMessage {
	switch call.Name {
	case "edit_context":
		return a.executeContextEdit(sess, call.Arguments)
	case "read_skill":
		return a.executeReadSkill(ctx, sess, call.Arguments)
	case "view_image":
		return a.executeViewImage(ctx, sess, call.Arguments)
	case "bash":
		return a.executeBash(ctx, sess, call.Arguments)
	case "python":
		return a.executePython(ctx, sess, call.Arguments, call.CallID)
	case "apply_patch":
		return a.executePatch(sess, call.Arguments)
	default:
		return textToolOutput(toolError("unknown tool", fmt.Errorf("%s is not available", call.Name)))
	}
}

func (a *agent) executeReadSkill(ctx context.Context, sess *session, arguments string) json.RawMessage {
	var args struct {
		Path string `json:"path"`
		File string `json:"file"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid read_skill arguments", err))
	}
	if a.skillsError != nil {
		return textToolOutput(toolError("find skills directory", a.skillsError))
	}
	file := args.File
	if file == "" {
		file = "SKILL.md"
	}
	fmt.Fprintf(a.stderr, "→ read_skill: %s/%s\n", args.Path, file)
	result, err := readSkill(a.skillsRoots, args.Path, args.File)
	if err != nil {
		return textToolOutput(toolError("read_skill failed", err))
	}
	if sess.Model == "ds-pro" && result.imageURL != "" {
		return a.describeImageOutput(ctx, sess, marshalToolResult(result), result.imageURL)
	}
	return skillFileToolOutput(result)
}

func (a *agent) executeViewImage(ctx context.Context, sess *session, arguments string) json.RawMessage {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid view_image arguments", err))
	}
	fmt.Fprintf(a.stderr, "→ view_image: %s\n", args.Path)
	result, err := viewImage(sess.RepoRoot, sess.CWD, args.Path)
	if err != nil {
		return textToolOutput(toolError("view_image failed", err))
	}
	if sess.Model == "ds-pro" {
		return a.describeImageOutput(ctx, sess, marshalToolResult(result), result.imageURL)
	}
	return imageContentToolOutput(marshalToolResult(result), result.imageURL)
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
		"instruction":             interruptedToolInstruction("apply_patch"),
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
	return imageContentToolOutput(metadata, file.imageURL)
}

func imageContentToolOutput(metadata, imageURL string) json.RawMessage {
	b, _ := json.Marshal([]map[string]string{
		{"type": "input_text", "text": metadata},
		{"type": "input_image", "image_url": imageURL, "detail": "auto"},
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
