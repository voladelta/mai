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
	maxSkillFileBytes        = 2 << 20
	maxSkillDescriptionChars = 1_024
)

var explicitSkillPattern = regexp.MustCompile(`\$([a-z][a-z0-9-]*)`)

type skillSummary struct {
	ID            string
	Name          string
	Description   string
	AllowImplicit bool
	root          string
}

type skillContext struct {
	Instructions string
	Warnings     []string
	rootsByID    map[string]string
}

type skillFileResult struct {
	OK        bool   `json:"ok"`
	Skill     string `json:"skill"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Content   string `json:"content,omitempty"`
	imageURL  string
}

type yamlEntry struct {
	line   int
	indent int
	key    string
	value  string
}

func defaultSkillsRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".agents", "skills"), nil
}

func buildSkillContext(roots []string, userPrompt string) skillContext {
	skills, warnings := loadSkills(roots)
	rootsByID := make(map[string]string, len(skills))
	visible := make([]skillSummary, 0, len(skills))
	for _, skill := range skills {
		rootsByID[skill.ID] = skill.root
		if skill.AllowImplicit {
			visible = append(visible, skill)
		}
	}
	catalog := renderSkillCatalog(visible)

	var explicit strings.Builder
	for _, mention := range explicitSkillMentions(userPrompt) {
		matches := matchingSkills(skills, mention)
		switch len(matches) {
		case 0:
			continue
		case 1:
			file, readErr := readSkill([]string{matches[0].root}, matches[0].ID, "")
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
- If the request clearly matches an available skill description, call read_skill({"path":"<id>"}) and follow the complete SKILL.md before acting.
- Catalog metadata is only for selection. Never use it as a substitute for reading a matching SKILL.md.
- To read a supporting file required by the selected SKILL.md, call read_skill({"path":"<id>","file":"<relative file path>"}) with the same skill id.
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
	return skillContext{
		Instructions: instructions.String(),
		Warnings:     warnings,
		rootsByID:    rootsByID,
	}
}

func loadSkills(roots []string) ([]skillSummary, []string) {
	var skills []skillSummary
	var warnings []string
	seenIDs := make(map[string]bool)
	seenNames := make(map[string]bool)
	for _, root := range roots {
		found, rootWarnings, err := loadSkillsRoot(root)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", root, err))
			continue
		}

		warnings = append(warnings, rootWarnings...)
		for _, skill := range found {
			if seenIDs[skill.ID] || seenNames[skill.Name] {
				continue
			}

			skills = append(skills, skill)
		}
		for _, skill := range found {
			seenIDs[skill.ID] = true
			seenNames[skill.Name] = true
		}
	}

	sort.Slice(skills, func(i, j int) bool {
		if skills[i].Name != skills[j].Name {
			return skills[i].Name < skills[j].Name
		}
		return skills[i].ID < skills[j].ID
	})
	return skills, warnings
}

func loadSkillsRoot(root string) ([]skillSummary, []string, error) {
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
	name, description, disableModelInvocation, err := parseSkillFrontMatter(string(b))
	if err != nil {
		return skillSummary{}, err
	}
	allowImplicit, err := loadImplicitPolicy(dir)
	if err != nil {
		return skillSummary{}, err
	}
	return skillSummary{
		ID:            id,
		Name:          name,
		Description:   description,
		AllowImplicit: allowImplicit && !disableModelInvocation,
		root:          root,
	}, nil
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
		entry, ok, err := parseYAMLEntry(lineNumber+1, line)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		if policyIndent >= 0 && entry.indent <= policyIndent {
			policyIndent = -1
		}
		if entry.indent == 0 && entry.key == "policy" && entry.value == "" {
			policyIndent = entry.indent
			continue
		}
		if policyIndent < 0 || entry.key != "allow_implicit_invocation" {
			continue
		}
		switch strings.ToLower(entry.value) {
		case "true":
			return true, nil
		case "false":
			return false, nil
		default:
			return false, fmt.Errorf("agents/openai.yaml line %d has a non-boolean policy.allow_implicit_invocation", entry.line)
		}
	}
	return true, nil
}

func parseYAMLEntry(lineNumber int, line string) (yamlEntry, bool, error) {
	withoutComment, _, _ := strings.Cut(line, "#")
	if strings.TrimSpace(withoutComment) == "" {
		return yamlEntry{}, false, nil
	}
	leading := withoutComment[:len(withoutComment)-len(strings.TrimLeft(withoutComment, " \t"))]
	if strings.Contains(leading, "\t") {
		return yamlEntry{}, false, fmt.Errorf("agents/openai.yaml line %d uses a tab for indentation", lineNumber)
	}
	key, value, ok := strings.Cut(strings.TrimSpace(withoutComment), ":")
	if !ok {
		return yamlEntry{line: lineNumber, indent: len(leading)}, true, nil
	}
	return yamlEntry{
		line: lineNumber, indent: len(leading),
		key: strings.TrimSpace(key), value: strings.TrimSpace(value),
	}, true, nil
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

func renderSkillCatalog(skills []skillSummary) string {
	lines := make([]string, 0, len(skills))
	for _, skill := range skills {
		lines = append(lines, fmt.Sprintf("- %s: %s (id: %s)", skill.Name, skill.Description, skill.ID))
	}
	return strings.Join(lines, "\n")
}

func readSkill(roots []string, id, file string) (skillFileResult, error) {
	var dir string
	for _, root := range roots {
		var err error
		dir, err = secureSkillDir(root, id)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return skillFileResult{}, err
		}
		break
	}
	if dir == "" {
		return skillFileResult{}, fmt.Errorf("skill %q: %w", id, os.ErrNotExist)
	}
	if file == "" {
		file = "SKILL.md"
	}
	clean := filepath.Clean(file)
	if filepath.IsAbs(file) || clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return skillFileResult{}, errors.New("skill file path must stay inside the selected skill")
	}
	resolved, err := secureSkillPath(dir, clean)
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
		OK: true, Skill: id, Path: filepath.ToSlash(clean), MediaType: mediaType,
	}
	if strings.HasPrefix(strings.ToLower(mediaType), "image/") {
		contentType, _, _ := strings.Cut(mediaType, ";")
		result.imageURL = "data:" + contentType + ";base64," + base64.StdEncoding.EncodeToString(b)
		return result, nil
	}
	content := string(b)
	if !utf8.Valid(b) || strings.IndexByte(content, 0) >= 0 {
		return skillFileResult{}, fmt.Errorf("%s is binary data with unsupported media type %s", clean, mediaType)
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
	if id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
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
	// Reject special files before opening, which could block on a FIFO.
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer file.Close()

	b, err := readBoundedFile(file, maxSkillFileBytes)
	if errors.Is(err, errNotRegularFile) {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if errors.Is(err, errFileTooLarge) {
		return nil, fmt.Errorf("%s exceeds the %d byte skill file limit", path, maxSkillFileBytes)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	return b, nil
}

func parseSkillFrontMatter(content string) (string, string, bool, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", false, errors.New("SKILL.md has no YAML front matter")
	}

	var name, description string
	var disableModelInvocation bool
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "---" {
			name = strings.TrimSpace(name)
			description = strings.TrimSpace(description)
			if name == "" || description == "" {
				return "", "", false, errors.New("SKILL.md front matter requires name and description")
			}
			if len([]rune(description)) > maxSkillDescriptionChars {
				return "", "", false, fmt.Errorf("SKILL.md front matter description exceeds %d characters", maxSkillDescriptionChars)
			}
			return name, description, disableModelInvocation, nil
		}

		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		if key != "name" && key != "description" && key != "disable-model-invocation" {
			continue
		}

		value = strings.TrimSpace(value)
		if key == "disable-model-invocation" {
			value, _, _ = strings.Cut(value, "#")
			switch strings.ToLower(strings.TrimSpace(value)) {
			case "true":
				disableModelInvocation = true
			case "false":
				disableModelInvocation = false
			default:
				return "", "", false, fmt.Errorf("SKILL.md front matter line %d has a non-boolean disable-model-invocation", i+1)
			}
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
			description = strings.Join(parts, " ")
			continue
		}
		if key == "name" {
			name = yamlScalar(value)
		} else {
			description = yamlScalar(value)
		}
	}
	return "", "", false, errors.New("SKILL.md front matter is not closed")
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
