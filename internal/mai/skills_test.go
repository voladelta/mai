package mai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAgentLoadsRepositorySkillsBeforeGlobalSkills(t *testing.T) {
	repo := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cmd := exec.Command("git", "init", repo)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}

	local := filepath.Join(repo, "agents", "skills")
	global := filepath.Join(home, ".agents", "skills")
	writeTestSkill(t, local, "shared", "local-name", "Repository version.")
	writeTestSkill(t, global, "shared", "global-name", "Shadowed by directory id.")
	writeTestSkill(t, local, "local-dir", "same-name", "Repository named skill.")
	writeTestSkill(t, global, "global-dir", "same-name", "Shadowed by name.")
	writeTestSkill(t, global, "global-only", "global-only", "Global fallback.")
	mustWrite(t, filepath.Join(local, "shared", "guide.md"), "Local guide.")
	mustWrite(t, filepath.Join(global, "shared", "guide.md"), "Global guide.")
	mustWrite(t, filepath.Join(global, "shared", "global-only.md"), "Do not mix skill files.")

	subdir := filepath.Join(repo, "src")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(subdir)
	a := newAgent(io.Discard, io.Discard, "", time.Second, false)
	a.skillsRoots, a.skillsError = discoverSkillRoots()
	catalog, explicit := a.loadSkillInstructions("Use $same-name and $shared.")
	instructions := catalog + explicit
	for _, want := range []string{"Repository version.", "Repository named skill.", "Global fallback.", "# local-name instructions", "# same-name instructions"} {
		if !strings.Contains(instructions, want) {
			t.Fatalf("missing %q in instructions:\n%s", want, instructions)
		}
	}
	if strings.Contains(instructions, "Shadowed") || strings.Contains(instructions, "ambiguous") {
		t.Fatalf("global collision was not overridden:\n%s", instructions)
	}

	for _, test := range []struct {
		arguments string
		want      string
	}{
		{arguments: `{"path":"shared"}`, want: "# local-name instructions"},
		{arguments: `{"path":"shared","file":"guide.md"}`, want: "Local guide."},
		{arguments: `{"path":"global-only"}`, want: "# global-only instructions"},
		{arguments: `{"path":"shared","file":"global-only.md"}`, want: `\"ok\":false`},
	} {
		result := a.executeTool(context.Background(), &session{}, functionCall{Name: "read_skill", Arguments: test.arguments})
		if !strings.Contains(string(result), test.want) {
			t.Fatalf("read_skill(%s) = %s, want %q", test.arguments, result, test.want)
		}
	}
}

func TestAgentReadsDiscoveredGlobalSkillPastInvalidLocalCopy(t *testing.T) {
	for _, test := range []struct {
		name         string
		manifest     string
		explicitOnly bool
	}{
		{name: "missing manifest"},
		{name: "malformed manifest", manifest: "invalid front matter"},
		{name: "explicit-only skill", manifest: "invalid front matter", explicitOnly: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, global := testSkillRoot(t), testSkillRoot(t)
			mustWrite(t, filepath.Join(local, "demo", "guide.md"), "Local guide.")
			if test.manifest != "" {
				mustWrite(t, filepath.Join(local, "demo", "SKILL.md"), test.manifest)
			}

			writeTestSkill(t, global, "demo", "global-demo", "Global skill.")
			mustWrite(t, filepath.Join(global, "demo", "guide.md"), "Global guide.")
			if test.explicitOnly {
				mustWrite(t, filepath.Join(global, "demo", "agents", "openai.yaml"), "policy:\n  allow_implicit_invocation: false\n")
			}

			a := &agent{stderr: io.Discard, skillsRoots: []string{local, global}}
			instructions, _ := a.loadSkillInstructions("Inspect files.")
			if strings.Contains(instructions, "Global skill.") == test.explicitOnly {
				t.Fatalf("unexpected catalog visibility:\n%s", instructions)
			}

			for _, prompt := range []string{"Inspect files.", "Use $global-demo."} {
				_, explicit := a.loadSkillInstructions(prompt)
				if strings.Contains(prompt, "$global-demo") && !strings.Contains(explicit, "# global-demo instructions") {
					t.Fatalf("global explicit instructions missing:\n%s", explicit)
				}

				for _, read := range []struct {
					arguments string
					want      string
				}{
					{arguments: `{"path":"demo"}`, want: "# global-demo instructions"},
					{arguments: `{"path":"demo","file":"guide.md"}`, want: "Global guide."},
				} {
					output := a.executeTool(context.Background(), &session{}, functionCall{Name: "read_skill", Arguments: read.arguments})
					if !strings.Contains(string(output), read.want) {
						t.Fatalf("read_skill(%s) after %q = %s, want %q", read.arguments, prompt, output, read.want)
					}
				}
			}
		})
	}
}

