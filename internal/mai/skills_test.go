package mai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchSkillsRanksMetadataAndListsWithEmptyQuery(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "layout", "layout-helper", "Build responsive page layouts.")
	writeTestSkill(t, root, "writing", "plain-writing", "Write clear interface text.")
	writeTestSkill(t, root, "review", "layout-review", "Review code structure.")

	raw, err := searchSkills(root, "layout", 2)
	if err != nil {
		t.Fatal(err)
	}
	var result skillSearchResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) != 2 || result.Skills[0].ID != "layout" || result.Skills[1].ID != "review" {
		t.Fatalf("unexpected ranked skills: %#v", result.Skills)
	}

	raw, err = searchSkills(root, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) != 3 {
		t.Fatalf("listed %d skills, want 3", len(result.Skills))
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

	raw, err := readSkill(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	var result skillFileResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Path != "SKILL.md" || !strings.Contains(result.Content, "# demo instructions") {
		t.Fatalf("unexpected skill result: %#v", result)
	}

	raw, err = readSkillFile(root, "demo", "references/guide.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Encoding != "utf-8" || result.Content != "# Guide\nRead all of this.\n" {
		t.Fatalf("unexpected reference result: %#v", result)
	}
	raw, err = readSkillFile(root, "demo", "root-note.md")
	if err != nil {
		t.Fatal(err)
	}

	raw, err = readSkillFile(root, "demo", "assets/icon.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if result.Encoding != "base64" || result.Content != base64.StdEncoding.EncodeToString(binary) {
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
}

func TestAgentRoutesRegisteredSkillTools(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	mustWrite(t, filepath.Join(root, "demo", "references", "guide.md"), "Guide.\n")
	a := &agent{stderr: io.Discard, skillsRoot: root}

	calls := []functionCall{
		{Name: "search_skills", Arguments: `{"query":"demo","limit":1}`},
		{Name: "read_skill", Arguments: `{"skill":"demo"}`},
		{Name: "read_skill_file", Arguments: `{"skill":"demo","path":"references/guide.md"}`},
	}
	for _, call := range calls {
		result := a.executeTool(context.Background(), &session{}, call)
		if !strings.Contains(result, `"ok":true`) {
			t.Fatalf("%s failed: %s", call.Name, result)
		}
	}

	definitions := toolDefinitions()
	if len(definitions) != 5 {
		t.Fatalf("registered %d tools, want 5", len(definitions))
	}
	for i, name := range []string{"search_skills", "read_skill", "read_skill_file"} {
		if definitions[i]["name"] != name {
			t.Fatalf("tool %d is %v, want %s", i, definitions[i]["name"], name)
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
