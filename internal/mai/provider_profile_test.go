package mai

import (
	"bytes"
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

func TestProviderProfileControlsWireBehaviorAcrossAliasesAndResume(t *testing.T) {
	for _, test := range []struct {
		profile protocolProfile
		names   []string
		effort  string
		opaque  bool
	}{
		{
			profile: profileDeepSeek,
			names:   []string{"deepseek", "deepseek-work", "openrouter"},
			effort:  "max",
		},
		{
			profile: profileOpenRouter,
			names:   []string{"openrouter", "router-work", "deepseek"},
			effort:  "xhigh",
			opaque:  true,
		},
		{
			profile: profileResponses,
			names:   []string{"enclave", "responses-work", "openrouter"},
			effort:  "max",
			opaque:  true,
		},
	} {
		for _, name := range test.names {
			t.Run(string(test.profile)+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				t.Chdir(root)
				t.Setenv("MAI_PROFILE_KEY", "profile-test-key")
				t.Setenv("MAI_DEEPSEEK_URL", "")
				t.Setenv("MAI_CONTEXT_WINDOW", "")

				requests := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests++
					var body struct {
						Model     string            `json:"model"`
						Input     []json.RawMessage `json:"input"`
						Reasoning struct {
							Effort string `json:"effort"`
						} `json:"reasoning"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}

					if body.Model != "Exact/Pro-ID" || body.Reasoning.Effort != test.effort {
						t.Errorf("wire settings = %s/%s, want Exact/Pro-ID/%s", body.Model, body.Reasoning.Effort, test.effort)
					}

					if requests == 1 {
						deepseekTestResponse(w, `[{"type":"reasoning","content":[{"type":"reasoning_text","text":"plain-thought"}],"encrypted_content":"opaque-thought","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PROFILE_OK"}]}]`)
						return
					}

					input := mustJSON(t, body.Input)
					if !bytes.Contains(input, []byte("plain-thought")) || bytes.Contains(input, []byte("opaque-thought")) != test.opaque {
						t.Errorf("resume reasoning does not follow %s profile: %s", test.profile, input)
					}

					deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"PROFILE_OK"}]}]`)
				}))
				defer server.Close()

				config := providerFile{
					DefaultProvider: name,
					Providers: map[string]providerConfig{
						name: {
							BaseURL:   server.URL,
							APIKeyEnv: "MAI_PROFILE_KEY",
							Models:    modelMappings{Pro: "Exact/Pro-ID", Flash: "Exact/Flash-ID"},
							Profile:   test.profile,
						},
					},
				}
				configPath := filepath.Join(root, ".mai.config")
				if err := os.WriteFile(configPath, mustJSON(t, config), 0o600); err != nil {
					t.Fatal(err)
				}

				for i, args := range [][]string{
					{"start", "--max", "--persist"},
					{"continue", "--last"},
				} {
					if i == 1 {
						if err := os.WriteFile(configPath, []byte(`{"invalid":true}`), 0o600); err != nil {
							t.Fatal(err)
						}
					}

					var stdout, stderr bytes.Buffer
					args = append(args, "--no-input", "--skip-skills")
					if code := Main(args, &stdout, &stderr); code != 0 {
						t.Fatalf("%v: exit %d: %s", args, code, stderr.String())
					}

					if !strings.Contains(stdout.String(), "PROFILE_OK") {
						t.Fatalf("missing response: %s", stdout.String())
					}
				}

				if requests != 2 {
					t.Fatalf("requests = %d, want 2", requests)
				}

				paths := projectSessionPaths(root)
				id, err := loadCurrentSessionID(paths)
				if err != nil {
					t.Fatal(err)
				}

				raw, err := os.ReadFile(sessionPath(paths, id))
				if err != nil {
					t.Fatal(err)
				}

				var saved session
				if err := json.Unmarshal(raw, &saved); err != nil {
					t.Fatal(err)
				}

				if saved.Backend == nil || saved.Backend.Profile != test.profile {
					t.Fatalf("profile was not persisted: %+v", saved.Backend)
				}
			})
		}
	}
}

