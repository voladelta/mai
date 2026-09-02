package mai

import (
	"strings"
	"testing"
)

func TestParseOptionsInterspersed(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		want options
	}{
		{
			name: "short forms after prompt",
			args: []string{"fix", "the", "test", "--last", "-m", "sol", "-e=h"},
			want: options{prompt: "fix the test", last: true, model: "sol", effort: "h", modelExplicit: true, effortExplicit: true, timeout: defaultHTTPTimeout},
		},
		{
			name: "long forms around prompt",
			args: []string{"--model=terra", "hello", "--effort", "xhigh", "--persist"},
			want: options{prompt: "hello", persist: true, model: "terra", effort: "x", modelExplicit: true, effortExplicit: true, timeout: defaultHTTPTimeout},
		},
		{
			name: "end of options",
			args: []string{"--", "-m", "is", "part", "of", "the", "prompt"},
			want: options{prompt: "-m is part of the prompt", timeout: defaultHTTPTimeout},
		},
		{
			name: "custom subagent implies no input",
			args: []string{"map", "the", "parser", "--subagent", "repo_scout"},
			want: options{prompt: "map the parser", subagent: "repo_scout", noInput: true, timeout: defaultHTTPTimeout},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
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
	if _, err := parseOptions([]string{"hello", "-m", "unknown"}); err == nil {
		t.Fatal("expected invalid model error")
	}
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
		{"hello", "--subagent", "../repo_scout"},
		{"hello", "--subagent", "repo_scout", "--persist"},
		{"hello", "--subagent", "repo_scout", "--last"},
		{"hello", "--subagent", "repo_scout", "--model", "sol"},
		{"hello", "--subagent", "repo_scout", "--effort", "h"},
		{strings.Repeat("x", maxSubagentPromptBytes+1), "--subagent", "repo_scout"},
	} {
		if _, err := parseOptions(args); err == nil {
			t.Fatalf("expected invalid subagent options for %v", args)
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
