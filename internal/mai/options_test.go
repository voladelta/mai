package mai

import (
	"strings"
	"testing"
	"time"
)

func TestParseIndependentTimeouts(t *testing.T) {
	got, err := parseOptions([]string{"work", "--timeout", "3s"})
	if err != nil {
		t.Fatal(err)
	}
	if got.timeout != 3*time.Second {
		t.Fatalf("timeouts = request %s", got.timeout)
	}

	for _, flag := range []string{"--timeout"} {
		if _, err := parseOptions([]string{"work", flag, "0s"}); err == nil {
			t.Fatalf("%s accepted a zero duration", flag)
		}
	}
}

func TestParseMaxTurns(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want int
	}{
		{name: "default", args: []string{"work"}, want: 64},
		{name: "separate value", args: []string{"work", "--max-turns", "128"}, want: 128},
		{name: "inline value on resume", args: []string{"continue", "--last", "--max-turns=256"}, want: 256},
		{name: "unlimited separate value", args: []string{"work", "--max-turns", "-1"}, want: -1},
		{name: "unlimited inline value on resume", args: []string{"continue", "--last", "--max-turns=-1"}, want: -1},
		{name: "resume default", args: []string{"continue", "--last"}, want: 64},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseOptions(test.args)
			if err != nil {
				t.Fatal(err)
			}

			if got.maxTurns != test.want {
				t.Fatalf("max turns = %d, want %d", got.maxTurns, test.want)
			}
		})
	}

	for _, value := range []string{"", "0", "-2", "1.5", "many", "999999999999999999999999999999"} {
		if _, err := parseOptions([]string{"work", "--max-turns=" + value}); err == nil || !strings.Contains(err.Error(), "positive integer") {
			t.Fatalf("max turns %q: error = %v", value, err)
		}
	}

	if _, err := parseOptions([]string{"work", "--max-turns"}); err == nil || !strings.Contains(err.Error(), "requires a value") {
		t.Fatalf("missing max turns: error = %v", err)
	}
}

