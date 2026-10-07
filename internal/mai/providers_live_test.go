package mai

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLiveProviderConformance(t *testing.T) {
	provider := os.Getenv("MAI_LIVE_PROVIDER")
	if provider == "" {
		t.Skip("set MAI_LIVE_PROVIDER=openrouter or enclave")
	}
	if provider != "openrouter" && provider != "enclave" {
		t.Fatal("MAI_LIVE_PROVIDER must be openrouter or enclave")
	}
	config, err := os.ReadFile(filepath.Join("..", "..", ".mai.config.example"))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeProviderConfig(strings.NewReader(string(config)))
	if err != nil {
		t.Fatal(err)
	}
	wantModel := decoded.Providers[provider].Model
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".mai.config", config, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAI_CONTEXT_WINDOW", "")

	const marker = "MAI_PROVIDER_FLASH_OK"
	prompt := "Call bash exactly once with command `printf " + marker + "`, then reply exactly " + marker + ". Do not edit files, inspect environment variables, or call other tools."
	var stdout, stderr bytes.Buffer
	if code := Main([]string{prompt, "--provider", provider, "--jsonl", "--no-input", "--skip-skills", "--max-turns", "4", "--timeout", "90s"}, &stdout, &stderr); code != 0 {
		t.Fatalf("%s Flash exited %d: %s", provider, code, stderr.String())
	}

	var started, tool, completed bool
	var answer strings.Builder
	requests := 0
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		var event struct {
			Type     string `json:"type"`
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Effort   string `json:"effort"`
			Text     string `json:"text"`
			Name     string `json:"name"`
			Output   string `json:"output"`
		}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case "task.started":
			started = event.Provider == provider && event.Model == wantModel && event.Effort == "h"
		case "tool.completed":
			tool = tool || event.Name == "bash" && strings.Contains(event.Output, marker)
		case "model.delta":
			answer.WriteString(event.Text)
		case "model.completed":
			requests++
		case "task.completed":
			completed = true
		}
	}
	if !started || !tool || !strings.Contains(answer.String(), marker) || !completed || requests < 2 {
		t.Fatalf("%s Flash conformance: started=%v tool=%v answer=%q completed=%v requests=%d", provider, started, tool, answer.String(), completed, requests)
	}
	t.Logf("PROVIDER_CONFORMANCE provider=%s model=%s effort=high tool_replay=true requests=%d", provider, wantModel, requests)
}
