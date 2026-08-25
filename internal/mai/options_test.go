package mai

import "testing"

func TestParseOptionsInterspersed(t *testing.T) {
	opts, err := parseOptions([]string{"fix", "the", "test", "--last", "-m", "sol", "-e=h"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.prompt != "fix the test" || !opts.last || opts.model != "sol" || opts.effort != "h" {
		t.Fatalf("unexpected options: %#v", opts)
	}
}

func TestParseOptionsLongNames(t *testing.T) {
	opts, err := parseOptions([]string{"--model=terra", "hello", "--effort", "xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	if opts.model != "terra" || opts.effort != "x" || opts.prompt != "hello" {
		t.Fatalf("unexpected options: %#v", opts)
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
