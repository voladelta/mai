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
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const maxSkillFileBytes = 2 << 20

type skillSummary struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Score       int    `json:"score,omitempty"`
}

type skillSearchResult struct {
	OK       bool           `json:"ok"`
	Skills   []skillSummary `json:"skills"`
	Warnings []string       `json:"warnings,omitempty"`
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

func searchSkills(root, query string) (string, error) {
	root, err := canonicalPath(root)
	if err != nil {
		return "", fmt.Errorf("resolve skills directory: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("list skills: %w", err)
	}
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(query)))
	result := skillSearchResult{OK: true, Skills: []skillSummary{}}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		summary, err := loadSkillSummary(root, entry.Name())
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %v", entry.Name(), err))
			continue
		}
		score, matched := skillMatch(summary, terms)
		if !matched {
			continue
		}
		summary.Score = score
		result.Skills = append(result.Skills, summary)
	}
	sort.Slice(result.Skills, func(i, j int) bool {
		if result.Skills[i].Score != result.Skills[j].Score {
			return result.Skills[i].Score > result.Skills[j].Score
		}
		return result.Skills[i].Name < result.Skills[j].Name
	})
	return marshalToolResult(result), nil
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
	return skillSummary{ID: id, Name: name, Description: description}, nil
}

func skillMatch(skill skillSummary, terms []string) (int, bool) {
	if len(terms) == 0 {
		return 0, true
	}
	name := strings.ToLower(skill.Name)
	description := strings.ToLower(skill.Description)
	score := 0
	matched := false
	for _, term := range terms {
		switch {
		case name == term:
			score += 100
			matched = true
		case strings.HasPrefix(name, term):
			score += 50
			matched = true
		case strings.Contains(name, term):
			score += 25
			matched = true
		case strings.Contains(description, term):
			score += 10
			matched = true
		}
	}
	return score, matched
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
