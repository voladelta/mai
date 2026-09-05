package mai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxSubagentNameChars   = 64
	maxSubagentPromptBytes = 64 << 10
	maxAgentFileBytes      = 1 << 20
)

type customAgent struct {
	Name                  string
	Description           string
	DeveloperInstructions string
	Effort                string
}

type subagentResult struct {
	OK           bool   `json:"ok"`
	Name         string `json:"name"`
	Output       string `json:"output"`
	Error        string `json:"error,omitempty"`
	Stderr       string `json:"stderr,omitempty"`
	ExitCode     int    `json:"exit_code"`
	DurationMS   int64  `json:"duration_ms"`
	TimedOut     bool   `json:"timed_out,omitempty"`
	Cancelled    bool   `json:"cancelled,omitempty"`
	Truncated    bool   `json:"truncated"`
	OutputBytes  int64  `json:"output_bytes"`
	StderrBytes  int64  `json:"stderr_bytes"`
	OmittedBytes int64  `json:"omitted_bytes"`
}

func defaultAgentsRoot() (string, error) {
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	return filepath.Join(codexHome, "agents"), nil
}

func loadCustomAgents(root string) (map[string]customAgent, []string, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("list custom agents: %w", err)
	}
	agents := make(map[string]customAgent)
	var warnings []string
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		agent, loadErr := loadCustomAgentFile(filepath.Join(root, entry.Name()))
		if loadErr != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", entry.Name(), loadErr))
			continue
		}
		fileName := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if agent.Name != fileName {
			warnings = append(warnings, fmt.Sprintf("%s: defines name %q; the file name and agent name must match", entry.Name(), agent.Name))
			continue
		}
		if _, duplicate := agents[agent.Name]; duplicate {
			warnings = append(warnings, fmt.Sprintf("%s: duplicate custom agent name %q", entry.Name(), agent.Name))
			continue
		}
		agents[agent.Name] = agent
	}
	sort.Strings(warnings)
	return agents, warnings, nil
}

func loadCustomAgent(root, name string) (customAgent, error) {
	if err := validateSubagentName(name); err != nil {
		return customAgent{}, err
	}
	path := filepath.Join(root, name+".toml")
	agent, err := loadCustomAgentFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return customAgent{}, fmt.Errorf("custom agent %q does not exist at %s", name, path)
	}
	if err != nil {
		return customAgent{}, fmt.Errorf("load custom agent %q: %w", name, err)
	}
	if agent.Name != name {
		return customAgent{}, fmt.Errorf("custom agent file %s defines name %q", path, agent.Name)
	}
	return agent, nil
}

func loadCustomAgentFile(path string) (customAgent, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return customAgent{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return customAgent{}, err
	}
	if !info.Mode().IsRegular() {
		return customAgent{}, errors.New("agent configuration is not a regular file")
	}
	if info.Size() > maxAgentFileBytes {
		return customAgent{}, fmt.Errorf("agent configuration exceeds %d bytes", maxAgentFileBytes)
	}
	content, err := io.ReadAll(io.LimitReader(file, maxAgentFileBytes+1))
	if err != nil {
		return customAgent{}, err
	}
	if len(content) > maxAgentFileBytes {
		return customAgent{}, fmt.Errorf("agent configuration exceeds %d bytes", maxAgentFileBytes)
	}
	return parseCustomAgent(string(content))
}

func parseCustomAgent(content string) (customAgent, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	values := make(map[string]string)
	wanted := map[string]bool{
		"name": true, "description": true, "developer_instructions": true,
		"model": true, "model_reasoning_effort": true,
	}
	for lineIndex := 0; lineIndex < len(lines); lineIndex++ {
		line := strings.TrimSpace(lines[lineIndex])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			break
		}
		key, raw, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || !wanted[key] {
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return customAgent{}, fmt.Errorf("duplicate %s field", key)
		}
		value, lastLine, err := parseTOMLString(lines, lineIndex, strings.TrimSpace(raw))
		if err != nil {
			return customAgent{}, fmt.Errorf("parse %s: %w", key, err)
		}
		values[key] = value
		lineIndex = lastLine
	}
	agent := customAgent{
		Name:                  strings.TrimSpace(values["name"]),
		Description:           strings.TrimSpace(values["description"]),
		DeveloperInstructions: strings.TrimSpace(values["developer_instructions"]),
	}
	if agent.Name == "" || agent.Description == "" || agent.DeveloperInstructions == "" ||
		strings.TrimSpace(values["model_reasoning_effort"]) == "" {
		return customAgent{}, errors.New("name, description, developer_instructions, and model_reasoning_effort are required")
	}
	if err := validateSubagentName(agent.Name); err != nil {
		return customAgent{}, fmt.Errorf("invalid name: %w", err)
	}
	agent.Effort = normalizeEffort(values["model_reasoning_effort"])
	if _, ok := effortIDs[agent.Effort]; !ok {
		return customAgent{}, fmt.Errorf("unsupported model_reasoning_effort %q", values["model_reasoning_effort"])
	}
	return agent, nil
}

