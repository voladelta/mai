package mai

import (
	"strings"
	"testing"
	"time"
)

func TestParseIndependentTimeouts(t *testing.T) {
	got, err := parseOptions([]string{"work", "--timeout", "3s", "--cell-timeout=4m"})
	if err != nil {
		t.Fatal(err)
	}
	if got.timeout != 3*time.Second || got.cellTimeout != 4*time.Minute {
		t.Fatalf("timeouts = request %s, cell %s", got.timeout, got.cellTimeout)
	}

	for _, flag := range []string{"--timeout", "--cell-timeout"} {
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

	for _, value := range []string{"", "0", "-1", "1.5", "many", "999999999999999999999999999999"} {
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
			args: []string{"fix", "the", "test", "--last", "-e=h"},
			want: options{prompt: "fix the test", last: true, effort: "h", effortExplicit: true, timeout: defaultHTTPTimeout},
		},
		{
			name: "long forms around prompt",
			args: []string{"hello", "--effort", "max", "--persist"},
			want: options{prompt: "hello", persist: true, effort: "max", effortExplicit: true, timeout: defaultHTTPTimeout},
		},
		{
			name: "model selection",
			args: []string{"hello", "-m", "DS-FLASH"},
			want: options{prompt: "hello", model: "ds-flash", modelExplicit: true, timeout: defaultHTTPTimeout},
		},
		{
			name: "Pro model alias",
			args: []string{"hello", "--model=ds-pro"},
			want: options{prompt: "hello", model: "ds-pro", modelExplicit: true, timeout: defaultHTTPTimeout},
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
			test.want.cellTimeout = defaultCellTimeout
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
		{"hello", "--subagent", "repo_scout"},
		{"hello", "--subagent-timeout", "1h"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("expected removed subagent option to be rejected for %v", args)
		}
	}
}

func TestParseOptionsAllowsEmptyInvocation(t *testing.T) {
	got, err := parseOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.prompt != "" || got.timeout != defaultHTTPTimeout {
		t.Fatalf("unexpected options: %#v", got)
	}
}

func TestParseOptionsHelpOverridesOtherArguments(t *testing.T) {
	got, err := parseOptions([]string{"--unknown", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.help {
		t.Fatalf("unexpected options: %#v", got)
	}
}

func TestParseOptionsDoesNotTreatOptionValuesAsHelp(t *testing.T) {
	for _, flag := range []string{"--model", "--effort", "--timeout", "--max-turns"} {
		t.Run(flag, func(t *testing.T) {
			if _, err := parseOptions([]string{"work", flag, "--help"}); err == nil {
				t.Fatal("invalid option value was interpreted as a help request")
			}
		})
	}
}

func TestModelSelectionRejectsOtherModels(t *testing.T) {
	for _, model := range []string{"sol", "luna", "gpt-6.1-sol", "deepseek-flash", "deepseek-v4-pro", "unknown", ""} {
		if _, err := parseOptions([]string{"hello", "--model=" + model}); err == nil || !strings.Contains(err.Error(), "invalid model") {
			t.Fatalf("%q: error = %v", model, err)
		}
	}
}