func TestSkillDiscoveryWithMissingRootsAndBrokenRoot(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "Available skill.")
	missing := filepath.Join(t.TempDir(), "missing")
	broken := filepath.Join(t.TempDir(), "file")
	mustWrite(t, broken, "not a directory")

	for _, test := range []struct {
		name         string
		roots        []string
		wantSkill    bool
		wantWarnings int
	}{
		{name: "global only", roots: []string{missing, root}, wantSkill: true},
		{name: "local only", roots: []string{root, missing}, wantSkill: true},
		{name: "neither", roots: []string{missing}},
		{name: "broken global preserves local", roots: []string{root, broken}, wantSkill: true, wantWarnings: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := buildSkillContext(test.roots, "Use $demo.")

			if strings.Contains(result.Explicit, "# demo instructions") != test.wantSkill || len(result.Warnings) != test.wantWarnings {
				t.Fatalf("unexpected context: %#v", result)
			}
		})
	}
}

func TestAgentLoadsLocalOptOutWithoutGlobalSkillsOutsideGit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(dir, "agents", "skills")
	writeTestSkill(t, root, "manual", "manual", "Explicit use only.")
	mustWrite(t, filepath.Join(root, "manual", "agents", "openai.yaml"), "policy:\n  allow_implicit_invocation: false\n")
	var warnings strings.Builder
	a := newAgent(io.Discard, &warnings, "", time.Second, false)
	a.skillsRoots, a.skillsError = discoverSkillRoots()

	if instructions, _ := a.loadSkillInstructions("inspect files"); strings.Contains(instructions, "Explicit use only.") {
		t.Fatalf("opt-out local skill appeared in the catalog:\n%s", instructions)
	}

	if _, explicit := a.loadSkillInstructions("Use $manual."); !strings.Contains(explicit, "# manual instructions") {
		t.Fatalf("local explicit skill was not loaded without global skills:\n%s", explicit)
	}

	if warnings.Len() != 0 {
		t.Fatalf("missing global directory caused warnings: %s", warnings.String())
	}
}

func TestBuildSkillContextListsImplicitSkillsAndLoadsExplicitOptOut(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "layout-dir", "layout-helper", "Build responsive page layouts.")
	writeTestSkill(t, root, "manual-dir", "manual-only", "Run only when explicitly requested.")
	mustWrite(t, filepath.Join(root, "manual-dir", "agents", "openai.yaml"), "policy:\n  allow_implicit_invocation: false\n")

	result := buildSkillContext([]string{root}, "Use $manual-only for this request. Mention $manual-only only once.")

	available, explicit := result.Instructions, result.Explicit
	if !strings.HasPrefix(explicit, "### Explicit skill") || strings.Contains(available, "### Explicit skill") {
		t.Fatalf("explicit skill was not separated from the catalog:\n%s\n---\n%s", available, explicit)
	}
	if !strings.Contains(available, "layout-helper: Build responsive page layouts. (id: layout-dir)") {
		t.Fatalf("implicit skill is missing from catalog:\n%s", available)
	}
	if strings.Contains(available, "manual-only") {
		t.Fatalf("opt-out skill leaked into implicit catalog:\n%s", available)
	}
	if !strings.Contains(explicit, "$manual-only (id: manual-dir)") || !strings.Contains(explicit, "# manual-only instructions") {
		t.Fatalf("explicit opt-out instructions are incomplete:\n%s", explicit)
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", result.Warnings)
	}
	if !strings.Contains(result.Instructions, `read_skill({"path":"<id>"})`) ||
		!strings.Contains(result.Instructions, `read_skill({"path":"<id>","file":"<relative file path>"})`) ||
		strings.Contains(result.Instructions, "read_skill_file") {
		t.Fatalf("skill instructions do not describe the single tool:\n%s", result.Instructions)
	}
}

