package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProviderConfigDiscovery(t *testing.T) {
	cwd, userHome := t.TempDir(), t.TempDir()
	cfg, err := loadProviderConfig(cwd, userHome)
	if err != nil || cfg.DefaultProvider != "deepseek" || cfg.Providers["deepseek"].Models.Pro != "deepseek-v4-pro" {
		t.Fatalf("built-in config = %+v, %v", cfg, err)
	}

	homeConfig := providerFile{
		DefaultProvider: "home-provider",
		Providers:       map[string]providerConfig{"home-provider": defaultProviderConfig()},
	}
	if err := os.WriteFile(filepath.Join(userHome, ".mai.config"), mustJSON(t, homeConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadProviderConfig(cwd, userHome)
	if err != nil || cfg.DefaultProvider != "home-provider" {
		t.Fatalf("home config = %+v, %v", cfg, err)
	}

	localPath := filepath.Join(cwd, ".mai.config")
	localConfig := providerFile{
		DefaultProvider: "local-provider",
		Providers:       map[string]providerConfig{"local-provider": defaultProviderConfig()},
	}
	if err := os.WriteFile(localPath, mustJSON(t, localConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err = loadProviderConfig(cwd, userHome)
	if err != nil || cfg.DefaultProvider != "local-provider" || len(cfg.Providers) != 1 {
		t.Fatalf("local config merged or lost priority: %+v, %v", cfg, err)
	}

	if err := os.WriteFile(localPath, []byte(`{"providers":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProviderConfig(cwd, userHome); err == nil || !strings.Contains(err.Error(), localPath) {
		t.Fatalf("invalid local config fell back to home: %v", err)
	}
}

func TestProviderConfigRejectsInvalidAndUnsafeSettings(t *testing.T) {
	valid := `{"providers":{"deepseek":{"base_url":"https://api.deepseek.com","api_key_env":"DEEPSEEK_API_KEY","models":{"flash":"DeepSeek/Flash","pro":"deepseek-v4-pro"}}}}`
	cfg, err := decodeProviderConfig(strings.NewReader(valid))
	if err != nil || cfg.DefaultProvider != defaultProvider || cfg.Providers[defaultProvider].Models.Flash != "DeepSeek/Flash" {
		t.Fatalf("defaults or model ID casing changed: %+v, %v", cfg, err)
	}

	for _, raw := range []string{
		`null`,
		`{}`,
		valid + `{}`,
		strings.Replace(valid, `"providers":`, `"default_model":"pro","providers":`, 1),
		strings.Replace(valid, `"providers":`, `"default_provider":"missing","providers":`, 1),
		strings.Replace(valid, `"deepseek":`, `"../deepseek":`, 1),
		strings.Replace(valid, "https://api.deepseek.com", "http://provider.example/v1", 1),
		strings.Replace(valid, "https://api.deepseek.com", "https://user:secret@provider.example/v1", 1),
		strings.Replace(valid, "https://api.deepseek.com", "https://provider.example/v1?key=secret", 1),
		strings.Replace(valid, "https://api.deepseek.com", "https://provider.example/v1#fragment", 1),
		strings.Replace(valid, "DEEPSEEK_API_KEY", "sk-raw-secret", 1),
		strings.Replace(valid, `"pro":"deepseek-v4-pro"`, `"pro":""`, 1),
		strings.Replace(valid, `"flash":"DeepSeek/Flash"`, `"ds-flash":"DeepSeek/Flash"`, 1),
	} {
		if _, err := decodeProviderConfig(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted invalid configuration: %s", raw)
		}
	}

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".mai.config"), []byte(strings.Repeat(" ", (1<<20)+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadProviderConfig(cwd, t.TempDir()); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized configuration accepted: %v", err)
	}
}

func TestProviderCLISelectionToolReplayAndResume(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("MAI_DEEPSEEK_URL", "https://wrong-endpoint.invalid/responses")
	t.Setenv("MAI_CONTEXT_WINDOW", "")
	t.Setenv("MAI_TEST_ROUTER_KEY", "router-test-secret")
	t.Setenv("MAI_TEST_ENCLAVE_KEY", "enclave-test-secret")
	var requests []struct {
		Model string
		Key   string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("endpoint = %s", r.URL.Path)
		}
		var body struct {
			Model string            `json:"model"`
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, struct{ Model, Key string }{body.Model, r.Header.Get("Authorization")})
		if len(requests) == 1 {
			deepseekTestResponse(w, `[{"type":"reasoning","id":"router-reasoning","encrypted_content":"opaque-router-state","summary":[]},{"type":"function_call","call_id":"provider-probe","name":"bash","arguments":"{\"command\":\"printf PROVIDER_TOOL_OK\"}"}]`)
			return
		}
		input := mustJSON(t, body.Input)
		if !bytes.Contains(input, []byte("PROVIDER_TOOL_OK")) {
			t.Error("tool output lost across turn/resume/provider switch")
		}
		if len(requests) <= 3 && !bytes.Contains(input, []byte("opaque-router-state")) {
			t.Error("router reasoning lost on same-provider resume")
		}
		if len(requests) > 3 && bytes.Contains(input, []byte("opaque-router-state")) {
			t.Error("provider switch replayed another provider's reasoning")
		}
		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PROVIDER_OK"}]}]`)
	}))
	defer server.Close()

	cfg := providerFile{
		DefaultProvider: "openrouter",
		Providers: map[string]providerConfig{
			"openrouter": {
				BaseURL:   server.URL + "/v1/",
				APIKeyEnv: "MAI_TEST_ROUTER_KEY",
				Models:    modelMappings{Flash: "deepseek/Flash", Pro: "deepseek/Pro"},
				Profile:   profileOpenRouter,
			},
			"enclave": {
				BaseURL:   server.URL + "/v1",
				APIKeyEnv: "MAI_TEST_ENCLAVE_KEY",
				Models:    modelMappings{Flash: "cyberouter/Flash", Pro: "cyberouter/Pro"},
				Profile:   profileResponses,
			},
		},
	}
	if err := os.WriteFile(filepath.Join(root, ".mai.config"), mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	for index, args := range [][]string{
		{"probe", "--f", "--persist"},
		{"continue", "--last"},
		{"continue", "--last", "--provider", "enclave"},
		{"continue", "--last"},
	} {
		if index == 1 || index == 3 {
			// Resume must ignore changed configuration and use the current key.
			if err := os.WriteFile(filepath.Join(root, ".mai.config"), []byte(`{"invalid":true}`), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("MAI_TEST_ROUTER_KEY", "router-rotated-secret")
		}
		if index == 2 {
			if err := os.WriteFile(filepath.Join(root, ".mai.config"), mustJSON(t, cfg), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		var stdout, stderr bytes.Buffer
		args = append(args, "--jsonl", "--no-input", "--skip-skills")
		if code := Main(args, &stdout, &stderr); code != 0 {
			t.Fatalf("%v: exit %d, %s", args, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), `"provider":`) || !strings.Contains(stdout.String(), "PROVIDER_OK") {
			t.Fatalf("missing provider or answer in events: %s", stdout.String())
		}
		if strings.Contains(stdout.String()+stderr.String(), "secret") {
			t.Fatal("credential leaked in CLI output")
		}
	}
	if len(requests) != 5 {
		t.Fatalf("requests = %d", len(requests))
	}
	for index, want := range []string{"deepseek/Flash", "deepseek/Flash", "deepseek/Flash", "cyberouter/Flash", "cyberouter/Flash"} {
		if requests[index].Model != want {
			t.Fatalf("request %d model = %s, want %s", index, requests[index].Model, want)
		}
		wantKey := "Bearer router-test-secret"
		if index == 2 {
			wantKey = "Bearer router-rotated-secret"
		}
		if index >= 3 {
			wantKey = "Bearer enclave-test-secret"
		}
		if requests[index].Key != wantKey {
			t.Fatalf("request %d used the wrong provider credential", index)
		}
	}
	paths := projectSessionPaths(root)
	id, err := loadCurrentSessionID(paths)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := loadSession(sessionPath(paths, id))
	if err != nil {
		t.Fatal(err)
	}

	if saved.Provider != "enclave" || saved.Model != "flash" || saved.Effort != "h" || saved.ReasoningStart == 0 {
		t.Fatalf("provider switch lost saved mode/settings: %+v, %v", saved, err)
	}

	if saved.Backend == nil || saved.Backend.Profile != profileResponses {
		t.Fatalf("provider switch lost saved profile: %+v", saved.Backend)
	}

	if !bytes.Contains(mustJSON(t, saved.History), []byte("opaque-router-state")) {
		t.Fatal("provider switch destroyed original reasoning history")
	}
	if bytes.Contains(mustJSON(t, saved), []byte("secret")) {
		t.Fatal("provider credential persisted")
	}
	if err := os.WriteFile(filepath.Join(root, ".mai.config"), mustJSON(t, cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, provider := range []string{"missing", "enclave"} {
		if provider == "enclave" {
			t.Setenv("MAI_TEST_ENCLAVE_KEY", "")
		}
		var stdout, stderr bytes.Buffer
		if code := Main([]string{"probe", "--provider", provider, "--no-input"}, &stdout, &stderr); code != 1 {
			t.Fatalf("invalid provider/credential accepted: %s", provider)
		}
	}
	if len(requests) != 5 {
		t.Fatal("invalid provider/credential made a request")
	}
}

func TestProviderSidekickAndImageUseFlashMapping(t *testing.T) {
	t.Setenv("MAI_TEST_PROVIDER_KEY", "test-secret")
	t.Setenv("MAI_CONTEXT_WINDOW", "")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Model != "cyberouter/mapped-flash" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("Flash request escaped the selected provider")
		}
		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"FLASH_OK"}]}]`)
	}))
	defer server.Close()

	sess := deepseekTestSession(t)
	sess.Provider, sess.Model = "enclave", "pro"
	a := newAgent(io.Discard, io.Discard, "", time.Second, false)
	defer a.close()
	a.skipSkills = true
	if err := a.configureBackend(sess, providerConfig{
		BaseURL: server.URL, APIKeyEnv: "MAI_TEST_PROVIDER_KEY",
		Models: modelMappings{Flash: "cyberouter/mapped-flash", Pro: "cyberouter/mapped-pro"},
	}); err != nil {
		t.Fatal(err)
	}
	worker, err := a.newSidekick(sess)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.agent.close()
	if worker.session.Provider != "enclave" {
		t.Fatal("sidekick lost provider identity")
	}
	if err := worker.agent.run(context.Background(), worker.session, "answer"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.backend.(*responsesClient).describeImage(context.Background(), sess, "data:image/png;base64,iVBORw0KGgo="); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("Flash requests = %d", requests)
	}
}

func TestLegacySessionAliasesLoad(t *testing.T) {
	for _, legacy := range []string{"ds-flash", "ds-pro"} {
		sess := deepseekTestSession(t)
		sess.Model = legacy
		path := filepath.Join(t.TempDir(), "session.json")
		if err := saveJSON(path, sess); err != nil {
			t.Fatal(err)
		}
		loaded, err := loadSession(path)
		if err != nil || loaded.Provider != defaultProvider || loaded.Model != strings.TrimPrefix(legacy, "ds-") {
			t.Fatalf("legacy task did not load: %+v, %v", loaded, err)
		}
	}
}

func TestProviderSwitchKeepsContextEditIndexesAndResetsBoundaryAfterCompaction(t *testing.T) {
	sess := contextEditFixture(t)
	original := mustJSON(t, sess.History)
	sess.ContextEdits = []contextEdit{{Index: 3, Digest: contextDigest(sess.History[3]), Summary: "Preserve ORIGINAL-RECALL-FACT; build completed."}}
	sess.ReasoningStart = len(sess.History)
	sess.History = append(sess.History, json.RawMessage(`{"type":"reasoning","content":[{"type":"reasoning_text","text":"new provider reasoning"}]}`))

	history, err := sess.requestHistory()
	if err != nil {
		t.Fatal(err)
	}
	encoded := mustJSON(t, history)
	if bytes.Contains(encoded, []byte("private reasoning")) || !bytes.Contains(encoded, []byte("new provider reasoning")) || !bytes.Contains(encoded, []byte("stdout_context_summary")) {
		t.Fatalf("reasoning boundary or context projection failed: %s", encoded)
	}
	if !bytes.Equal(original, mustJSON(t, sess.History[:4])) || sess.ContextEdits[0].Index != 3 {
		t.Fatal("provider switch mutated original history or context edit indexes")
	}
	if err := validateResponsesHistory(history, sess.Model, profileDeepSeek); err != nil {
		t.Fatal(err)
	}

	if err := appendUserPrompt(sess, "Continue with the new provider."); err != nil {
		t.Fatal(err)
	}
	sess.ContextTokens = modelContextWindow
	a := newAgent(io.Discard, io.Discard, "", time.Second, false)
	a.backend = &checkpointStub{reply: "Preserve ORIGINAL-RECALL-FACT; continue the task."}
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	if sess.ReasoningStart != 0 || len(sess.ContextEdits) != 0 {
		t.Fatal("compaction left a stale provider reasoning boundary or edit indexes")
	}
	if err := validateSessionHeader(sess); err != nil {
		t.Fatal(err)
	}
}
