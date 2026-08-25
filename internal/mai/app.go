package mai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const usage = `mai - a small coding agent

Usage:
  mai "prompt" [--last] [-m sol|luna|terra] [-e l|m|h|x|max]

Examples:
  mai "add tests for the parser"
  mai "now fix the failing test" --last
  mai "refactor this" -m sol -e h

Defaults start at luna/max. Explicit -m and -e values become future defaults.
--last resumes the single task saved in ~/.mai/session.json.
`

func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	opts, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "mai: %v\n\n%s", err, usage)
		return 2
	}
	if opts.help {
		fmt.Fprint(stdout, usage)
		return 0
	}

	state, err := statePaths()
	if err != nil {
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	cfg, err := loadConfig(state.config)
	if err != nil {
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	if opts.modelExplicit {
		cfg.Model = opts.model
	}
	if opts.effortExplicit {
		cfg.Effort = opts.effort
	}
	if err := saveJSON(state.config, cfg); err != nil {
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}

	var sess *session
	if opts.last {
		sess, err = loadSession(state.session)
		if err != nil {
			fmt.Fprintf(stderr, "mai: %v\n", err)
			return 1
		}
		if opts.modelExplicit {
			sess.Model = opts.model
		}
		if opts.effortExplicit {
			sess.Effort = opts.effort
		}
		if err := os.Chdir(sess.CWD); err != nil {
			fmt.Fprintf(stderr, "mai: resume task directory %s: %v\n", sess.CWD, err)
			return 1
		}
	} else {
		sess, err = createSession(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "mai: %v\n", err)
			return 1
		}
	}
	if err := repairInterruptedToolCalls(sess); err != nil {
		fmt.Fprintf(stderr, "mai: repair interrupted task: %v\n", err)
		return 1
	}

	userItem, err := json.Marshal(map[string]any{
		"role":    "user",
		"content": []map[string]string{{"type": "input_text", "text": opts.prompt}},
	})
	if err != nil {
		fmt.Fprintf(stderr, "mai: encode prompt: %v\n", err)
		return 1
	}
	sess.History = append(sess.History, userItem)
	sess.UpdatedAt = time.Now().UTC()
	if err := saveJSON(state.session, sess); err != nil {
		fmt.Fprintf(stderr, "mai: save task: %v\n", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runner := newAgent(stdout, stderr, stdin, state.session)
	if err := runner.run(ctx, sess); err != nil {
		fmt.Fprintf(stderr, "mai: %v\n", err)
		return 1
	}
	return 0
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
	now := time.Now().UTC()
	return &session{
		Version: stateVersion, ID: id, CWD: cwd, RepoRoot: root,
		Model: cfg.Model, Effort: cfg.Effort, CreatedAt: now, UpdatedAt: now,
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