func TestSkipSkillsBypassesDiscoveryAndExplicitLoading(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	mustWrite(t, filepath.Join(root, "broken", "SKILL.md"), "invalid front matter")

	var warnings strings.Builder
	a := &agent{
		stderr:      &warnings,
		skillsRoots: []string{root},
		skipSkills:  true,
	}

	if instructions, explicit := a.loadSkillInstructions("Use $demo for this request"); instructions != "" || explicit != "" {
		t.Fatalf("skill instructions = %q, explicit = %q, want none", instructions, explicit)
	}
	if warnings.Len() != 0 {
		t.Fatalf("skill discovery produced warnings: %s", warnings.String())
	}

	output := a.executeTool(context.Background(), &session{}, functionCall{Name: "read_skill", Arguments: `{"path":"demo"}`})
	if !strings.Contains(string(output), "skills disabled") || strings.Contains(string(output), "# demo instructions") {
		t.Fatalf("skip-skills allowed a known-ID read: %s", output)
	}
}

func TestSkillFrontMatterControlsAutomaticSelection(t *testing.T) {
	for _, test := range []struct {
		name        string
		frontMatter string
		policy      string
		wantVisible bool
	}{
		{name: "omitted", wantVisible: true},
		{name: "false", frontMatter: "disable-model-invocation: false\n", wantVisible: true},
		{name: "true", frontMatter: "disable-model-invocation: true\n"},
		{name: "true with comment", frontMatter: "disable-model-invocation: true # explicit only\n"},
		{name: "front matter opt-out wins", frontMatter: "disable-model-invocation: true\n", policy: "true"},
		{name: "policy opt-out wins", frontMatter: "disable-model-invocation: false\n", policy: "false"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := testSkillRoot(t)
			mustWrite(t, filepath.Join(root, "manual-dir", "SKILL.md"),
				"---\nname: manual-only\ndescription: A selectable skill.\n"+test.frontMatter+"---\n\n# Manual instructions\n")
			if test.policy != "" {
				mustWrite(t, filepath.Join(root, "manual-dir", "agents", "openai.yaml"),
					"policy:\n  allow_implicit_invocation: "+test.policy+"\n")
			}

			result := buildSkillContext([]string{root}, "Use a selectable skill.")

			if strings.Contains(result.Instructions, "manual-only") != test.wantVisible {
				t.Fatalf("catalog visibility should be %v:\n%s", test.wantVisible, result.Instructions)
			}
			if len(result.Warnings) != 0 {
				t.Fatalf("unexpected warnings: %v", result.Warnings)
			}

			for _, mention := range []string{"manual-only", "manual-dir"} {
				explicit := buildSkillContext([]string{root}, "Use $"+mention+".")

				if !strings.Contains(explicit.Explicit, "### Explicit skill: $manual-only (id: manual-dir)") ||
					!strings.Contains(explicit.Explicit, "# Manual instructions") {
					t.Fatalf("explicit invocation did not load complete instructions:\n%s", explicit.Explicit)
				}
				if len(explicit.Warnings) != 0 {
					t.Fatalf("unexpected explicit invocation warnings: %v", explicit.Warnings)
				}
			}
		})
	}
}

func TestSkillFrontMatterRejectsNonBooleanInvocationFlag(t *testing.T) {
	for _, value := range []string{"sometimes", "", "1", `"true"`} {
		t.Run(value, func(t *testing.T) {
			root := testSkillRoot(t)
			mustWrite(t, filepath.Join(root, "invalid", "SKILL.md"),
				"---\nname: invalid\ndescription: Invalid invocation flag.\ndisable-model-invocation: "+value+"\n---\n")
			writeTestSkill(t, root, "valid", "valid", "Still available.")

			result := buildSkillContext([]string{root}, "Inspect files.")

			if strings.Contains(result.Instructions, "Invalid invocation flag.") || !strings.Contains(result.Instructions, "Still available.") {
				t.Fatalf("invalid flag affected catalog incorrectly:\n%s", result.Instructions)
			}
			if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "non-boolean disable-model-invocation") {
				t.Fatalf("missing invalid flag warning: %v", result.Warnings)
			}
		})
	}
}