func parseTOMLString(lines []string, firstLine int, raw string) (string, int, error) {
	for _, delimiter := range []string{"\"\"\"", "'''"} {
		if !strings.HasPrefix(raw, delimiter) {
			continue
		}
		content := strings.TrimPrefix(raw, delimiter)
		firstContinuation := true
		for lineIndex := firstLine; ; {
			if end := strings.Index(content, delimiter); end >= 0 {
				if err := onlyTOMLComment(content[end+len(delimiter):]); err != nil {
					return "", lineIndex, err
				}
				value := content[:end]
				if delimiter == "\"\"\"" {
					decoded, err := decodeTOMLBasicString(value)
					return decoded, lineIndex, err
				}
				return value, lineIndex, nil
			}
			lineIndex++
			if lineIndex >= len(lines) {
				return "", firstLine, errors.New("unterminated multiline string")
			}
			if content != "" || !firstContinuation {
				content += "\n"
			}
			content += lines[lineIndex]
			firstContinuation = false
		}
	}
	if raw == "" || (raw[0] != '\'' && raw[0] != '"') {
		return "", firstLine, errors.New("value must be a TOML string")
	}
	quote := raw[0]
	end := 1
	for end < len(raw) && (raw[end] != quote || quote == '"' && escapedAt(raw, end)) {
		end++
	}
	if end == len(raw) {
		return "", firstLine, errors.New("unterminated string")
	}
	if err := onlyTOMLComment(raw[end+1:]); err != nil {
		return "", firstLine, err
	}
	if quote == '\'' {
		return raw[1:end], firstLine, nil
	}
	value, err := strconv.Unquote(raw[:end+1])
	return value, firstLine, err
}

func decodeTOMLBasicString(value string) (string, error) {
	var decoded strings.Builder
	for len(value) > 0 {
		if value[0] == '"' {
			decoded.WriteByte('"')
			value = value[1:]
			continue
		}
		r, _, tail, err := strconv.UnquoteChar(value, '"')
		if err != nil {
			return "", err
		}
		decoded.WriteRune(r)
		value = tail
	}
	return decoded.String(), nil
}

func escapedAt(value string, index int) bool {
	count := 0
	for index--; index >= 0 && value[index] == '\\'; index-- {
		count++
	}
	return count%2 == 1
}

func onlyTOMLComment(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "#") {
		return nil
	}
	return errors.New("unexpected content after string")
}

func validateSubagentName(name string) error {
	if name == "" {
		return errors.New("subagent name is empty")
	}
	if len([]rune(name)) > maxSubagentNameChars {
		return fmt.Errorf("subagent name exceeds %d characters", maxSubagentNameChars)
	}
	for _, char := range name {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		return errors.New("subagent name can contain only letters, numbers, underscores, and hyphens")
	}
	return nil
}

func renderSubagentInstructions(agents map[string]customAgent) string {
	if len(agents) == 0 {
		return ""
	}
	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)
	var out strings.Builder
	out.WriteString(`Subagents
- spawn_subagent runs one installed custom agent synchronously and returns its final output.
- Use it when the user, an applicable skill, or repository instructions request that custom agent.
- A child cannot spawn another child.

Available custom agents:`)
	for _, name := range names {
		fmt.Fprintf(&out, "\n- %s: %s", name, agents[name].Description)
	}
	return out.String()
}

func customAgentInstructions(agent *customAgent) string {
	if agent == nil {
		return ""
	}
	return fmt.Sprintf("Custom agent role: %s\nThese role instructions supplement mai's base instructions. They do not expand task authority or replace recovery rules.\n\n%s", agent.Name, agent.DeveloperInstructions)
}

func runSubagentProcess(parent context.Context, executable string, timeout time.Duration, cwd string, name, prompt string) string {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "--subagent", name, "--timeout", timeout.String(), "--", prompt)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
	var stdout, stderr cappedBuffer
	stdout.max = maxToolStreamBytes
	stderr.max = maxToolStreamBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	started := time.Now()
	runErr := cmd.Run()
	result := subagentResult{
		OK: runErr == nil, Name: name, Output: stdout.String(), ExitCode: 0,
		DurationMS: time.Since(started).Milliseconds(), Truncated: stdout.Truncated() || stderr.Truncated(),
		OutputBytes: stdout.TotalBytes(), StderrBytes: stderr.TotalBytes(),
		OmittedBytes: stdout.OmittedBytes() + stderr.OmittedBytes(),
	}
	if runErr != nil {
		result.Stderr = stderr.String()
		result.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
		result.Cancelled = ctx.Err() != nil && !result.TimedOut
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
		} else {
			result.ExitCode = -1
		}
		switch {
		case result.TimedOut:
			result.Error = fmt.Sprintf("child process timed out after %s", timeout)
		case result.Cancelled:
			result.Error = "child process was cancelled"
		default:
			result.Error = runErr.Error()
		}
	}
	b, err := json.Marshal(result)
	if err != nil {
		return toolError("encode spawn_subagent result", err)
	}
	return string(b)
}
