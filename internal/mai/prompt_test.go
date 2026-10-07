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
	// Smoke alarm, not a budget: this ceiling exists to catch accidental
	// duplication or a paste error. Leanness is a review judgement; do not
	// trim unrelated text to fit under this number.
	if words := len(strings.Fields(prompt)); words > 800 {
		t.Fatalf("base prompt exceeds the 800-word runaway ceiling (%d words); this is a smoke alarm for accidental duplication or a paste error, not a per-word budget — leanness is a review judgement", words)
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

func TestPromptDocumentsEveryAdvertisedTool(t *testing.T) {
	sess := &session{CWD: "/work/repo", RepoRoot: "/work/repo"}
	prompt := systemInstructions(sess, "")

	advertised := map[string]bool{}
	for _, definition := range toolDefinitions() {
		name, _ := definition["name"].(string)
		if name == "" {
			t.Fatalf("tool definition without a name: %v", definition)
		}
		advertised[name] = true
	}

	// Names that are deliberately absent from one side, each with its reason.
	// An exemption must stay accurate: it fails once the name's status changes.
	documentedElsewhere := map[string]string{
		"read_skill": "documented in the Skills block injected by skills.go; the tool is withdrawn entirely under --skip-skills",
	}
	notATool := map[string]string{
		"rm": "a bash command policy, not a registered tool",
	}

	// Parse the "- name:" lines between the Tools heading and the next section.
	documented := map[string]bool{}
	inTools := false
	for _, line := range strings.Split(prompt, "\n") {
		if line == "Tools" {
			inTools = true
			continue
		}
		if !inTools {
			continue
		}
		if line == "" {
			break
		}
		if !strings.HasPrefix(line, "- ") {
			t.Fatalf("unexpected line in the prompt's Tools section: %q", line)
		}
		name, _, ok := strings.Cut(strings.TrimPrefix(line, "- "), ":")
		if !ok || name == "" {
			t.Fatalf("cannot parse a tool name from Tools line %q", line)
		}
		documented[name] = true
	}

	for name := range advertised {
		if !documented[name] && documentedElsewhere[name] == "" {
			t.Errorf("tool %q is advertised to the model but undocumented in the prompt; add a line or an exemption with a reason", name)
		}
	}
	for name := range documented {
		if !advertised[name] && notATool[name] == "" {
			t.Errorf("prompt documents %q, which is not an advertised tool; remove the line or record why it belongs", name)
		}
	}

	for name, reason := range documentedElsewhere {
		if !advertised[name] {
			t.Errorf("stale exemption: %q is no longer an advertised tool (reason was: %s)", name, reason)
		} else if documented[name] {
			t.Errorf("stale exemption: %q now has a prompt line; remove the exemption (reason was: %s)", name, reason)
		}
	}
	for name, reason := range notATool {
		if advertised[name] {
			t.Errorf("stale exemption: %q is now a registered tool; it needs a real prompt line, not an exemption (reason was: %s)", name, reason)
		} else if !documented[name] {
			t.Errorf("stale exemption: %q no longer appears in the prompt's Tools section (reason was: %s)", name, reason)
		}
	}
}
