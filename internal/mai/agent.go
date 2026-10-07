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
	"sync"
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
	skillRootsByID map[string]string
	skillsError    error
	skipSkills     bool
	requestTimeout time.Duration
	cellTimeout    time.Duration
	python         pythonKernel
	events         io.Writer
	modelTurns     int
	filesOnce      sync.Once
	depth          int
	files          *fileTools
}

type functionCall struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func newAgent(stdout, stderr io.Writer, sessionPath string, requestTimeout time.Duration, inputAllowed bool) *agent {
	a := &agent{
		stdout: stdout, modelOutput: stdout, stderr: stderr, sessionPath: sessionPath,
		requestTimeout: requestTimeout,
		cellTimeout:    defaultCellTimeout,
		maxTurns:       defaultMaxTurns,
	}
	if inputAllowed {
		a.approve = a.terminalApproval
	}
	return a
}

func discoverSkillRoots() ([]string, error) {
	root, err := defaultSkillsRoot()
	if err != nil {
		return nil, err
	}
	cwd, err := currentDir()
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(findRepoRoot(cwd), "agents", "skills"), root}, nil
}

func (a *agent) run(ctx context.Context, sess *session, userPrompt string) error {
	defer a.close()
	_, err := a.runLoop(ctx, sess, userPrompt)
	return err
}

func (a *agent) close() {
	a.python.close()
}

func (a *agent) runLoop(ctx context.Context, sess *session, userPrompt string) ([]json.RawMessage, error) {
	interactive := isTerminalWriter(a.stderr)
	if interactive {
		fmt.Fprintln(a.stderr, "→ thinking")
	}
	skillInstructions, explicitSkills := a.loadSkillInstructions(userPrompt)
	instructions := systemInstructions(sess, skillInstructions)
	if a.skipSkills {
		instructions += "\n\nSkills are disabled. Do not load skills or follow skill mentions. Read repository AGENTS.md instructions directly when relevant."
	}
	// Instructions precede history in every request, so keep them identical
	// across turns and resumes; explicit skills join the history instead.
	// Compaction keeps developer items, so add each distinct text only once.
	if explicitSkills != "" && !hasDeveloperText(sess.History, explicitSkills) {
		message, err := json.Marshal(map[string]any{
			"role": "developer", "content": []map[string]string{{"type": "input_text", "text": explicitSkills}},
		})
		if err != nil {
			return nil, err
		}
		sess.appendEstimatedHistory(message)
		if a.sessionPath != "" {
			if err := saveJSON(a.sessionPath, sess); err != nil {
				return nil, fmt.Errorf("save explicit skills: %w", err)
			}
		}
	}
	if sess.ContextTokens == 0 {
		history, err := sess.requestHistory()
		if err != nil {
			return nil, err
		}
		sess.ContextTokens = estimateHistoryTokens(history) + estimateInstructionTokens(instructions)
	}
	for turn := 0; a.maxTurns == -1 || turn < a.maxTurns; turn++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if interactive && turn > 0 {
			fmt.Fprintln(a.stderr, "→ thinking")
		}
		terminalItems, err := a.runTurn(ctx, sess, instructions)
		if err != nil {
			return nil, err
		}
		if len(terminalItems) > 0 {
			return terminalItems, nil
		}
	}
	return nil, fmt.Errorf("agent stopped after %d model turns", a.maxTurns)
}

func hasDeveloperText(history []json.RawMessage, text string) bool {
	for _, raw := range history {
		var item struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) == nil && item.Role == "developer" && len(item.Content) == 1 && item.Content[0].Text == text {
			return true
		}
	}
	return false
}

// loadSkillInstructions returns the skill catalog for the instructions and
// the complete text of skills the prompt mentions explicitly.
func (a *agent) loadSkillInstructions(userPrompt string) (string, string) {
	a.skillRootsByID = nil
	if a.skipSkills {
		return "", ""
	}
	if a.skillsError != nil {
		fmt.Fprintf(a.stderr, "mai: skills unavailable: %v\n", a.skillsError)
		return "", ""
	}
	skillContext := buildSkillContext(a.skillsRoots, userPrompt)
	a.skillRootsByID = skillContext.rootsByID
	for _, warning := range skillContext.Warnings {
		fmt.Fprintf(a.stderr, "mai: skill warning: %s\n", warning)
	}
	return skillContext.Instructions, skillContext.Explicit
}

func (a *agent) emit(event map[string]any) error {
	if a.events == nil {
		return nil
	}
	return writeJSONLEvent(a.events, event)
}

