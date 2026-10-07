package mai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const version = "0.1.0"

const fullHelp = `mai - a small coding agent

Usage:
  mai "prompt" [options]

Examples:
  mai "add tests for the parser"
  mai "start a saved task" --persist
  mai "now fix the failing test" --last
  mai "refactor this" --effort max
  mai "quick review" --provider enclave --model cyberouter/glm-5.3-flash

Options:
  -h, --help             Show this help text.
  --version              Show the mai version.
  --persist              Save this new task in the current project.
  --last                 Resume the saved task and its provider/model settings.
  --fork                 Start a new saved session from this project's saved task.
  --fork-from ID         Start a new saved session from session ID in this project.
  --provider NAME        Select a provider, including when resuming with --last.
  -m, --model NAME       Override the provider's configured model for this run.
  --effort VALUE         Select reasoning effort: l, h or max (default: h).
  --max-turns COUNT      Set the model-turn limit (default: 64; -1: unlimited).
  --timeout DURATION     Set the per-request first-byte/idle timeout (default: 10m).
  --cell-timeout DURATION      Set the wall-clock limit for each Python cell (default: 10m).
  --no-input             Do not ask for interactive approval.
  -s, --skip-skills      Skip skill discovery for this run.
  --jsonl                 Write task, model, and tool events as JSON Lines.

Tasks are stateless unless you use --persist or --last.
The built-in default is DeepSeek deepseek-v4-pro/high.
Provider settings: .mai.config in the current directory, then $HOME/.mai.config.
--last keeps the saved model and effort; --provider adopts its configured model.

Documentation and support: https://github.com/voladelta/mai
`

func Main(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args)
	if err != nil {
		if slices.Contains(args, "--jsonl") {
			_ = writeJSONLEvent(stdout, map[string]any{"type": "error", "message": err.Error()})
		}
		fmt.Fprintf(stderr, "mai: %v\nRun 'mai --help' for usage.\n", err)
		return 2
	}
	if opts.version {
		fmt.Fprintf(stdout, "mai %s\n", version)
		return 0
	}
	if opts.help {
		fmt.Fprint(stdout, fullHelp)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprint(stdout, `mai - a small coding agent

Usage:
  mai "prompt" [options]

Example:
  mai "add tests for the parser"

Built-in default: DeepSeek deepseek-v4-pro/high.
Run 'mai --help' for more information.
`)
		return 0
	}
	return runTask(opts, stdout, stderr)
}

// nestingDepth enforces the delegation depth limit before any state or model
// request exists, so a refused run costs nothing. MAI_DEPTH is this run's
// depth (default 0); MAI_MAX_DEPTH is the maximum (default 2).
func nestingDepth() (int, error) {
	depth, err := parseDepthEnv("MAI_DEPTH", 0)
	if err != nil {
		return 0, err
	}
	limit, err := parseDepthEnv("MAI_MAX_DEPTH", 2)
	if err != nil {
		return 0, err
	}
	if depth > limit {
		return 0, fmt.Errorf("nesting depth %d exceeds the limit of %d (MAI_MAX_DEPTH); complete this work directly instead of starting another mai", depth, limit)
	}
	return depth, nil
}

func parseDepthEnv(name string, fallback int) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer, got %q", name, value)
	}
	return parsed, nil
}