func TestImplicitPolicyDefaultsTrueAndParsesFalse(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		want    bool
	}{
		{name: "missing policy", content: "interface:\n  display_name: Demo\n", want: true},
		{name: "true", content: "policy:\n  allow_implicit_invocation: true\n", want: true},
		{name: "false", content: "policy:\n  allow_implicit_invocation: false # explicit only\n", want: false},
		{name: "top-level content ends policy", content: "policy:\nother content\n  allow_implicit_invocation: false\n", want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseImplicitPolicy(test.content)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
	if _, err := parseImplicitPolicy("policy:\n  allow_implicit_invocation: sometimes\n"); err == nil {
		t.Fatal("accepted a non-boolean implicit invocation policy")
	}
}

func TestUnknownDollarNameIsNotTreatedAsMissingSkill(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	result := buildSkillContext([]string{root}, "Print the value of $path, but do not use a skill.")

	if len(result.Warnings) != 0 || result.Explicit != "" {
		t.Fatalf("unknown dollar name affected skill loading: %#v\n%s", result.Warnings, result.Explicit)
	}
}

func TestSkillCatalogIncludesEveryValidDescription(t *testing.T) {
	root := testSkillRoot(t)
	description := strings.Repeat("d", maxSkillDescriptionChars)
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("skill-%02d", i)
		writeTestSkill(t, root, id, id, description)
	}
	result := buildSkillContext([]string{root}, "inspect the repository")

	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected catalog warnings: %#v", result.Warnings)
	}
	catalog := result.Instructions
	if chars := len([]rune(catalog)); chars <= 8_000 {
		t.Fatalf("test catalog is too small to prove removal of the old limit: %d characters", chars)
	}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("skill-%02d", i)
		line := fmt.Sprintf("- %s: %s (id: %s)", id, description, id)
		if !strings.Contains(catalog, line) {
			t.Fatalf("catalog omitted or shortened %s", id)
		}
	}
}