// runTurn returns the exact terminal response, or nil after executing tool calls.
func (a *agent) runTurn(ctx context.Context, sess *session, instructions string) ([]json.RawMessage, error) {
	backend := a.backend
	if backend == nil {
		return nil, errors.New("model backend is not configured")
	}
	if err := a.compactIfNeeded(ctx, sess, instructions); err != nil {
		return nil, err
	}
	if err := a.emit(map[string]any{"type": "model.started"}); err != nil {
		return nil, err
	}
	modelStarted := time.Now()
	a.modelTurns++
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
			return nil, eventErr
		}
		return nil, err
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
		return nil, err
	}
	sess.History = append(sess.History, result.items...)
	if result.totalTokens > 0 {
		sess.ContextTokens = result.totalTokens
	} else {
		sess.ContextTokens += estimateHistoryTokens(result.items)
	}
	if a.sessionPath != "" {
		if err := saveJSON(a.sessionPath, sess); err != nil {
			return nil, fmt.Errorf("save assistant response: %w", err)
		}
	}
	calls, err := extractFunctionCalls(result.items)
	if err != nil {
		return nil, err
	}
	if len(calls) == 0 {
		return result.items, nil
	}
	if err := a.executeCalls(ctx, sess, calls); err != nil {
		return nil, err
	}
	return nil, nil
}

func (a *agent) compactIfNeeded(ctx context.Context, sess *session, instructions string) error {
	window := a.contextWindow
	if window == 0 {
		window = modelContextWindow
	}
	if sess.ContextTokens < window*autoCompactPercent/100 {
		return nil
	}
	started := time.Now()
	history, usage, report, err := portableHistory(ctx, sess, a.backend)
	if err != nil {
		return fmt.Errorf("compact conversation: %w", err)
	}
	next := *sess
	next.History = history
	next.ReasoningStart = 0
	if err := archiveTranscript(a.sessionPath, sess, &next); err != nil {
		return err
	}
	// Tell the model what the checkpoint replaced and how to retrieve the
	// originals; keep the notice out of the transcript archive, and drop any
	// older notice so it is never duplicated.
	kept := next.History[:0]
	for _, item := range next.History {
		if !isCompactionNotice(item) {
			kept = append(kept, item)
		}
	}
	next.History = append(kept, compactionNotice(report))
	next.TranscriptSkip = len(next.History)
	next.ContextTokens = estimateHistoryTokens(next.History) + estimateInstructionTokens(instructions)
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
	completed["changes"] = []map[string]any{compactionChange(report)}
	return a.emit(completed)
}

func estimateInstructionTokens(instructions string) int64 {
	return (int64(len(instructions)) + 3) / 4
}

func (a *agent) executeCalls(ctx context.Context, sess *session, calls []functionCall) error {
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return err
		}

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
	case "read_skill":
		return a.executeReadSkill(call.Arguments)
	case "view_image":
		return a.executeViewImage(sess, call.Arguments)
	case "bash":
		return a.executeBash(ctx, sess, call.Arguments)
	case "python":
		return a.executePython(ctx, sess, call.Arguments, call.CallID)
	case "read", "write", "edit":
		return a.executeFileTool(ctx, sess, call.Name, call.Arguments)
	default:
		return textToolOutput(toolError("unknown tool", fmt.Errorf("%s is not available", call.Name)))
	}
}

func (a *agent) executeReadSkill(arguments string) json.RawMessage {
	if a.skipSkills {
		return textToolOutput(toolError("skills disabled", errors.New("read_skill is not available in this run")))
	}
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
	roots := a.skillsRoots
	if selected, ok := a.skillRootsByID[args.Path]; ok {
		roots = []string{selected}
	}

	result, err := readSkill(roots, args.Path, file)
	if err != nil {
		return textToolOutput(toolError("read_skill failed", err))
	}
	return skillFileToolOutput(result)
}

func (a *agent) executeViewImage(sess *session, arguments string) json.RawMessage {
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
	return imageContentToolOutput(marshalToolResult(result), result.imageURL)
}

func (a *agent) executeBash(ctx context.Context, sess *session, arguments string) json.RawMessage {
	var args struct {
		Command     string `json:"command"`
		TimeoutMS   int    `json:"timeout_ms"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid bash arguments", err))
	}
	if strings.TrimSpace(args.Command) == "" {
		return textToolOutput(toolError("invalid bash arguments", errors.New("command is empty")))
	}
	label := args.Command
	if strings.TrimSpace(args.Description) != "" {
		label = args.Description
	}
	fmt.Fprintf(a.stderr, "→ bash: %s\n", oneLine(label, 180))
	return textToolOutput(runBash(ctx, bashRequest{
		Command: args.Command, TimeoutMS: args.TimeoutMS, CWD: sess.CWD,
		RepoRoot: sess.RepoRoot, Approve: a.approve,
		Depth: a.depth,
	}))
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