func runTask(opts options, stdout, stderr io.Writer) int {
	reportError := func(err error) int {
		if opts.jsonl {
			_ = writeJSONLEvent(stdout, map[string]any{"type": "error", "message": err.Error()})
		}
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	depth, err := nestingDepth()
	if err != nil {
		return reportError(err)
	}
	taskCfg := configForTask(opts)
	provider := defaultProviderConfig()
	if !opts.last || opts.provider != "" {
		cwd, err := currentDir()
		if err != nil {
			return reportError(err)
		}
		userHome, err := os.UserHomeDir()
		if err != nil {
			return reportError(fmt.Errorf("find home configuration: %w", err))
		}
		providers, err := loadProviderConfig(cwd, userHome)
		if err != nil {
			return reportError(err)
		}
		if opts.provider == "" {
			taskCfg.Provider = providers.DefaultProvider
		}
		var exists bool
		provider, exists = providers.Providers[taskCfg.Provider]
		if !exists {
			return reportError(fmt.Errorf("provider %q is not configured", taskCfg.Provider))
		}
		if taskCfg.Model == "" {
			taskCfg.Model = provider.Model
		}
	}

	active, err := startSession(taskCfg, opts)
	if err != nil {
		return reportError(err)
	}
	defer active.close()
	if active.forkedFrom != "" {
		fmt.Fprintf(stderr, "mai: forked %s at %d items -> %s\n", active.forkedFrom, active.forkedAt, active.session.ID)
	}
	if opts.last && opts.provider != "" {
		// A saved model ID is provider-specific, so adopt the new
		// provider's configured model unless --model overrides it.
		active.session.Provider = opts.provider
		active.session.Backend = nil
		active.session.ReasoningStart = len(active.session.History)
		active.session.ContextTokens = 0
		if opts.model == "" {
			active.session.Model = provider.Model
		}
	}

	runner := newAgent(stdout, stderr, active.path, opts.timeout, !opts.noInput && isTerminal(os.Stdin))
	runner.depth = depth
	runner.skipSkills = opts.skipSkills
	if !opts.skipSkills {
		runner.skillsRoots, runner.skillsError = discoverSkillRoots()
	}
	if opts.jsonl {
		runner.modelOutput = jsonlTextWriter{output: stdout}
	}
	if err := repairInterruptedToolCalls(active.session); err != nil {
		return reportError(fmt.Errorf("repair interrupted task: %w", err))
	}
	if err := runner.configureBackend(active.session, provider); err != nil {
		return reportError(err)
	}

	if err := appendUserPrompt(active.session, opts.prompt); err != nil {
		return reportError(fmt.Errorf("save task: %w", err))
	}
	if err := active.saveInitial(); err != nil {
		return reportError(fmt.Errorf("save task: %w", err))
	}
	if opts.jsonl {
		started := map[string]any{
			"type": "task.started", "session_id": active.session.ID,
			"provider": active.session.Provider,
			"model":    active.session.Model, "effort": active.session.Effort,
		}
		if active.session.ParentID != "" {
			started["parent_id"] = active.session.ParentID
			started["forked_at_turn"] = active.session.ForkedAtTurn
		}
		if err := writeJSONLEvent(stdout, started); err != nil {
			return reportError(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner.cellTimeout = opts.cellTimeout
	runner.maxTurns = opts.maxTurns
	if opts.jsonl {
		runner.events = stdout
	}
	taskStarted := time.Now()
	if err := runner.run(ctx, active.session, opts.prompt); err != nil {
		if ctx.Err() != nil {
			if opts.jsonl {
				_ = writeJSONLEvent(stdout, map[string]any{"type": "error", "message": "interrupted"})
			}
			fmt.Fprintln(stderr, "mai: interrupted")
			return 130
		}
		return reportError(err)
	}
	if opts.jsonl {
		if err := writeJSONLEvent(stdout, map[string]any{"type": "task.completed", "session_id": active.session.ID, "duration_ms": time.Since(taskStarted).Milliseconds()}); err != nil {
			return reportError(err)
		}
	}
	return 0
}

type activeTask struct {
	session    *session
	path       string
	paths      sessionPaths
	lock       *os.File
	forkedFrom string
	forkedAt   int
}

func (task *activeTask) saveInitial() error {
	if task.path == "" {
		return nil
	}
	if err := saveJSON(task.path, task.session); err != nil {
		return err
	}
	return saveCurrentSession(task.paths, task.session.ID)
}

func (task *activeTask) close() {
	if task.lock != nil {
		_ = task.lock.Close()
	}
}

func configForTask(opts options) taskConfig {
	// An empty Model means the selected provider's configured model.
	cfg := taskConfig{Provider: defaultProvider, Model: opts.model, Effort: "h"}
	if opts.provider != "" {
		cfg.Provider = opts.provider
	}
	if opts.effort != "" {
		cfg.Effort = opts.effort
	}
	return cfg
}

func startSession(cfg taskConfig, opts options) (*activeTask, error) {
	if !opts.last {
		if opts.fork || opts.forkFrom != "" {
			return startForkedSession(cfg, opts)
		}
		sess, err := createSession(cfg)
		if err != nil {
			return nil, err
		}
		if !opts.persist {
			return &activeTask{session: sess}, nil
		}
		paths := projectSessionPaths(sess.RepoRoot)
		if err := prepareSessionPaths(paths); err != nil {
			return nil, err
		}
		lock, err := acquireSessionLock(paths, sess.ID)
		if err != nil {
			return nil, err
		}
		return &activeTask{session: sess, path: sessionPath(paths, sess.ID), paths: paths, lock: lock}, nil
	}

	cwd, err := currentDir()
	if err != nil {
		return nil, err
	}
	root := findRepoRoot(cwd)
	paths := projectSessionPaths(root)
	id, err := loadCurrentSessionID(paths)
	if err != nil {
		return nil, err
	}
	if err := prepareSessionPaths(paths); err != nil {
		return nil, err
	}
	lock, err := acquireSessionLock(paths, id)
	if err != nil {
		return nil, err
	}
	path := sessionPath(paths, id)
	sess, err := loadSession(path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	if sess.ID != id || sess.RepoRoot != root {
		lock.Close()
		return nil, errors.New("saved task does not belong to this project")
	}
	if err := os.Chdir(sess.CWD); err != nil {
		lock.Close()
		return nil, fmt.Errorf("resume task directory %s: %w", sess.CWD, err)
	}
	if opts.model != "" && opts.model != sess.Model {
		// Reasoning can be tied to its model; a different upstream model must
		// not replay another model's reasoning.
		sess.ReasoningStart = len(sess.History)
		sess.ContextTokens = 0
		sess.Model = opts.model
	}
	if opts.effort != "" {
		sess.Effort = opts.effort
	}
	return &activeTask{session: sess, path: path, paths: paths, lock: lock}, nil
}

// startForkedSession clones a saved session into a new child. The parent file
// is read without its lock: saveJSON writes atomically via rename, so a running
// parent's file is always a complete snapshot.
func startForkedSession(cfg taskConfig, opts options) (*activeTask, error) {
	cwd, err := currentDir()
	if err != nil {
		return nil, err
	}
	root := findRepoRoot(cwd)
	paths := projectSessionPaths(root)
	parentID := opts.forkFrom
	if parentID == "" {
		parentID, err = loadCurrentSessionID(paths)
		if err != nil {
			return nil, err
		}
	}
	if err := prepareSessionPaths(paths); err != nil {
		return nil, err
	}
	parent, err := loadSession(sessionPath(paths, parentID))
	if err != nil {
		return nil, fmt.Errorf("fork session %s: %w", parentID, err)
	}
	if parent.RepoRoot != root {
		return nil, fmt.Errorf("session %s does not belong to this project", parentID)
	}
	childID, err := newSessionID()
	if err != nil {
		return nil, err
	}
	child := *parent
	child.ID = childID
	child.ParentID = parentID
	child.ForkedAtTurn = len(parent.History)
	child.PythonActivities = nil // kernel state belongs to the parent's process
	child.History = append([]json.RawMessage(nil), parent.History...)
	child.ContextEdits = append([]contextEdit(nil), parent.ContextEdits...)
	child.Transcript = append([]transcriptEntry(nil), parent.Transcript...)
	if child.Backend != nil {
		backend := *child.Backend
		child.Backend = &backend
	}
	childPath := sessionPath(paths, childID)
	child.transcriptPath = transcriptPath(childPath)

	if opts.provider != "" && opts.provider != child.Provider {
		// Same reset as resuming across providers; cfg.Model is the new
		// provider's configured model or an explicit --model.
		child.Provider = opts.provider
		child.Backend = nil
		child.ReasoningStart = len(child.History)
		child.ContextTokens = 0
		child.Model = cfg.Model
		if opts.model != "" {
			child.Model = opts.model
		}
	} else if opts.model != "" && opts.model != child.Model {
		// Reasoning can be tied to its model; a different upstream model must
		// not replay another model's reasoning.
		child.ReasoningStart = len(child.History)
		child.ContextTokens = 0
		child.Model = opts.model
	}
	if opts.effort != "" {
		child.Effort = opts.effort
	}

	lock, err := acquireSessionLock(paths, childID)
	if err != nil {
		return nil, err
	}
	if child.TranscriptEnd > 0 {
		// Copy the parent's committed archive prefix; the parent may keep
		// appending, so the child must not share its file.
		if err := copyTranscriptPrefix(parent.transcriptPath, child.transcriptPath, child.TranscriptEnd); err != nil {
			lock.Close()
			return nil, fmt.Errorf("copy transcript archive: %w", err)
		}
	}
	if err := os.Chdir(child.CWD); err != nil {
		lock.Close()
		return nil, fmt.Errorf("resume task directory %s: %w", child.CWD, err)
	}
	return &activeTask{session: &child, path: childPath, paths: paths, lock: lock, forkedFrom: parentID, forkedAt: len(parent.History)}, nil
}

func copyTranscriptPrefix(srcPath, dstPath string, end int64) error {
	src, err := openTranscript(srcPath, os.O_RDONLY)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.CopyN(dst, src, end); err != nil {
		dst.Close()
		return err
	}
	if err := dst.Sync(); err != nil {
		dst.Close()
		return err
	}
	return dst.Close()
}

func appendUserPrompt(sess *session, prompt string) error {
	userItem, err := json.Marshal(map[string]any{
		"role":    "user",
		"content": []map[string]string{{"type": "input_text", "text": prompt}},
	})
	if err != nil {
		return fmt.Errorf("encode prompt: %w", err)
	}
	sess.appendEstimatedHistory(userItem)
	return nil
}

func createSession(cfg taskConfig) (*session, error) {
	cwd, err := currentDir()
	if err != nil {
		return nil, err
	}
	root := findRepoRoot(cwd)
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	return &session{
		Version: stateVersion, ID: id, CWD: cwd, RepoRoot: root,
		Provider: cfg.Provider, Model: cfg.Model, Effort: cfg.Effort,
	}, nil
}

func currentDir() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	cwd, err = canonicalPath(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve current directory: %w", err)
	}
	return cwd, nil
}

func findRepoRoot(cwd string) string {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return cwd
	}
	root, err := canonicalPath(strings.TrimSpace(string(out)))
	if err != nil {
		return cwd
	}
	return root
}

func canonicalPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}