func TestReadSkillRejectsEscapesAndUnscopedFiles(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	mustWrite(t, filepath.Join(root, "secret.txt"), "secret")

	for _, path := range []string{"../secret.txt", "/etc/passwd"} {
		if _, err := readSkill([]string{root}, "demo", path); err == nil {
			t.Fatalf("readSkill accepted %q", path)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "demo", "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(root, "demo", "references", "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := readSkill([]string{root}, "demo", "references/escape.txt"); err == nil {
		t.Fatal("readSkill accepted a symlink escape")
	}
	mustWrite(t, filepath.Join(root, "demo", "assets", "data.bin"), string([]byte{0, 1, 2}))
	if _, err := readSkill([]string{root}, "demo", "assets/data.bin"); err == nil || !strings.Contains(err.Error(), "unsupported media type") {
		t.Fatalf("unexpected binary file error: %v", err)
	}
}

func TestSkillImageContentKeepsHistoryUsable(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	mustWrite(t, filepath.Join(root, "demo", "assets", "icon.png"), string([]byte{0x89, 'P', 'N', 'G', 0, 1}))
	a := &agent{stdout: io.Discard, stderr: io.Discard, skillsRoots: []string{root}}
	sess := deepseekTestSession(t)
	call := functionCall{
		Type:      "function_call",
		CallID:    "skill-image",
		Name:      "read_skill",
		Arguments: `{"path":"demo","file":"assets/icon.png"}`,
	}
	sess.appendEstimatedHistory(mustJSONValue(t, call))

	if err := a.executeCalls(context.Background(), sess, []functionCall{call}); err != nil {
		t.Fatal(err)
	}

	if err := validateResponsesHistory(sess.History, profileDeepSeek); err != nil {
		t.Fatalf("skill output made history unusable: %v", err)
	}
	var output struct {
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	if err := json.Unmarshal(sess.History[len(sess.History)-1], &output); err != nil {
		t.Fatal(err)
	}
	image := false
	for _, part := range output.Output {
		image = image || part.Type == "input_image"
	}
	if !image {
		t.Fatalf("skill image did not produce image content: %s", sess.History[len(sess.History)-1])
	}

	text := a.executeTool(context.Background(), sess, functionCall{Name: "read_skill", Arguments: `{"path":"demo"}`})
	var encoded string
	if err := json.Unmarshal(text, &encoded); err != nil {
		t.Fatal(err)
	}
	var skill skillFileResult
	if err := json.Unmarshal([]byte(encoded), &skill); err != nil || !skill.OK || skill.Content == "" {
		t.Fatalf("text skill read failed: %s", text)
	}
}

func TestAgentRoutesRegisteredSkillTools(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	mustWrite(t, filepath.Join(root, "demo", "references", "guide.md"), "Guide.\n")
	a := &agent{stderr: io.Discard, skillsRoots: []string{root}}

	calls := []struct {
		arguments   string
		wantPath    string
		wantContent string
	}{
		{
			arguments:   `{"path":"demo"}`,
			wantPath:    "SKILL.md",
			wantContent: "---\nname: demo\ndescription: A demonstration skill.\n---\n\n# demo instructions\n",
		},
		{
			arguments:   `{"path":"demo","file":"references/guide.md"}`,
			wantPath:    "references/guide.md",
			wantContent: "Guide.\n",
		},
	}
	for _, call := range calls {
		raw := a.executeTool(context.Background(), &session{}, functionCall{Name: "read_skill", Arguments: call.arguments})
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			t.Fatalf("decode read_skill output: %v", err)
		}
		var result skillFileResult
		if err := json.Unmarshal([]byte(encoded), &result); err != nil {
			t.Fatalf("decode skill result: %v", err)
		}
		if !result.OK || result.Path != call.wantPath || result.Content != call.wantContent {
			t.Fatalf("read_skill(%s) = %#v", call.arguments, result)
		}
	}

	mustWrite(t, filepath.Join(root, "demo", "assets", "icon.png"), string([]byte{0x89, 'P', 'N', 'G', 0, 1}))
	raw := a.executeTool(context.Background(), &session{}, functionCall{
		Name: "read_skill", Arguments: `{"path":"demo","file":"assets/icon.png"}`,
	})
	var content []map[string]string
	if err := json.Unmarshal(raw, &content); err != nil {
		t.Fatal(err)
	}
	if len(content) != 2 || content[1]["type"] != "input_image" || !strings.HasPrefix(content[1]["image_url"], "data:image/png;base64,") {
		t.Fatalf("unexpected image output: %#v", content)
	}
	if strings.Contains(content[0]["text"], "base64") {
		t.Fatalf("image bytes leaked into text output: %s", content[0]["text"])
	}

	var imageMetadata skillFileResult
	if err := json.Unmarshal([]byte(content[0]["text"]), &imageMetadata); err != nil {
		t.Fatal(err)
	}

	if !imageMetadata.OK || imageMetadata.Path != "assets/icon.png" || imageMetadata.Content != "" {
		t.Fatalf("unexpected image metadata: %#v", imageMetadata)
	}

	definitions := toolDefinitions()
	registered := make(map[string]bool, len(definitions))
	var skillDefinition map[string]any
	for _, definition := range definitions {
		name, _ := definition["name"].(string)
		registered[name] = true
		if name == "read_skill" {
			skillDefinition = definition
		}
	}
	if !registered["read_skill"] || registered["read_skill_file"] {
		t.Fatalf("skill tools are not collapsed: %#v", registered)
	}
	parameters := skillDefinition["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	required := parameters["required"].([]string)
	if len(required) != 1 || required[0] != "path" || len(properties) != 2 || properties["path"] == nil || properties["file"] == nil {
		t.Fatalf("unexpected read_skill schema: %#v", parameters)
	}
	if output := a.executeTool(context.Background(), &session{}, functionCall{Name: "read_skill_file"}); !strings.Contains(string(output), "unknown tool") {
		t.Fatalf("retired tool still dispatched: %s", output)
	}
}

func TestParseSkillFrontMatterSupportsFoldedDescription(t *testing.T) {
	name, description, _, err := parseSkillFrontMatter("---\nname: folded\ndescription: >-\n  First line.\n  Second line.\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if name != "folded" || description != "First line. Second line." {
		t.Fatalf("unexpected front matter: %q %q", name, description)
	}
}

func TestParseSkillFrontMatterEnforcesDescriptionLimit(t *testing.T) {
	valid := strings.Repeat("a", maxSkillDescriptionChars)
	if _, description, _, err := parseSkillFrontMatter("---\nname: valid\ndescription: " + valid + "\n---\n"); err != nil || description != valid {
		t.Fatalf("valid description was rejected: length=%d err=%v", len([]rune(description)), err)
	}
	tooLong := strings.Repeat("a", maxSkillDescriptionChars+1)
	if _, _, _, err := parseSkillFrontMatter("---\nname: invalid\ndescription: " + tooLong + "\n---\n"); err == nil || !strings.Contains(err.Error(), "1024") {
		t.Fatalf("overlong description error = %v", err)
	}
}

func testSkillRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func writeTestSkill(t *testing.T, root, id, name, description string) {
	t.Helper()
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n\n# " + name + " instructions\n"
	mustWrite(t, filepath.Join(root, id, "SKILL.md"), content)
}