func TestParseOptionsInterspersed(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want options
	}{
		{
			name: "short forms after prompt",
			args: []string{"fix", "the", "test", "--persist", "-m", "deepseek-flash"},
			want: options{prompt: "fix the test", persist: true, model: "deepseek-flash", timeout: defaultHTTPTimeout},
		},
		{
			name: "long forms around prompt",
			args: []string{"hello", "--effort", "max", "--persist"},
			want: options{prompt: "hello", persist: true, effort: "max", timeout: defaultHTTPTimeout},
		},
		{
			name: "provider selection",
			args: []string{"hello", "--provider", "enclave"},
			want: options{prompt: "hello", provider: "enclave", timeout: defaultHTTPTimeout},
		},
		{
			name: "provider override on resume",
			args: []string{"continue", "--last", "--provider", "enclave"},
			want: options{prompt: "continue", last: true, provider: "enclave", timeout: defaultHTTPTimeout},
		},
		{
			name: "inline provider and model",
			args: []string{"hello", "--provider=openrouter", "--model=deepseek/deepseek-v4-pro"},
			want: options{prompt: "hello", provider: "openrouter", model: "deepseek/deepseek-v4-pro", timeout: defaultHTTPTimeout},
		},
		{
			name: "end of options",
			args: []string{"--", "-m", "is", "part", "of", "the", "prompt"},
			want: options{prompt: "-m is part of the prompt", timeout: defaultHTTPTimeout},
		},
		{
			name: "literal help after end of options",
			args: []string{"--", "--help"},
			want: options{prompt: "--help", timeout: defaultHTTPTimeout},
		},
		{
			name: "short skip skills flag with resume",
			args: []string{"continue", "--last", "-s"},
			want: options{prompt: "continue", last: true, skipSkills: true, timeout: defaultHTTPTimeout},
		},
		{
			name: "long skip skills flag",
			args: []string{"--skip-skills", "inspect", "the", "repo"},
			want: options{prompt: "inspect the repo", skipSkills: true, timeout: defaultHTTPTimeout},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.want.maxTurns = defaultMaxTurns
			got, err := parseOptions(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("options = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseOptionsRejectsInvalid(t *testing.T) {
	if _, err := parseOptions([]string{"--last"}); err == nil {
		t.Fatal("expected missing prompt error")
	}
	if _, err := parseOptions([]string{"hello", "--last", "--persist"}); err == nil {
		t.Fatal("expected conflicting persistence mode error")
	}
	if _, err := parseOptions([]string{"hello", "--save-defaults"}); err == nil {
		t.Fatal("expected removed global settings option to be rejected")
	}
	if _, err := parseOptions([]string{"hello", "--timeout", "never"}); err == nil {
		t.Fatal("expected invalid timeout error")
	}
	for _, args := range [][]string{
		{"work", "--f"},
		{"work", "--max"},
		{"hello", "--subagent", "repo_scout"},
		{"hello", "--subagent-timeout", "1h"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("expected removed subagent option to be rejected for %v", args)
		}
	}
}

func TestParseOptionsDoesNotTreatOptionValuesAsHelp(t *testing.T) {
	for _, flag := range []string{"--provider", "--model", "--timeout", "--max-turns"} {
		t.Run(flag, func(t *testing.T) {
			if _, err := parseOptions([]string{"work", flag, "--help"}); err == nil {
				t.Fatal("invalid option value was interpreted as a help request")
			}
		})
	}
}

func TestExecutionModeSelection(t *testing.T) {
	for _, test := range []struct {
		args   []string
		model  string
		effort string
	}{
		{[]string{"work"}, "", "h"},
		{[]string{"work", "--effort", "max"}, "", "max"},
		{[]string{"work", "--effort=l"}, "", "l"},
		{[]string{"work", "--model", "deepseek-flash"}, "deepseek-flash", "h"},
		{[]string{"work", "-m", "cyberouter/glm-5.3-flash"}, "cyberouter/glm-5.3-flash", "h"},
		{[]string{"work", "--provider", "enclave"}, "", "h"},
		{[]string{"work", "--provider", "openrouter", "--model", "deepseek/deepseek-v4-pro", "--effort", "l"}, "deepseek/deepseek-v4-pro", "l"},
	} {
		opts, err := parseOptions(test.args)
		if err != nil {
			t.Fatal(err)
		}

		cfg := configForTask(opts)
		if cfg.Model != test.model || cfg.Effort != test.effort {
			t.Fatalf("%v: config = %#v", test.args, cfg)
		}
	}

	for _, args := range [][]string{
		{"work", "--effort", "bogus"},
		{"work", "--effort="},
		{"work", "--model", ""},
		{"work", "--model", "  "},
		{"work", "-m", "two words"},
		{"work", "-m", "bad\tmodel"},
		{"work", "--model", "-badmodel"},
		{"work", "-m=-flash"},
		{"work", "--model=" + strings.Repeat("x", 257)},
		{"work", "-e", "h"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("accepted invalid model or effort option: %v", args)
		}
	}
}

func TestLastAllowsModelAndEffortOverrides(t *testing.T) {
	for _, test := range []struct {
		args   []string
		model  string
		effort string
	}{
		{[]string{"continue", "--last"}, "", ""},
		{[]string{"continue", "--last", "--provider", "enclave"}, "", ""},
		{[]string{"continue", "--last", "--model", "cyberouter/glm-5.3-flash"}, "cyberouter/glm-5.3-flash", ""},
		{[]string{"continue", "--last", "-m", "deepseek-flash"}, "deepseek-flash", ""},
		{[]string{"continue", "--last", "--effort", "l"}, "", "l"},
		{[]string{"continue", "--last", "--provider", "enclave", "--model", "x/y", "--effort", "max"}, "x/y", "max"},
	} {
		opts, err := parseOptions(test.args)
		if err != nil {
			t.Fatal(err)
		}
		if !opts.last || opts.model != test.model || opts.effort != test.effort {
			t.Fatalf("%v: options = %#v", test.args, opts)
		}
	}
}

func TestInvalidModelIDsAndProviders(t *testing.T) {
	for _, provider := range []string{"", "../enclave", "OpenRouter", "--help", "two words"} {
		if _, err := parseOptions([]string{"hello", "--provider=" + provider}); err == nil || !strings.Contains(err.Error(), "invalid provider") {
			t.Fatalf("%q: error = %v", provider, err)
		}
	}
}
