package mai

import "testing"

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
			args: []string{"--model=terra", "hello", "--effort", "xhigh"},
			want: options{prompt: "hello", model: "terra", effort: "x", modelExplicit: true, effortExplicit: true, timeout: defaultHTTPTimeout},
		},
		{
			name: "end of options",
			args: []string{"--", "-m", "is", "part", "of", "the", "prompt"},
			want: options{prompt: "-m is part of the prompt", timeout: defaultHTTPTimeout},
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
	if _, err := parseOptions([]string{"--save-defaults"}); err == nil {
		t.Fatal("expected missing default selection error")
	}
	if _, err := parseOptions([]string{"--last", "-m", "sol", "--save-defaults"}); err == nil {
		t.Fatal("expected --last without prompt error")
	}
	if _, err := parseOptions([]string{"-m", "sol", "--save-defaults", "--timeout", "1m"}); err == nil {
		t.Fatal("expected timeout without prompt error")
	}
	if _, err := parseOptions([]string{"hello", "--timeout", "never"}); err == nil {
		t.Fatal("expected invalid timeout error")
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
