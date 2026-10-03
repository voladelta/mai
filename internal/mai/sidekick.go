package mai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	maxSidekickTurns       = 32
	sidekickTimeout        = 10 * time.Minute
	maxSidekickAnswerBytes = 64 << 10
)

const sidekickInstructions = `Sidekick worker
- Execute only the director's bounded assignment, respecting its scope and success criteria. Report findings, changed files, checks and unresolved issues. Never claim checks you did not run.
- Do not delegate, launch another mai, or create background workers. You have no sidekick tool. This is a working policy, not a shell sandbox.
- Your conversation and Python namespace last only for this parent run. Do not use --persist or --last or change .mai/current. Shared repository files are real; inspect effects before retrying interrupted work.`

type sidekickWorker struct {
	session *session
	agent   *agent
	failed  bool
}

type sidekickResult struct {
	OK              bool       `json:"ok"`
	WorkerID        string     `json:"worker_id"`
	Status          string     `json:"status"`
	Model           string     `json:"model"`
	Effort          string     `json:"effort"`
	Answer          string     `json:"answer,omitempty"`
	AnswerTruncated bool       `json:"answer_truncated,omitempty"`
	Error           string     `json:"error,omitempty"`
	Instruction     string     `json:"instruction,omitempty"`
	Turns           int        `json:"turns"`
	DurationMS      int64      `json:"duration_ms"`
	Usage           tokenUsage `json:"usage"`
	UsageReports    int        `json:"usage_reports"`
}

func (a *agent) executeSidekick(ctx context.Context, parent *session, arguments string) json.RawMessage {
	if parent.Model != "pro" || a.workerID != "" {
		return textToolOutput(toolError("sidekick unavailable", errors.New("only the Pro director can call sidekick")))
	}
	var args struct {
		Task     string `json:"task"`
		Context  string `json:"context"`
		WorkerID string `json:"worker_id"`
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return textToolOutput(toolError("invalid sidekick arguments", err))
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return textToolOutput(toolError("invalid sidekick arguments", errors.New("expected one JSON object")))
	}
	if strings.TrimSpace(args.Task) == "" || len(args.Task) > 32768 || len(args.Context) > 65536 {
		return textToolOutput(toolError("invalid sidekick arguments", errors.New("task must be nonempty and at most 32 KiB; context at most 64 KiB")))
	}
	if err := ctx.Err(); err != nil {
		return textToolOutput(toolError("sidekick cancelled before dispatch", err))
	}

	worker := a.sidekick
	if args.WorkerID != "" {
		if worker == nil || worker.session.ID != args.WorkerID {
			return textToolOutput(toolError("sidekick unavailable", errors.New("worker_id is unknown or expired; worker history lasts only for this run")))
		}
	} else {
		if worker != nil {
			return textToolOutput(toolError("sidekick already exists", fmt.Errorf("use worker_id %s for follow-ups", worker.session.ID)))
		}
		var err error
		worker, err = a.newSidekick(parent)
		if err != nil {
			return textToolOutput(toolError("start sidekick", err))
		}
		a.sidekick = worker
	}
	if worker.failed || worker.agent.modelTurns >= maxSidekickTurns {
		return textToolOutput(toolError("sidekick unavailable", errors.New("worker failed or exhausted its 32-turn budget; inspect effects and complete the task as director")))
	}

	prompt := args.Task
	if args.Context != "" {
		prompt += "\n\nDirector context:\n" + args.Context
	}
	if err := appendUserPrompt(worker.session, prompt); err != nil {
		return textToolOutput(toolError("prepare sidekick assignment", err))
	}
	worker.agent.maxTurns = maxSidekickTurns - worker.agent.modelTurns
	fmt.Fprintf(a.stderr, "→ sidekick %s: %s\n", worker.session.ID, oneLine(args.Task, 180))
	started := time.Now()
	childCtx, cancel := context.WithTimeout(ctx, sidekickTimeout)
	defer cancel()
	terminalItems, err := worker.agent.runLoop(childCtx, worker.session, prompt)
	if childCtx.Err() != nil {
		err = childCtx.Err()
	}
	var answer string
	if err == nil {
		var parts []string
		for _, item := range terminalItems {
			entry, visible, parseErr := visibleTranscriptEntry(item)
			if parseErr != nil {
				err = parseErr
				break
			}
			if visible && entry.Kind == "assistant" {
				parts = append(parts, entry.Text)
			}
		}
		answer = strings.Join(parts, "\n")
		if err == nil && strings.TrimSpace(answer) == "" {
			err = errors.New("sidekick completed without an assistant answer")
		}
	}

	result := sidekickResult{
		OK: err == nil, WorkerID: worker.session.ID, Status: "completed",
		Model: "flash", Effort: "high", Turns: worker.agent.modelTurns,
		DurationMS: time.Since(started).Milliseconds(),
		Usage:      worker.agent.usage, UsageReports: worker.agent.usageReports,
	}
	if err != nil {
		worker.failed = true
		worker.agent.close()
		result.Status, result.Error = "failed", err.Error()
		result.Instruction = "The worker may have changed files or run commands. Inspect effects before retrying; this worker cannot be continued."
	} else {
		result.Answer = answer
		if len(result.Answer) > maxSidekickAnswerBytes {
			end := maxSidekickAnswerBytes
			for !utf8.ValidString(result.Answer[:end]) {
				end--
			}
			result.Answer, result.AnswerTruncated = result.Answer[:end], true
		}
	}
	return textToolOutput(marshalToolResult(result))
}

func (a *agent) newSidekick(parent *session) (*sidekickWorker, error) {
	client, ok := a.backend.(*responsesClient)
	if !ok {
		return nil, errors.New("Flash backend is unavailable")
	}
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	sess := &session{
		Version: stateVersion, ID: id, CWD: parent.CWD, RepoRoot: parent.RepoRoot,
		Provider: parent.Provider, Backend: parent.Backend, Model: "flash", Effort: "h",
	}
	child := newAgent(io.Discard, sidekickLogWriter{output: a.stderr, id: id}, "", a.requestTimeout, false)
	child.workerID, child.roleInstructions = id, sidekickInstructions
	child.approve = a.approve
	child.skillsRoots, child.skillsError, child.skipSkills = a.skillsRoots, a.skillsError, a.skipSkills
	child.cellTimeout, child.contextWindow = a.cellTimeout, a.contextWindow
	if a.events != nil {
		child.events = sidekickEventWriter{output: a.events, id: id}
		child.modelOutput = jsonlTextWriter{output: child.events}
	}
	backend := *client
	backend.stdout = child.modelOutput
	child.backend = &backend
	return &sidekickWorker{session: sess, agent: child}, nil
}

func (a *agent) recordUsage(usage *tokenUsage) {
	if usage == nil {
		return
	}
	a.usageReports++
	addUsage(&a.usage, usage)
}

type sidekickEventWriter struct {
	output io.Writer
	id     string
}

func (w sidekickEventWriter) Write(p []byte) (int, error) {
	var event map[string]any
	if err := json.Unmarshal(p, &event); err != nil {
		return 0, err
	}
	event["worker_id"] = w.id
	if err := writeJSONLEvent(w.output, event); err != nil {
		return 0, err
	}
	return len(p), nil
}

type sidekickLogWriter struct {
	output io.Writer
	id     string
}

func (w sidekickLogWriter) Write(p []byte) (int, error) {
	if _, err := fmt.Fprintf(w.output, "[sidekick %s] %s", w.id, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
