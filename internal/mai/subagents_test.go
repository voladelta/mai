package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadCustomAgentParsesCodexAgentFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentConfig(t, root, "repo_scout", `name = "repo_scout"
description = "Map the repository."
model = "gpt-5.6-terra"
model_reasoning_effort = "medium"
sandbox_mode = "read-only"

developer_instructions = """
Map the minimum context.
Keep quoted "symbols" exact.
"""

[[skills.config]]
path = "/tmp/example/SKILL.md"
enabled = true
`)

	agent, err := loadCustomAgent(root, "repo_scout")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Name != "repo_scout" || agent.Description != "Map the repository." ||
		agent.Effort != "m" {
		t.Fatalf("custom agent = %#v", agent)
	}
	if agent.DeveloperInstructions != "Map the minimum context.\nKeep quoted \"symbols\" exact." {
		t.Fatalf("developer instructions = %q", agent.DeveloperInstructions)
	}
}

func TestLoadCustomAgentsOmitsInvalidAndMismatchedFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentConfig(t, root, "repo_scout", validAgentConfig("repo_scout"))
	writeAgentConfig(t, root, "wrong_file", validAgentConfig("implementor"))
	writeAgentConfig(t, root, "invalid", `name = "invalid"`)

	agents, warnings, err := loadCustomAgents(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents["repo_scout"].Name != "repo_scout" {
		t.Fatalf("loaded agents = %#v", agents)
	}
	if len(warnings) != 2 || !strings.Contains(strings.Join(warnings, "\n"), "must match") {
		t.Fatalf("warnings = %#v", warnings)
	}
	catalog := renderSubagentInstructions(agents)
	if !strings.Contains(catalog, "repo_scout: Test agent repo_scout.") ||
		strings.Contains(catalog, "Instructions for repo_scout") {
		t.Fatalf("catalog contains the wrong data:\n%s", catalog)
	}
}

func TestNewAgentEnablesCatalogWhenCustomAgentExists(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	agentsRoot := filepath.Join(codexHome, "agents")
	if err := os.MkdirAll(agentsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentConfig(t, agentsRoot, "repo_scout", validAgentConfig("repo_scout"))

	a := newAgent(&bytes.Buffer{}, &bytes.Buffer{}, "", time.Second, false, nil)
	if !a.client.allowSubagents {
		t.Fatal("custom agent did not enable spawn_subagent")
	}
	catalog := a.loadSubagentInstructions()
	if !strings.Contains(catalog, "repo_scout: Test agent repo_scout.") ||
		strings.Contains(catalog, "Instructions for repo_scout") {
		t.Fatalf("custom agent catalog = %s", catalog)
	}
}

func TestLoadCustomAgentRejectsUnsafeNameAndNameMismatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "agents")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentConfig(t, root, "repo_scout", validAgentConfig("implementor"))
	if _, err := loadCustomAgent(root, "../repo_scout"); err == nil {
		t.Fatal("unsafe agent name was accepted")
	}
	if _, err := loadCustomAgent(root, "repo_scout"); err == nil || !strings.Contains(err.Error(), "defines name") {
		t.Fatalf("name mismatch error = %v", err)
	}
}

func TestRunSubagentProcessUsesCLIContractAndWorkingDirectory(t *testing.T) {
	root := t.TempDir()
	executable := filepath.Join(t.TempDir(), "fake-mai")
	script := `#!/bin/sh
printf 'cwd=<%s>\n' "$PWD"
printf 'args='
for arg in "$@"; do printf '<%s>' "$arg"; done
printf '\n'
printf 'diagnostic\n' >&2
`
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	result := decodeSubagentResult(t, runSubagentProcess(context.Background(), executable, time.Second, root, "repo_scout", "map the parser"))
	if !result.OK || result.ExitCode != 0 || result.Name != "repo_scout" {
		t.Fatalf("subagent result = %#v", result)
	}
	if !strings.Contains(result.Output, "cwd=<"+root+">") ||
		!strings.Contains(result.Output, "args=<--subagent><repo_scout><--timeout><1s><--><map the parser>") {
		t.Fatalf("child output = %q", result.Output)
	}
	if result.Stderr != "" || result.StderrBytes == 0 {
		t.Fatalf("successful child exposed stderr: %#v", result)
	}
}

func TestRunSubagentProcessTimesOutWholeChild(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "slow-mai")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result := decodeSubagentResult(t, runSubagentProcess(context.Background(), executable, 50*time.Millisecond, t.TempDir(), "repo_scout", "wait"))
	if result.OK || !result.TimedOut || !strings.Contains(result.Error, "timed out") {
		t.Fatalf("timeout result = %#v", result)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("child timeout took %s", elapsed)
	}
}

