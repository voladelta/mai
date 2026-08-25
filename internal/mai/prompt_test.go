package mai

import (
	"strings"
	"testing"
)

func TestSystemInstructionsAreLeanAndComplete(t *testing.T) {
	prompt := systemInstructions(&session{CWD: "/work/repo", RepoRoot: "/work/repo"}, "")
	for _, required := range []string{
		"autonomous coding agent",
		"Change files only when the user asks",
		"complete in-scope work",
		"bash:",
		"apply_patch:",
		"ASD-STE100 Simplified Technical English",
		"when the user prefers it",
		"requested outcome is complete or genuinely blocked",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt is missing %q:\n%s", required, prompt)
		}
	}
	if words := len(strings.Fields(prompt)); words > 170 {
		t.Fatalf("base prompt grew beyond the 170-word budget: %d words", words)
	}
	withSkills := systemInstructions(&session{CWD: "/work/repo", RepoRoot: "/work/repo"}, "Skills\n- demo: A demonstration skill. (id: demo)")
	if !strings.Contains(withSkills, "demo: A demonstration skill") {
		t.Fatalf("skill instructions were not appended: %s", withSkills)
	}
}

func TestSystemInstructionsQuoteWorkspacePaths(t *testing.T) {
	prompt := systemInstructions(&session{CWD: "/work/repo with spaces", RepoRoot: "/work/root"}, "")
	if !strings.Contains(prompt, `"/work/repo with spaces"`) {
		t.Fatalf("working directory is not quoted: %s", prompt)
	}
}
