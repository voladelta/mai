package mai

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type options struct {
	prompt         string
	last           bool
	persist        bool
	effort         string
	effortExplicit bool
	help           bool
	version        bool
	noInput        bool
	subagent       string
	timeout        time.Duration
}

type optionKind int

const (
	optionHelp optionKind = iota
	optionVersion
	optionLast
	optionPersist
	optionNoInput
	optionSubagent
	optionEffort
	optionTimeout
)

var optionKinds = map[string]optionKind{
	"-h": optionHelp, "--help": optionHelp,
	"--version":  optionVersion,
	"--last":     optionLast,
	"--persist":  optionPersist,
	"--no-input": optionNoInput,
	"--subagent": optionSubagent,
	"-e":         optionEffort, "--effort": optionEffort,
	"--timeout": optionTimeout,
}

func parseOptions(args []string) (options, error) {
	out := options{timeout: defaultHTTPTimeout}
	if helpRequested(args) {
		out.help = true
		return out, nil
	}
	promptParts, err := parseOptionTokens(args, &out)
	if err != nil {
		return out, err
	}
	out.prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	if out.version {
		return out, nil
	}
	if err := out.normalizeAndValidate(len(args)); err != nil {
		return out, err
	}
	return out, nil
}

func helpRequested(args []string) bool {
	for _, arg := range args {
		if kind, ok := optionKinds[arg]; ok && kind == optionHelp {
			return true
		}
	}
	return false
}

func parseOptionTokens(args []string, out *options) ([]string, error) {
	var prompt []string
	optionsEnded := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if optionsEnded || !strings.HasPrefix(arg, "-") {
			prompt = append(prompt, arg)
			continue
		}
		if arg == "--" {
			optionsEnded = true
			continue
		}

		name, value, inline := strings.Cut(arg, "=")
		kind, ok := optionKinds[name]
		if !ok || (inline && !kind.takesValue()) {
			return nil, fmt.Errorf("unknown option %q", arg)
		}
		if kind.takesValue() && !inline {
			if i+1 >= len(args) {
				return nil, fmt.Errorf("%s requires a value", arg)
			}
			i++
			value = args[i]
		}
		if err := out.setOption(kind, value); err != nil {
			return nil, err
		}
	}
	return prompt, nil
}

func (kind optionKind) takesValue() bool {
	return kind == optionSubagent || kind == optionEffort || kind == optionTimeout
}

func (out *options) setOption(kind optionKind, value string) error {
	switch kind {
	case optionVersion:
		out.version = true
	case optionLast:
		out.last = true
	case optionPersist:
		out.persist = true
	case optionNoInput:
		out.noInput = true
	case optionSubagent:
		out.subagent = value
	case optionEffort:
		out.effort, out.effortExplicit = value, true
	case optionTimeout:
		timeout, err := parseTimeout(value)
		if err != nil {
			return err
		}
		out.timeout = timeout
	}
	return nil
}

func (out *options) normalizeAndValidate(argCount int) error {
	if err := out.normalizeSelections(); err != nil {
		return err
	}
	return out.validateMode(argCount)
}

func (out *options) normalizeSelections() error {
	out.subagent = strings.TrimSpace(out.subagent)
	if out.subagent != "" {
		if err := validateSubagentName(out.subagent); err != nil {
			return err
		}
		out.noInput = true
	}
	if out.effortExplicit {
		out.effort = normalizeEffort(out.effort)
		if _, ok := effortIDs[out.effort]; !ok {
			return fmt.Errorf("invalid effort %q (use l, m, h, x, or max)", out.effort)
		}
	}
	return nil
}

func (out options) validateMode(argCount int) error {
	if out.last && out.persist {
		return errors.New("--last and --persist cannot be used together")
	}
	if out.subagent != "" {
		switch {
		case out.last || out.persist:
			return errors.New("--subagent cannot be used with --last or --persist")
		case out.effortExplicit:
			return errors.New("--subagent cannot be used with --effort")
		case len(out.prompt) > maxSubagentPromptBytes:
			return fmt.Errorf("subagent prompt exceeds %d bytes", maxSubagentPromptBytes)
		}
	}
	if out.prompt == "" && argCount > 0 {
		return errors.New("prompt is required")
	}
	return nil
}

func parseTimeout(value string) (time.Duration, error) {
	timeout, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("invalid timeout %q (use a positive duration such as 30s or 10m)", value)
	}
	return timeout, nil
}

func normalizeEffort(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "low":
		return "l"
	case "medium":
		return "m"
	case "high":
		return "h"
	case "xhigh":
		return "x"
	default:
		return value
	}
}

const modelID = "gpt-6-astra"

// Keep the conservative input budget for the private Codex backend.
const modelContextWindow int64 = 272_000

var effortIDs = map[string]string{
	"l":   "low",
	"m":   "medium",
	"h":   "high",
	"x":   "xhigh",
	"max": "max",
}
