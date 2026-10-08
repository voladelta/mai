package mai

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type options struct {
	prompt     string
	last       bool
	persist    bool
	fork       bool
	forkFrom   string
	provider   string
	model      string
	effort     string
	help       bool
	version    bool
	noInput    bool
	skipSkills bool
	jsonl      bool
	maxTurns   int
	timeout    time.Duration
}

type optionKind int

const (
	optionHelp optionKind = iota
	optionVersion
	optionLast
	optionPersist
	optionFork
	optionForkFrom
	optionNoInput
	optionSkipSkills
	optionJSONL
	optionProvider
	optionModel
	optionEffort
	optionTimeout
	optionMaxTurns
)

var optionKinds = map[string]optionKind{
	"-h": optionHelp, "--help": optionHelp,
	"--version":   optionVersion,
	"--last":      optionLast,
	"--persist":   optionPersist,
	"--fork":      optionFork,
	"--fork-from": optionForkFrom,
	"--no-input":  optionNoInput,
	"-s":          optionSkipSkills, "--skip-skills": optionSkipSkills,
	"--jsonl":    optionJSONL,
	"--provider": optionProvider,
	"-m":         optionModel, "--model": optionModel,
	"--effort":    optionEffort,
	"--timeout":   optionTimeout,
	"--max-turns": optionMaxTurns,
}

func parseOptions(args []string) (options, error) {
	out := options{
		timeout:  defaultHTTPTimeout,
		maxTurns: defaultMaxTurns,
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
	return kind == optionProvider || kind == optionModel || kind == optionEffort || kind == optionTimeout || kind == optionMaxTurns || kind == optionForkFrom
}

func (out *options) setOption(kind optionKind, value string) error {
	switch kind {
	case optionVersion:
		out.version = true
	case optionLast:
		out.last = true
	case optionPersist:
		out.persist = true
	case optionFork:
		out.fork = true
	case optionForkFrom:
		if !validSessionID(value) {
			return fmt.Errorf("invalid --fork-from %q (use a saved session ID)", value)
		}
		out.forkFrom = value
	case optionNoInput:
		out.noInput = true
	case optionSkipSkills:
		out.skipSkills = true
	case optionJSONL:
		out.jsonl = true
	case optionModel:
		if !validModelID(value) {
			return fmt.Errorf("invalid --model %q (use a nonempty model ID without whitespace or control characters)", value)
		}
		out.model = value
	case optionEffort:
		if !supportedEffort(value) {
			return fmt.Errorf("invalid --effort %q (use l, h or max)", value)
		}
		out.effort = value
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
	}
	return nil
}

func (out options) validateMode(argCount int) error {
	if out.last && out.persist {
		return errors.New("--last and --persist cannot be used together")
	}
	if out.fork || out.forkFrom != "" {
		if out.last {
			return errors.New("--fork/--fork-from and --last cannot be used together")
		}
		if out.persist {
			return errors.New("--fork/--fork-from already saves the task; do not combine with --persist")
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

const maxModelIDBytes = 256

// Model IDs are sent upstream exactly as written; reject only values that
// cannot be a nonempty portable identifier.
func validModelID(model string) bool {
	if model == "" || len(model) > maxModelIDBytes || strings.TrimSpace(model) != model || strings.HasPrefix(model, "-") {
		return false
	}
	for _, char := range model {
		if char <= ' ' || char == 0x7f {
			return false
		}
	}
	return true
}

func supportedEffort(effort string) bool {
	_, ok := effortIDs[effort]
	return ok
}

// Default input budget for DeepSeek Responses.
const modelContextWindow int64 = 1_000_000

var effortIDs = map[string]string{
	"l":   "low",
	"h":   "high",
	"max": "max",
}
