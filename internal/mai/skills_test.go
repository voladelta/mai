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

func TestSearchSkillsRanksMetadataAndListsWithEmptyQuery(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "layout", "layout-helper", "Build responsive page layouts.")
	writeTestSkill(t, root, "writing", "plain-writing", "Write clear interface text.")
	writeTestSkill(t, root, "review", "layout-review", "Review code structure.")
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("filler-%02d", i)
		writeTestSkill(t, root, id, id, "A catalog entry.")
	}

	raw, err := searchSkills(root, "layout")
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

	raw, err = searchSkills(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Skills) != 15 {
		t.Fatalf("listed %d skills, want 15", len(result.Skills))
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
		{Name: "search_skills", Arguments: `{"query":"demo"}`},
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
	if len(definitions) != 5 {
		t.Fatalf("registered %d tools, want 5", len(definitions))
	}
	for i, name := range []string{"search_skills", "read_skill", "read_skill_file"} {
		if definitions[i]["name"] != name {
			t.Fatalf("tool %d is %v, want %s", i, definitions[i]["name"], name)
		}
	}
	searchParameters := definitions[0]["parameters"].(map[string]any)
	properties := searchParameters["properties"].(map[string]any)
	if _, ok := properties["limit"]; ok {
		t.Fatal("search_skills still exposes partial-list limit state")
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
