package mai

import (
	"runtime"
	"strings"
	"testing"
)

func TestSystemInstructionsAreLeanAndComplete(t *testing.T) {
	sess := &session{CWD: "/work/repo with spaces", RepoRoot: "/work/root with spaces"}
	prompt := systemInstructions(sess, "")
	for _, required := range []string{
		"Change files only when the user asks",
		"bash:",
		"Host OS: " + runtime.GOOS,
		"python:",
		"mai.history(query)",
		"including entries before compaction",
		"case-insensitive literal substring",
		"including --last, starts fresh",
		"Never replay uncertain cells automatically",
		"read:",
		"write:",
		"edit:",
		"Bash reads do not authorize",
		"Paths are relative to the repository root.",
		"unknown outcome",
		"reconcile the intended change",
		"non-idempotent effects without user confirmation",
		`"/work/repo with spaces"`,
		`"/work/root with spaces"`,
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt is missing %q:\n%s", required, prompt)
		}
	}
	if words := len(strings.Fields(prompt)); words > 500 {
		t.Fatalf("base prompt grew beyond the 500-word budget: %d words", words)
	}
	withSkills := systemInstructions(sess, "Skills\n- demo: A demonstration skill. (id: demo)")
	if !strings.Contains(withSkills, "demo: A demonstration skill") {
		t.Fatalf("skill instructions were not appended: %s", withSkills)
	}
	withRoleAndSkills := systemInstructions(sess, "Custom role", "Skills")
	if !strings.Contains(withRoleAndSkills, "Custom role\n\nSkills") {
		t.Fatalf("multiple instruction sections were not appended in order: %s", withRoleAndSkills)
	}
}
