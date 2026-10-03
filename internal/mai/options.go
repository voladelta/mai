package mai

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type options struct {
	prompt      string
	last        bool
	persist     bool
	provider    string
	fast        bool
	maxEffort   bool
	help        bool
	version     bool
	noInput     bool
	skipSkills  bool
	jsonl       bool
	maxTurns    int
	timeout     time.Duration
	cellTimeout time.Duration
}

type optionKind int

const (
	optionHelp optionKind = iota
	optionVersion
	optionLast
	optionPersist
	optionNoInput
	optionSkipSkills
	optionJSONL
	optionFast
	optionMaxEffort
	optionProvider
	optionTimeout
	optionCellTimeout
	optionMaxTurns
)

var optionKinds = map[string]optionKind{
	"-h": optionHelp, "--help": optionHelp,
	"--version":  optionVersion,
	"--last":     optionLast,
	"--persist":  optionPersist,
	"--no-input": optionNoInput,
	"-s":         optionSkipSkills, "--skip-skills": optionSkipSkills,
	"--jsonl":        optionJSONL,
	"--f":            optionFast,
	"--max":          optionMaxEffort,
	"--provider":     optionProvider,
	"--timeout":      optionTimeout,
	"--cell-timeout": optionCellTimeout,
	"--max-turns":    optionMaxTurns,
}

func parseOptions(args []string) (options, error) {
	out := options{
		timeout:     defaultHTTPTimeout,
		cellTimeout: defaultCellTimeout,
		maxTurns:    defaultMaxTurns,
	}
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
	if err := out.normalizeSelections(); err != nil {
		return out, err
	}
	if err := out.validateMode(len(args)); err != nil {
		return out, err
	}
	return out, nil
}

func helpRequested(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return false
		}
		name, _, inline := strings.Cut(arg, "=")
		kind, known := optionKinds[name]
		if known && kind == optionHelp && !inline {
			return true
		}
		if known && kind.takesValue() && !inline {
			i++
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
	return kind == optionProvider || kind == optionTimeout || kind == optionCellTimeout || kind == optionMaxTurns
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
	case optionSkipSkills:
		out.skipSkills = true
	case optionJSONL:
		out.jsonl = true
	case optionFast:
		out.fast = true
	case optionMaxEffort:
		out.maxEffort = true
	case optionProvider:
		if !validProviderName(value) {
			return fmt.Errorf("invalid provider %q (use lowercase letters, digits, hyphens or underscores)", value)
		}
		out.provider = value
	case optionMaxTurns:
		maxTurns, err := strconv.Atoi(value)
		if err != nil || (maxTurns <= 0 && maxTurns != -1) {
			return fmt.Errorf("invalid --max-turns %q (use a positive integer or -1 for unlimited turns)", value)
		}
		out.maxTurns = maxTurns
	case optionTimeout:
		timeout, err := parseTimeout(value)
		if err != nil {
			return err
		}
		out.timeout = timeout
	case optionCellTimeout:
		timeout, err := parseTimeout(value)
		if err != nil {
			return err
		}
		out.cellTimeout = timeout
	}
	return nil
}

func (out *options) normalizeSelections() error {
	if out.fast && out.maxEffort {
		return errors.New("--f and --max cannot be used together")
	}
	return nil
}

func (out options) validateMode(argCount int) error {
	if out.last && out.persist {
		return errors.New("--last and --persist cannot be used together")
	}
	if out.last && (out.fast || out.maxEffort) {
		return errors.New("--last keeps the saved model and reasoning effort; --f and --max cannot be used with --last")
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

const defaultModel = "pro"

func supportedModel(model string) bool {
	return model == "flash" || model == "pro"
}

func supportedEffort(effort string) bool {
	return effort == "l" || effort == "h" || effort == "max"
}

// Default input budget for DeepSeek Responses.
const modelContextWindow int64 = 1_000_000

var effortIDs = map[string]string{
	"l":   "low",
	"h":   "high",
	"max": "max",
}