func TestSpawnSubagentToolRegistrationAndNestedRejection(t *testing.T) {
	if hasTool(toolDefinitions(), "spawn_subagent") {
		t.Fatal("spawn_subagent was registered without available custom agents")
	}
	if !hasTool(toolDefinitions(true), "spawn_subagent") {
		t.Fatal("spawn_subagent was not registered when enabled")
	}

	a := &agent{customAgent: &customAgent{Name: "repo_scout"}}
	raw := a.executeSpawnSubagent(context.Background(), &session{}, `{"name":"repo_scout","prompt":"inspect"}`)
	var output string
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "cannot spawn another subagent") {
		t.Fatalf("nested spawn output = %s", output)
	}
}

func TestSpawnSubagentToolRunsConfiguredChild(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "fake-mai")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'child result'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	a := &agent{
		customAgents: map[string]customAgent{"repo_scout": {Name: "repo_scout"}},
		executable:   executable,
		timeout:      time.Second,
		stderr:       &stderr,
	}
	raw := a.executeTool(context.Background(), &session{CWD: t.TempDir()}, functionCall{
		Name: "spawn_subagent", Arguments: `{"name":"repo_scout","prompt":"inspect"}`,
	})
	var output string
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	result := decodeSubagentResult(t, output)
	if !result.OK || result.Output != "child result" {
		t.Fatalf("tool result = %#v", result)
	}
	if !strings.Contains(stderr.String(), "subagent: repo_scout") || !strings.Contains(stderr.String(), "completed") {
		t.Fatalf("progress output = %q", stderr.String())
	}
}

func TestDirectSubagentUsesConfiguredRoleAndDisablesSpawn(t *testing.T) {
	writeTestCodexAuth(t)
	agentsRoot := filepath.Join(os.Getenv("CODEX_HOME"), "agents")
	if err := os.MkdirAll(agentsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAgentConfig(t, agentsRoot, "repo_scout", validAgentConfig("repo_scout"))

	requestBody := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		requestBody <- body
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"mapped"}`)
		fmt.Fprintln(w)
		writeSSEItem(t, w, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"mapped"}]}`, 100)
	}))
	defer server.Close()
	t.Setenv("MAI_CODEX_URL", server.URL)
	root := t.TempDir()
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"--subagent", "repo_scout", "map the parser"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	body := <-requestBody
	if body["model"] != "gpt-6-astra" {
		t.Fatalf("request model = %#v", body["model"])
	}
	reasoning, _ := body["reasoning"].(map[string]any)
	if reasoning["effort"] != "medium" {
		t.Fatalf("request reasoning = %#v", reasoning)
	}
	instructions, _ := body["instructions"].(string)
	if !strings.Contains(instructions, "Custom agent role: repo_scout") ||
		!strings.Contains(instructions, "Instructions for repo_scout") ||
		strings.Contains(instructions, "Available custom agents:") ||
		strings.Contains(instructions, "Available skills:") {
		t.Fatalf("request instructions = %s", instructions)
	}
	tools, _ := body["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["name"] == "spawn_subagent" {
			t.Fatal("direct subagent received spawn_subagent")
		}
	}
	if stdout.String() != "mapped\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(root, ".mai")); !os.IsNotExist(err) {
		t.Fatalf("direct subagent created .mai: %v", err)
	}
}

func validAgentConfig(name string) string {
	return `name = "` + name + `"
description = "Test agent ` + name + `."
model = "gpt-5.6-terra"
model_reasoning_effort = "medium"
developer_instructions = "Instructions for ` + name + `."
`
}

func writeAgentConfig(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name+".toml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func decodeSubagentResult(t *testing.T, raw string) subagentResult {
	t.Helper()
	var result subagentResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatalf("decode subagent result: %v\n%s", err, raw)
	}
	return result
}

func hasTool(definitions []map[string]any, name string) bool {
	for _, definition := range definitions {
		if definition["name"] == name {
			return true
		}
	}
	return false
}

func TestCustomAgentDoesNotRequireModel(t *testing.T) {
	root := t.TempDir()
	config := strings.ReplaceAll(validAgentConfig("repo_scout"), "model = \"gpt-5.6-terra\"\n", "")
	writeAgentConfig(t, root, "repo_scout", config)
	if _, err := loadCustomAgent(root, "repo_scout"); err != nil {
		t.Fatal(err)
	}
}
