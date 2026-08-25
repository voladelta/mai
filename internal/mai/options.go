package mai

import (
	"errors"
	"fmt"
	"strings"
)

type options struct {
	prompt         string
	last           bool
	model          string
	effort         string
	modelExplicit  bool
	effortExplicit bool
	help           bool
}

func parseOptions(args []string) (options, error) {
	var out options
	var promptParts []string

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help":
			out.help = true
		case arg == "--last":
			out.last = true
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
		case strings.HasPrefix(arg, "-"):
			return out, fmt.Errorf("unknown option %q", arg)
		default:
			promptParts = append(promptParts, arg)
		}
	}

	out.prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	if out.help {
		return out, nil
	}
	if out.prompt == "" {
		return out, errors.New("prompt is required")
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
	return out, nil
}

func normalizeModel(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "low":
		return "l"
	case "medium":
		return "m"
	case "high":
		return "h"
	case "xhigh":
		return "x"
	default:
		return strings.ToLower(strings.TrimSpace(value))
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
