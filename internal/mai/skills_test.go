package mai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSkillContextListsImplicitSkillsAndLoadsExplicitOptOut(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "layout-dir", "layout-helper", "Build responsive page layouts.")
	writeTestSkill(t, root, "manual-dir", "manual-only", "Run only when explicitly requested.")
	mustWrite(t, filepath.Join(root, "manual-dir", "agents", "openai.yaml"), "policy:\n  allow_implicit_invocation: false\n")

	result, err := buildSkillContext(root, "Use $manual-only for this request. Mention $manual-only only once.")
	if err != nil {
		t.Fatal(err)
	}
	available, explicit, ok := strings.Cut(result.Instructions, "### Explicit skill")
	if !ok {
		t.Fatalf("explicit skill was not loaded:\n%s", result.Instructions)
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
	result, err := buildSkillContext(root, "Print the value of $path, but do not use a skill.")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Warnings) != 0 || strings.Contains(result.Instructions, "### Explicit skill") {
		t.Fatalf("unknown dollar name affected skill loading: %#v\n%s", result.Warnings, result.Instructions)
	}
}

func TestSkillCatalogFitsBudgetByShorteningDescriptions(t *testing.T) {
	var skills []skillSummary
	for i := 0; i < 40; i++ {
		skills = append(skills, skillSummary{
			ID: fmt.Sprintf("skill-%02d", i), Name: fmt.Sprintf("skill-%02d", i),
			Description: strings.Repeat("description ", 40), AllowImplicit: true,
		})
	}
	catalog, truncated, omitted := renderSkillCatalog(skills)
	if !truncated || omitted != 0 {
		t.Fatalf("unexpected catalog result: truncated=%v omitted=%d", truncated, omitted)
	}
	if chars := len([]rune(catalog)); chars > maxSkillCatalogChars {
		t.Fatalf("catalog has %d characters, limit is %d", chars, maxSkillCatalogChars)
	}
	for _, skill := range skills {
		if !strings.Contains(catalog, "(id: "+skill.ID+")") {
			t.Fatalf("catalog omitted %s despite enough minimum-line space", skill.ID)
		}
	}
}

func TestReadSkillReturnsCompleteFileAndSupportingFiles(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	mustWrite(t, filepath.Join(root, "demo", "references", "guide.md"), "# Guide\nRead all of this.\n")
	mustWrite(t, filepath.Join(root, "demo", "root-note.md"), "Root-level note.\n")
	binary := []byte{0x89, 'P', 'N', 'G', 0, 1}
	if err := os.MkdirAll(filepath.Join(root, "demo", "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "demo", "assets", "icon.png"), binary, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := readSkill(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "SKILL.md" || !strings.Contains(result.Content, "# demo instructions") {
		t.Fatalf("unexpected skill result: %#v", result)
	}

	result, err = readSkillFile(root, "demo", "references/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "# Guide\nRead all of this.\n" {
		t.Fatalf("unexpected reference result: %#v", result)
	}
	result, err = readSkillFile(root, "demo", "root-note.md")
	if err != nil {
		t.Fatal(err)
	}

	result, err = readSkillFile(root, "demo", "assets/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.imageURL, "data:image/png;base64,") || result.Content != "" {
		t.Fatalf("unexpected asset result: %#v", result)
	}
}

func TestReadSkillFileRejectsEscapesAndUnscopedFiles(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	mustWrite(t, filepath.Join(root, "secret.txt"), "secret")

	for _, path := range []string{"../secret.txt", "/etc/passwd"} {
		if _, err := readSkillFile(root, "demo", path); err == nil {
			t.Fatalf("readSkillFile accepted %q", path)
		}
	}

	if err := os.MkdirAll(filepath.Join(root, "demo", "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "secret.txt"), filepath.Join(root, "demo", "references", "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := readSkillFile(root, "demo", "references/escape.txt"); err == nil {
		t.Fatal("readSkillFile accepted a symlink escape")
	}
	mustWrite(t, filepath.Join(root, "demo", "assets", "data.bin"), string([]byte{0, 1, 2}))
	if _, err := readSkillFile(root, "demo", "assets/data.bin"); err == nil || !strings.Contains(err.Error(), "unsupported media type") {
		t.Fatalf("unexpected binary file error: %v", err)
	}
}

func TestAgentRoutesRegisteredSkillTools(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	mustWrite(t, filepath.Join(root, "demo", "references", "guide.md"), "Guide.\n")
	a := &agent{stderr: io.Discard, skillsRoot: root}

	calls := []functionCall{
		{Name: "read_skill", Arguments: `{"skill":"demo"}`},
		{Name: "read_skill_file", Arguments: `{"skill":"demo","path":"references/guide.md"}`},
	}
	for _, call := range calls {
		raw := a.executeTool(context.Background(), &session{}, call)
		var result string
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatalf("decode %s output: %v", call.Name, err)
		}
		if !strings.Contains(result, `"ok":true`) {
			t.Fatalf("%s failed: %s", call.Name, result)
		}
	}

	mustWrite(t, filepath.Join(root, "demo", "assets", "icon.png"), string([]byte{0x89, 'P', 'N', 'G', 0, 1}))
	raw := a.executeTool(context.Background(), &session{}, functionCall{
		Name: "read_skill_file", Arguments: `{"skill":"demo","path":"assets/icon.png"}`,
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

	definitions := toolDefinitions()
	registered := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		name, _ := definition["name"].(string)
		registered[name] = true
	}
	for _, name := range []string{"read_skill", "read_skill_file"} {
		if !registered[name] {
			t.Fatalf("tool %q is not registered", name)
		}
	}
}

func TestParseSkillFrontMatterSupportsFoldedDescription(t *testing.T) {
	name, description, err := parseSkillFrontMatter("---\nname: folded\ndescription: >-\n  First line.\n  Second line.\n---\n")
	if err != nil {
		t.Fatal(err)
	}
	if name != "folded" || description != "First line. Second line." {
		t.Fatalf("unexpected front matter: %q %q", name, description)
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
