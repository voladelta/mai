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
			want: options{prompt: "fix the test", last: true, model: "sol", effort: "h", modelExplicit: true, effortExplicit: true},
		},
		{
			name: "long forms around prompt",
			args: []string{"--model=terra", "hello", "--effort", "xhigh"},
			want: options{prompt: "hello", model: "terra", effort: "x", modelExplicit: true, effortExplicit: true},
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
	if _, err := parseOptions(nil); err == nil {
		t.Fatal("expected missing prompt error")
	}
}
