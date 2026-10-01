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
  mai "refactor this" -e h
  mai "quick review" -m ds-flash

Options:
  -h, --help             Show this help text.
  --version              Show the mai version.
  --persist              Save this new task in the current project.
  --last                 Resume the current saved task in the current project.
  -e, --effort EFFORT    Use l, h, or max (low, high, max).
  -m, --model MODEL      Use ds-flash or ds-pro for this task.
  --max-turns COUNT      Set the model-turn limit (default: 64; -1: unlimited).
  --timeout DURATION     Set the per-request first-byte/idle timeout (default: 10m).
  --cell-timeout DURATION      Set the wall-clock limit for each Python cell (default: 10m).
  --no-input             Do not ask for interactive approval.
  -s, --skip-skills      Skip skill discovery for this run.
  --jsonl                 Write task, model, and tool events as JSON Lines.

Tasks are stateless unless you use --persist or --last.
The built-in default is ds-flash/high.

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

Built-in default: ds-flash/high.
Run 'mai --help' for more information.
`)
		return 0
	}
	return runTask(opts, stdout, stderr)
}

func runTask(opts options, stdout, stderr io.Writer) int {
	reportError := func(err error) int {
		if opts.jsonl {
			_ = writeJSONLEvent(stdout, map[string]any{"type": "error", "message": err.Error()})
		}
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	taskCfg := configForTask(opts)
	active, err := startSession(taskCfg, opts)
	if err != nil {
		return reportError(err)
	}
	defer active.close()
	runner := newAgent(stdout, stderr, active.path, opts.timeout, !opts.noInput && isTerminal(os.Stdin))
	if opts.jsonl {
		runner.modelOutput = jsonlTextWriter{output: stdout}
	}
	if err := repairInterruptedToolCalls(active.session); err != nil {
		return reportError(fmt.Errorf("repair interrupted task: %w", err))
	}
	if err := runner.configureBackend(active.session); err != nil {
		return reportError(err)
	}

	if err := appendUserPrompt(active.session, opts.prompt); err != nil {
		return reportError(fmt.Errorf("save task: %w", err))
	}
	if err := active.saveInitial(); err != nil {
		return reportError(fmt.Errorf("save task: %w", err))
	}
	if opts.jsonl {
		if err := writeJSONLEvent(stdout, map[string]any{
			"type": "task.started", "session_id": active.session.ID,
			"model": active.session.Model, "effort": active.session.Effort,
		}); err != nil {
			return reportError(err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner.cellTimeout = opts.cellTimeout
	runner.maxTurns = opts.maxTurns
	runner.skipSkills = opts.skipSkills
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
	session     *session
	path        string
	makeCurrent *sessionPaths
	lock        *os.File
}

func (task *activeTask) saveInitial() error {
	if task.path == "" {
		return nil
	}
	if err := saveJSON(task.path, task.session); err != nil {
		return err
	}
	if task.makeCurrent != nil {
		return saveCurrentSession(*task.makeCurrent, task.session.ID)
	}
	return nil
}

func (task *activeTask) close() {
	if task.lock != nil {
		_ = task.lock.Close()
	}
}

func configForTask(opts options) taskConfig {
	cfg := taskConfig{Model: defaultModel, Effort: "h"}
	if opts.modelExplicit {
		cfg.Model = opts.model
	}
	if opts.effortExplicit {
		cfg.Effort = opts.effort
	}
	return cfg
}

func startSession(cfg taskConfig, opts options) (*activeTask, error) {
	if !opts.last {
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
		return &activeTask{session: sess, path: sessionPath(paths, sess.ID), makeCurrent: &paths, lock: lock}, nil
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
	if opts.modelExplicit {
		sess.Model = opts.model
	}
	if opts.effortExplicit {
		sess.Effort = opts.effort
	}
	if err := os.Chdir(sess.CWD); err != nil {
		lock.Close()
		return nil, fmt.Errorf("resume task directory %s: %w", sess.CWD, err)
	}
	return &activeTask{session: sess, path: path, lock: lock}, nil
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
		Model: cfg.Model, Effort: cfg.Effort,
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
