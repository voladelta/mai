package mai

import (
	"strings"
	"testing"
)

func TestSystemInstructionsAreLeanAndComplete(t *testing.T) {
	prompt := systemInstructions(&session{CWD: "/work/repo", RepoRoot: "/work/repo"})
	for _, required := range []string{
		"autonomous coding agent",
		"Change files only when the user asks",
		"complete the in-scope local work",
		"bash:",
		"apply_patch:",
		"requested outcome is complete or genuinely blocked",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt is missing %q:\n%s", required, prompt)
		}
	}
	if words := len(strings.Fields(prompt)); words > 150 {
		t.Fatalf("prompt grew beyond the 150-word budget: %d words", words)
	}
}

func TestSystemInstructionsQuoteWorkspacePaths(t *testing.T) {
	prompt := systemInstructions(&session{CWD: "/work/repo with spaces", RepoRoot: "/work/root"})
	if !strings.Contains(prompt, `"/work/repo with spaces"`) {
		t.Fatalf("working directory is not quoted: %s", prompt)
	}
}
