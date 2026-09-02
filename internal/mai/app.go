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
	"strings"
	"syscall"
)

const version = "0.1.0"

const fullHelp = `mai - a small coding agent

Usage:
  mai "prompt" [options]

Examples:
  mai "add tests for the parser"
  mai --subagent repo_scout "map the affected code"
  mai "start a saved task" --persist
  mai "now fix the failing test" --last
  mai "refactor this" -m sol -e h

Options:
  -h, --help             Show this help text.
  --version              Show the mai version.
  --persist              Save this new task in the current project.
  --last                 Resume the current saved task in the current project.
  -m, --model MODEL      Use sol, luna, or terra for this task.
  -e, --effort EFFORT    Use l, m, h, x, or max for this task.
  --timeout DURATION     Set the request timeout (default: 10m).
  --no-input             Do not ask for interactive approval.
  --subagent NAME        Run with an installed custom agent.

Tasks are stateless unless you use --persist or --last.
The built-in default is luna/medium.

Documentation and support: https://github.com/voladelta/mai
`

func Main(args []string, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "mai: %v\nRun 'mai --help' for usage.\n", err)
		return 2
	}
	if opts.version {
		fmt.Fprintf(stdout, "mai %s\n", version)
		return 0
	}
	if opts.help {
		return showHelp(stdout, stderr)
	}
	if len(args) == 0 {
		return showUsage(stdout, stderr)
	}
	return runTask(opts, stdout, stderr)
}

func showHelp(stdout, _ io.Writer) int {
	fmt.Fprint(stdout, fullHelp)
	return 0
}

func showUsage(stdout, _ io.Writer) int {
	fmt.Fprint(stdout, `mai - a small coding agent

Usage:
  mai "prompt" [options]

Example:
  mai "add tests for the parser"

Built-in default: luna/medium.
Run 'mai --help' for more information.
`)
	return 0
}

func runTask(opts options, stdout, stderr io.Writer) int {
	taskCfg := configForTask(opts)
	var selectedAgent *customAgent
	if opts.subagent != "" {
		root, err := defaultAgentsRoot()
		if err != nil {
			fmt.Fprintf(stderr, "mai: %v\n", err)
			return 1
		}
		loaded, err := loadCustomAgent(root, opts.subagent)
		if err != nil {
			fmt.Fprintf(stderr, "mai: %v\n", err)
			return 1
		}
		selectedAgent = &loaded
		taskCfg.Model = loaded.Model
		taskCfg.Effort = loaded.Effort
	}
	active, err := startSession(taskCfg, opts)
	if err != nil {
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	defer active.close()
	if err := repairInterruptedToolCalls(active.session); err != nil {
		fmt.Fprintf(stderr, "mai: repair interrupted task: %v\n", err)
		return 1
	}

	if err := appendUserPrompt(active.session, opts.prompt); err != nil {
		fmt.Fprintf(stderr, "mai: save task: %v\n", err)
		return 1
	}
	if err := active.saveInitial(); err != nil {
		fmt.Fprintf(stderr, "mai: save task: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner := newAgent(stdout, stderr, active.path, opts.timeout, !opts.noInput && isTerminal(os.Stdin), selectedAgent)
	if err := runner.run(ctx, active.session, opts.prompt); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "mai: interrupted")
			return 130
		}
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
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
	cfg := taskConfig{Model: "luna", Effort: "m"}
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

	root, err := currentRepoRoot()
	if err != nil {
		return nil, err
	}
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
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("get current directory: %w", err)
	}
	cwd, err = canonicalPath(cwd)
	if err != nil {
		return nil, fmt.Errorf("resolve current directory: %w", err)
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

func currentRepoRoot() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get current directory: %w", err)
	}
	cwd, err = canonicalPath(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve current directory: %w", err)
	}
	return findRepoRoot(cwd), nil
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
