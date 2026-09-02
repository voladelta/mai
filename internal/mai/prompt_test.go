package mai

import (
	"strings"
	"testing"
)

func TestSystemInstructionsAreLeanAndComplete(t *testing.T) {
	sess := &session{CWD: "/work/repo with spaces", RepoRoot: "/work/root with spaces"}
	prompt := systemInstructions(sess, "")
	for _, required := range []string{
		"autonomous coding agent",
		"Change files only when the user asks",
		"complete in-scope work",
		"bash:",
		"apply_patch:",
		"unknown outcome",
		"reconcile the requested patch",
		"non-idempotent effects without user confirmation",
		"ASD-STE100 Simplified Technical English",
		"when the user prefers it",
		"requested outcome is complete or genuinely blocked",
		`"/work/repo with spaces"`,
		`"/work/root with spaces"`,
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt is missing %q:\n%s", required, prompt)
		}
	}
	if words := len(strings.Fields(prompt)); words > 200 {
		t.Fatalf("base prompt grew beyond the 200-word budget: %d words", words)
	}
	withSkills := systemInstructions(sess, "Skills\n- demo: A demonstration skill. (id: demo)")
	if !strings.Contains(withSkills, "demo: A demonstration skill") {
		t.Fatalf("skill instructions were not appended: %s", withSkills)
	}
}
