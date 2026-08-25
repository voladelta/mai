package mai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxSkillFileBytes    = 2 << 20
	maxSkillCatalogChars = 8_000
)

var explicitSkillPattern = regexp.MustCompile(`\$([a-z][a-z0-9-]*)`)

type skillSummary struct {
	ID            string
	Name          string
	Description   string
	AllowImplicit bool
}

type skillContext struct {
	Instructions string
	Warnings     []string
}

type skillFileResult struct {
	OK        bool   `json:"ok"`
	Skill     string `json:"skill"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Content   string `json:"content,omitempty"`
	imageURL  string
}

func defaultSkillsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".agents", "skills"), nil
}

func buildSkillContext(root, userPrompt string) (skillContext, error) {
	skills, warnings, err := loadSkills(root)
	if err != nil {
		return skillContext{}, err
	}
	visible := make([]skillSummary, 0, len(skills))
	for _, skill := range skills {
		if skill.AllowImplicit {
			visible = append(visible, skill)
		}
	}
	catalog, truncated, omitted := renderSkillCatalog(visible)
	if truncated {
		warnings = append(warnings, "skill descriptions were shortened to fit the 8000-character catalog limit")
	}
	if omitted > 0 {
		warnings = append(warnings, fmt.Sprintf("%d skills were omitted from the model-visible catalog because it exceeded 8000 characters", omitted))
	}

	var explicit strings.Builder
	for _, mention := range explicitSkillMentions(userPrompt) {
		matches := matchingSkills(skills, mention)
		switch len(matches) {
		case 0:
			continue
		case 1:
			file, readErr := readSkill(root, matches[0].ID)
			if readErr != nil {
				warnings = append(warnings, fmt.Sprintf("explicit skill $%s could not be read: %v", mention, readErr))
				continue
			}
			fmt.Fprintf(&explicit, "\n### Explicit skill: $%s (id: %s)\n%s\n", matches[0].Name, matches[0].ID, file.Content)
		default:
			warnings = append(warnings, fmt.Sprintf("explicit skill $%s is ambiguous", mention))
		}
	}

	var instructions strings.Builder
	instructions.WriteString(`Skills
- Available skills are listed as name, description, and directory id.
- If the request clearly matches an available skill description, you must call read_skill with its id and follow the complete SKILL.md before acting.
- Catalog metadata is only for selection. Never use it as a substitute for reading a matching SKILL.md.
- Use read_skill_file only for supporting files required by the selected SKILL.md.
- A $name mention is explicit. Its complete instructions appear below when it resolves uniquely; do not call read_skill again for that explicit skill.
`)
	if catalog == "" {
		instructions.WriteString("\nAvailable skills: none.\n")
	} else {
		instructions.WriteString("\nAvailable skills:\n")
		instructions.WriteString(catalog)
		instructions.WriteByte('\n')
	}
	instructions.WriteString(explicit.String())
	return skillContext{Instructions: instructions.String(), Warnings: warnings}, nil
}

func loadSkills(root string) ([]skillSummary, []string, error) {
	root, err := canonicalPath(root)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve skills directory: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, fmt.Errorf("list skills: %w", err)
	}
	var skills []skillSummary
	var warnings []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		summary, err := loadSkillSummary(root, entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", entry.Name(), err))
			continue
		}
		skills = append(skills, summary)
	}
	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Name != skills[j].Name {
			return skills[i].Name < skills[j].Name
		}
		return skills[i].ID < skills[j].ID
	})
	return skills, warnings, nil
}

func loadSkillSummary(root, id string) (skillSummary, error) {
	dir, err := secureSkillDir(root, id)
	if err != nil {
		return skillSummary{}, err
	}
	path, err := secureSkillPath(dir, "SKILL.md")
	if err != nil {
		return skillSummary{}, err
	}
	b, err := readBoundedRegularFile(path)
	if err != nil {
		return skillSummary{}, err
	}
	name, description, err := parseSkillFrontMatter(string(b))
	if err != nil {
		return skillSummary{}, err
	}
	allowImplicit, err := loadImplicitPolicy(dir)
	if err != nil {
		return skillSummary{}, err
	}
	return skillSummary{ID: id, Name: name, Description: description, AllowImplicit: allowImplicit}, nil
}

func loadImplicitPolicy(skillDir string) (bool, error) {
	path, err := secureSkillPath(skillDir, filepath.Join("agents", "openai.yaml"))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	b, err := readBoundedRegularFile(path)
	if err != nil {
		return false, err
	}
	return parseImplicitPolicy(string(b))
}

func parseImplicitPolicy(content string) (bool, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	policyIndent := -1
	for lineNumber, line := range lines {
		withoutComment, _, _ := strings.Cut(line, "#")
		if strings.TrimSpace(withoutComment) == "" {
			continue
		}
		leading := withoutComment[:len(withoutComment)-len(strings.TrimLeft(withoutComment, " \t"))]
		if strings.Contains(leading, "\t") {
			return false, fmt.Errorf("agents/openai.yaml line %d uses a tab for indentation", lineNumber+1)
		}
		indent := len(leading)
		trimmed := strings.TrimSpace(withoutComment)
		if policyIndent >= 0 && indent <= policyIndent {
			policyIndent = -1
		}
		key, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if indent == 0 && key == "policy" && value == "" {
			policyIndent = indent
			continue
		}
		if policyIndent < 0 || indent <= policyIndent || key != "allow_implicit_invocation" {
			continue
		}
		switch strings.ToLower(value) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, fmt.Errorf("agents/openai.yaml line %d has a non-boolean policy.allow_implicit_invocation", lineNumber+1)
		}
	}
	return true, nil
}

func explicitSkillMentions(prompt string) []string {
	seen := make(map[string]bool)
	var mentions []string
	for _, match := range explicitSkillPattern.FindAllStringSubmatch(prompt, -1) {
		if !seen[match[1]] {
			seen[match[1]] = true
			mentions = append(mentions, match[1])
		}
	}
	return mentions
}

func matchingSkills(skills []skillSummary, mention string) []skillSummary {
	var byName []skillSummary
	for _, skill := range skills {
		if skill.Name == mention {
			byName = append(byName, skill)
		}
	}
	if len(byName) > 0 {
		return byName
	}
	for _, skill := range skills {
		if skill.ID == mention {
			return []skillSummary{skill}
		}
	}
	return nil
}

func renderSkillCatalog(skills []skillSummary) (catalog string, truncated bool, omitted int) {
	if len(skills) == 0 {
		return "", false, 0
	}
	if full := renderSkillLines(skills, -1); len([]rune(full)) <= maxSkillCatalogChars {
		return full, false, 0
	}

	low, high := 0, 0
	for _, skill := range skills {
		high = max(high, len([]rune(skill.Description)))
	}
	if minimum := renderSkillLines(skills, 0); len([]rune(minimum)) <= maxSkillCatalogChars {
		for low < high {
			mid := low + (high-low+1)/2
			if len([]rune(renderSkillLines(skills, mid))) <= maxSkillCatalogChars {
				low = mid
			} else {
				high = mid - 1
			}
		}
		return renderSkillLines(skills, low), true, 0
	}

	var lines []string
	used := 0
	for _, skill := range skills {
		line := renderSkillLine(skill, 0)
		cost := len([]rune(line))
		if len(lines) > 0 {
			cost++
		}
		if used+cost > maxSkillCatalogChars {
			break
		}
		lines = append(lines, line)
		used += cost
	}
	return strings.Join(lines, "\n"), true, len(skills) - len(lines)
}

func renderSkillLines(skills []skillSummary, descriptionLimit int) string {
	lines := make([]string, 0, len(skills))
	for _, skill := range skills {
		lines = append(lines, renderSkillLine(skill, descriptionLimit))
	}
	return strings.Join(lines, "\n")
}

func renderSkillLine(skill skillSummary, descriptionLimit int) string {
	if descriptionLimit == 0 {
		return fmt.Sprintf("- %s (id: %s)", skill.Name, skill.ID)
	}
	description := skill.Description
	if descriptionLimit > 0 {
		runes := []rune(description)
		if len(runes) > descriptionLimit {
			description = string(runes[:descriptionLimit]) + "…"
		}
	}
	return fmt.Sprintf("- %s: %s (id: %s)", skill.Name, description, skill.ID)
}

func readSkill(root, id string) (skillFileResult, error) {
	dir, err := secureSkillDir(root, id)
	if err != nil {
		return skillFileResult{}, err
	}
	return readSkillPath(id, dir, "SKILL.md")
}

func readSkillFile(root, id, path string) (skillFileResult, error) {
	dir, err := secureSkillDir(root, id)
	if err != nil {
		return skillFileResult{}, err
	}
	clean := filepath.Clean(path)
	if filepath.IsAbs(path) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return skillFileResult{}, errors.New("skill file path must stay inside the selected skill")
	}
	return readSkillPath(id, dir, clean)
}

func readSkillPath(id, dir, path string) (skillFileResult, error) {
	resolved, err := secureSkillPath(dir, path)
	if err != nil {
		return skillFileResult{}, err
	}
	b, err := readBoundedRegularFile(resolved)
	if err != nil {
		return skillFileResult{}, err
	}
	mediaType := mime.TypeByExtension(filepath.Ext(resolved))
	if mediaType == "" {
		mediaType = http.DetectContentType(b)
	}
	result := skillFileResult{
		OK: true, Skill: id, Path: filepath.ToSlash(path), MediaType: mediaType,
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		contentType, _, _ := strings.Cut(mediaType, ";")
		result.imageURL = "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(b)
		return result, nil
	}
	content := string(b)
	if !utf8.Valid(b) || strings.IndexByte(content, 0) >= 0 {
		return skillFileResult{}, fmt.Errorf("%s is binary data with unsupported media type %s", path, mediaType)
	}
	result.Content = content
	return result, nil
}

func secureSkillPath(dir, path string) (string, error) {
	resolved, err := canonicalPath(filepath.Join(dir, path))
	if err != nil {
		return "", fmt.Errorf("resolve skill file: %w", err)
	}
	if !pathWithin(dir, resolved) {
		return "", errors.New("skill file resolves outside the selected skill")
	}
	return resolved, nil
}

func secureSkillDir(root, id string) (string, error) {
	if id == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return "", errors.New("skill id must be one directory name")
	}
	root, err := canonicalPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve skills directory: %w", err)
	}
	dir, err := canonicalPath(filepath.Join(root, id))
	if err != nil {
		return "", fmt.Errorf("resolve skill %q: %w", id, err)
	}
	if !pathWithin(root, dir) {
		return "", fmt.Errorf("skill %q resolves outside the skills directory", id)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("inspect skill %q: %w", id, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("skill %q is not a directory", id)
	}
	return dir, nil
}

func readBoundedRegularFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() > maxSkillFileBytes {
		return nil, fmt.Errorf("%s exceeds the %d byte skill file limit", path, maxSkillFileBytes)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return b, nil
}

func parseSkillFrontMatter(content string) (string, string, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", errors.New("SKILL.md has no YAML front matter")
	}
	values := make(map[string]string)
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			name := strings.TrimSpace(values["name"])
			description := strings.TrimSpace(values["description"])
			if name == "" || description == "" {
				return "", "", errors.New("SKILL.md front matter requires name and description")
			}
			return name, description, nil
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key != "name" && key != "description" {
			continue
		}
		if key == "description" && (strings.HasPrefix(value, ">") || strings.HasPrefix(value, "|")) {
			var parts []string
			for i+1 < len(lines) && (strings.HasPrefix(lines[i+1], " ") || strings.TrimSpace(lines[i+1]) == "") {
				i++
				if part := strings.TrimSpace(lines[i]); part != "" {
					parts = append(parts, part)
				}
			}
			values[key] = strings.Join(parts, " ")
			continue
		}
		values[key] = yamlScalar(value)
	}
	return "", "", errors.New("SKILL.md front matter is not closed")
}

func yamlScalar(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if unquoted, err := strconv.Unquote(value); err == nil {
			return unquoted
		}
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return value
}

func marshalToolResult(value any) string {
	b, _ := json.Marshal(value)
	return string(b)
}