func TestProviderProfileDefaultsPreserveLegacySettings(t *testing.T) {
	for _, test := range []struct {
		name    string
		profile protocolProfile
	}{
		{name: "deepseek", profile: profileDeepSeek},
		{name: "openrouter", profile: profileOpenRouter},
		{name: "custom", profile: profileResponses},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := defaultProviderConfig()
			provider.Profile = ""
			config := providerFile{
				DefaultProvider: test.name,
				Providers:       map[string]providerConfig{test.name: provider},
			}

			decoded, err := decodeProviderConfig(bytes.NewReader(mustJSON(t, config)))
			if err != nil {
				t.Fatal(err)
			}

			if decoded.Providers[test.name].Profile != test.profile {
				t.Fatalf("legacy config profile = %s, want %s", decoded.Providers[test.name].Profile, test.profile)
			}

			sess := deepseekTestSession(t)
			sess.Provider = test.name
			sess.Backend = &sessionBackend{
				Endpoint:  "https://provider.example/responses",
				APIKeyEnv: "MAI_PROFILE_KEY",
				Models:    provider.Models,
			}
			path := filepath.Join(t.TempDir(), "session.json")
			if err := saveJSON(path, sess); err != nil {
				t.Fatal(err)
			}

			loaded, err := loadSession(path)
			if err != nil {
				t.Fatal(err)
			}

			if loaded.Backend.Profile != test.profile {
				t.Fatalf("legacy session profile = %s, want %s", loaded.Backend.Profile, test.profile)
			}
		})
	}
}

func TestProviderSettingsRejectedAtConfigSessionAndBackendBoundaries(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "test-key")
	t.Setenv("MAI_DEEPSEEK_URL", "")
	t.Setenv("MAI_CONTEXT_WINDOW", "")

	for _, test := range []struct {
		name   string
		mutate func(*providerConfig)
		want   string
	}{
		{
			name:   "unsafe URL",
			mutate: func(p *providerConfig) { p.BaseURL = "http://provider.example" },
			want:   "URL must be HTTPS",
		},
		{
			name:   "invalid key variable",
			mutate: func(p *providerConfig) { p.APIKeyEnv = "invalid-key" },
			want:   "api_key_env",
		},
		{
			name:   "blank pro",
			mutate: func(p *providerConfig) { p.Models.Pro = " \t" },
			want:   "model mappings",
		},
		{
			name:   "blank flash",
			mutate: func(p *providerConfig) { p.Models.Flash = " \t" },
			want:   "model mappings",
		},
		{
			name:   "invalid profile",
			mutate: func(p *providerConfig) { p.Profile = "invalid" },
			want:   "invalid profile",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := defaultProviderConfig()
			test.mutate(&provider)
			config := providerFile{
				DefaultProvider: defaultProvider,
				Providers:       map[string]providerConfig{defaultProvider: provider},
			}

			if _, err := decodeProviderConfig(bytes.NewReader(mustJSON(t, config))); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("config error = %v, want %s", err, test.want)
			}

			sess := deepseekTestSession(t)
			sess.Backend = &sessionBackend{
				Endpoint:  provider.BaseURL + "/responses",
				APIKeyEnv: provider.APIKeyEnv,
				Models:    provider.Models,
				Profile:   provider.Profile,
			}
			path := filepath.Join(t.TempDir(), "session.json")
			if err := saveJSON(path, sess); err != nil {
				t.Fatal(err)
			}

			if _, err := loadSession(path); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("session error = %v, want %s", err, test.want)
			}

			sess.Backend = nil
			agent := newAgent(io.Discard, io.Discard, "", time.Second, false)
			defer agent.close()

			if err := agent.configureBackend(sess, provider); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("backend error = %v, want %s", err, test.want)
			}

			if sess.Backend != nil || agent.backend != nil {
				t.Fatal("invalid settings installed a backend")
			}
		})
	}
}
