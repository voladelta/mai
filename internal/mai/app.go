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
  mai -m MODEL [-e EFFORT] --save-defaults

Examples:
  mai "add tests for the parser"
  mai "now fix the failing test" --last
  mai "refactor this" -m sol -e h

Options:
  -h, --help             Show this help text.
  --version              Show the mai version.
  --last                 Resume the saved task.
  -m, --model MODEL      Use sol, luna, or terra for this task.
  -e, --effort EFFORT    Use l, m, h, x, or max for this task.
  --save-defaults        Save an explicit model or effort for future tasks.
  --timeout DURATION     Set the request timeout (default: 10m).
  --no-input             Do not ask for interactive approval.

The saved task keeps its model and effort when you use --last.
Current defaults: %s.

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

func showHelp(stdout, stderr io.Writer) int {
	cfg, err := loadDisplayConfig()
	defaults := "unavailable"
	if err == nil {
		defaults = formatDefaults(cfg)
	} else {
		fmt.Fprintf(stderr, "mai: cannot read current defaults: %v\n", err)
	}
	fmt.Fprintf(stdout, fullHelp, defaults)
	return 0
}

func showUsage(stdout, stderr io.Writer) int {
	cfg, err := loadDisplayConfig()
	if err != nil {
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, `mai - a small coding agent

Usage:
  mai "prompt" [options]

Example:
  mai "add tests for the parser"

Current defaults: %s.
Run 'mai --help' for more information.
`, formatDefaults(cfg))
	return 0
}

func runTask(opts options, stdout, stderr io.Writer) int {
	state, cfg, err := loadStateConfig()
	if err != nil {
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}

	taskCfg := configForTask(cfg, opts)
	if opts.saveDefaults {
		if err := saveJSON(state.config, taskCfg); err != nil {
			fmt.Fprintf(stderr, "mai: %v\n", err)
			return 1
		}
		if opts.prompt == "" {
			fmt.Fprintf(stdout, "Saved defaults: %s.\n", formatDefaults(taskCfg))
			return 0
		}
		fmt.Fprintf(stderr, "mai: saved defaults: %s\n", formatDefaults(taskCfg))
	}

	sess, err := startSession(state.session, taskCfg, opts)
	if err != nil {
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	if err := repairInterruptedToolCalls(sess); err != nil {
		fmt.Fprintf(stderr, "mai: repair interrupted task: %v\n", err)
		return 1
	}

	if err := appendUserPrompt(state.session, sess, opts.prompt); err != nil {
		fmt.Fprintf(stderr, "mai: save task: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner := newAgent(stdout, stderr, state.session, opts.timeout, !opts.noInput && isTerminal(os.Stdin))
	if err := runner.run(ctx, sess, opts.prompt); err != nil {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "mai: interrupted")
			return 130
		}
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	return 0
}

func configForTask(saved config, opts options) config {
	if opts.modelExplicit {
		saved.Model = opts.model
	}
	if opts.effortExplicit {
		saved.Effort = opts.effort
	}
	return saved
}

func startSession(path string, cfg config, opts options) (*session, error) {
	if !opts.last {
		return createSession(cfg)
	}
	sess, err := loadSession(path)
	if err != nil {
		return nil, err
	}
	if opts.modelExplicit {
		sess.Model = opts.model
	}
	if opts.effortExplicit {
		sess.Effort = opts.effort
	}
	if err := os.Chdir(sess.CWD); err != nil {
		return nil, fmt.Errorf("resume task directory %s: %w", sess.CWD, err)
	}
	return sess, nil
}

func appendUserPrompt(path string, sess *session, prompt string) error {
	userItem, err := json.Marshal(map[string]any{
		"role":    "user",
		"content": []map[string]string{{"type": "input_text", "text": prompt}},
	})
	if err != nil {
		return fmt.Errorf("encode prompt: %w", err)
	}
	sess.History = append(sess.History, userItem)
	return saveJSON(path, sess)
}

func loadStateConfig() (paths, config, error) {
	state, err := statePaths()
	if err != nil {
		return paths{}, config{}, err
	}
	if err := migrateLegacyState(state); err != nil {
		return paths{}, config{}, err
	}
	cfg, err := loadConfig(state.config)
	if err != nil {
		return paths{}, config{}, err
	}
	return state, cfg, nil
}

func loadDisplayConfig() (config, error) {
	state, err := statePaths()
	if err != nil {
		return config{}, err
	}
	if state.legacyConfig == "" {
		return loadConfig(state.config)
	}
	if _, err := os.Stat(state.config); err == nil {
		return loadConfig(state.config)
	} else if !errors.Is(err, os.ErrNotExist) {
		return config{}, fmt.Errorf("read config: %w", err)
	}
	return loadConfig(state.legacyConfig)
}

func formatDefaults(cfg config) string {
	return cfg.Model + "/" + effortIDs[cfg.Effort]
}

func createSession(cfg config) (*session, error) {
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
