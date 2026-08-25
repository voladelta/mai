package mai

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type options struct {
	prompt          string
	last            bool
	model           string
	effort          string
	modelExplicit   bool
	effortExplicit  bool
	help            bool
	version         bool
	saveDefaults    bool
	noInput         bool
	timeout         time.Duration
	timeoutExplicit bool
}

func parseOptions(args []string) (options, error) {
	out := options{timeout: defaultHTTPTimeout}
	var promptParts []string
	optionsEnded := false

	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			out.help = true
			return out, nil
		}
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if optionsEnded {
			promptParts = append(promptParts, arg)
			continue
		}
		switch {
		case arg == "--":
			optionsEnded = true
		case arg == "--version":
			out.version = true
		case arg == "--last":
			out.last = true
		case arg == "--save-defaults":
			out.saveDefaults = true
		case arg == "--no-input":
			out.noInput = true
		case arg == "-m" || arg == "--model":
			if i+1 >= len(args) {
				return out, fmt.Errorf("%s requires a value", arg)
			}
			i++
			out.model = args[i]
			out.modelExplicit = true
		case strings.HasPrefix(arg, "-m="):
			out.model = strings.TrimPrefix(arg, "-m=")
			out.modelExplicit = true
		case strings.HasPrefix(arg, "--model="):
			out.model = strings.TrimPrefix(arg, "--model=")
			out.modelExplicit = true
		case arg == "-e" || arg == "--effort":
			if i+1 >= len(args) {
				return out, fmt.Errorf("%s requires a value", arg)
			}
			i++
			out.effort = args[i]
			out.effortExplicit = true
		case strings.HasPrefix(arg, "-e="):
			out.effort = strings.TrimPrefix(arg, "-e=")
			out.effortExplicit = true
		case strings.HasPrefix(arg, "--effort="):
			out.effort = strings.TrimPrefix(arg, "--effort=")
			out.effortExplicit = true
		case arg == "--timeout":
			if i+1 >= len(args) {
				return out, errors.New("--timeout requires a value")
			}
			i++
			var err error
			out.timeout, err = parseTimeout(args[i])
			if err != nil {
				return out, err
			}
			out.timeoutExplicit = true
		case strings.HasPrefix(arg, "--timeout="):
			var err error
			out.timeout, err = parseTimeout(strings.TrimPrefix(arg, "--timeout="))
			if err != nil {
				return out, err
			}
			out.timeoutExplicit = true
		case strings.HasPrefix(arg, "-"):
			return out, fmt.Errorf("unknown option %q", arg)
		default:
			promptParts = append(promptParts, arg)
		}
	}

	out.prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	if out.version {
		return out, nil
	}
	if out.modelExplicit {
		out.model = normalizeModel(out.model)
		if _, ok := modelIDs[out.model]; !ok {
			return out, fmt.Errorf("invalid model %q (use sol, luna, or terra)", out.model)
		}
	}
	if out.effortExplicit {
		out.effort = normalizeEffort(out.effort)
		if _, ok := effortIDs[out.effort]; !ok {
			return out, fmt.Errorf("invalid effort %q (use l, m, h, x, or max)", out.effort)
		}
	}
	if out.saveDefaults && !out.modelExplicit && !out.effortExplicit {
		return out, errors.New("--save-defaults requires --model or --effort")
	}
	if out.prompt == "" && out.saveDefaults && out.last {
		return out, errors.New("--last requires a prompt")
	}
	if out.prompt == "" && out.saveDefaults && out.noInput {
		return out, errors.New("--no-input requires a prompt")
	}
	if out.prompt == "" && out.saveDefaults && out.timeoutExplicit {
		return out, errors.New("--timeout requires a prompt")
	}
	if out.prompt == "" && !out.saveDefaults && len(args) > 0 {
		return out, errors.New("prompt is required")
	}
	return out, nil
}

func parseTimeout(value string) (time.Duration, error) {
	timeout, err := time.ParseDuration(strings.TrimSpace(value))
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("invalid timeout %q (use a positive duration such as 30s or 10m)", value)
	}
	return timeout, nil
}

func normalizeModel(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
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

var modelIDs = map[string]string{
	"sol":   "gpt-5.6-sol",
	"luna":  "gpt-5.6-luna",
	"terra": "gpt-5.6-terra",
}

var effortIDs = map[string]string{
	"l":   "low",
	"m":   "medium",
	"h":   "high",
	"x":   "xhigh",
	"max": "max",
}
