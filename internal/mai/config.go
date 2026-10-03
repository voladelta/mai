package mai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const defaultProvider = "deepseek"

type providerFile struct {
	DefaultProvider string                    `json:"default_provider"`
	Providers       map[string]providerConfig `json:"providers"`
}

type providerConfig struct {
	BaseURL   string          `json:"base_url"`
	APIKeyEnv string          `json:"api_key_env"`
	Models    modelMappings   `json:"models"`
	Profile   protocolProfile `json:"profile,omitempty"`
}

type protocolProfile string

const (
	profileDeepSeek   protocolProfile = "deepseek"
	profileOpenRouter protocolProfile = "openrouter"
	profileResponses  protocolProfile = "responses"
)

func resolveProtocolProfile(provider string, profile protocolProfile) (protocolProfile, error) {
	// Omitted profiles preserve the behavior of existing configs and snapshots.
	if profile == "" {
		switch provider {
		case "", defaultProvider:
			profile = profileDeepSeek
		case "openrouter":
			profile = profileOpenRouter
		default:
			profile = profileResponses
		}
	}

	switch profile {
	case profileDeepSeek, profileOpenRouter, profileResponses:
		return profile, nil
	default:
		return "", fmt.Errorf("invalid profile %q (use deepseek, openrouter, or responses)", profile)
	}
}

func validateProviderSettings(endpoint, apiKeyEnv string, models modelMappings) error {
	if !validEndpoint(endpoint) {
		return errors.New("URL must be HTTPS (HTTP allowed for loopback), without credentials, query or fragment")
	}

	if !validEnvName(apiKeyEnv) {
		return errors.New("requires a valid api_key_env name")
	}

	if strings.TrimSpace(models.Flash) == "" || strings.TrimSpace(models.Pro) == "" {
		return errors.New("requires flash and pro model mappings")
	}

	return nil
}

type modelMappings struct {
	Flash string `json:"flash"`
	Pro   string `json:"pro"`
}

func defaultProviderConfig() providerConfig {
	return providerConfig{
		BaseURL:   "https://api.deepseek.com",
		APIKeyEnv: "DEEPSEEK_API_KEY",
		Models:    modelMappings{Flash: "deepseek-flash", Pro: "deepseek-v4-pro"},
		Profile:   profileDeepSeek,
	}
}

func loadProviderConfig(cwd, userHome string) (providerFile, error) {
	for _, path := range []string{filepath.Join(cwd, ".mai.config"), filepath.Join(userHome, ".mai.config")} {
		file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return providerFile{}, fmt.Errorf("read %s: %w", path, err)
		}

		data, err := readBoundedFile(file, 1<<20)
		file.Close()
		if err != nil {
			return providerFile{}, fmt.Errorf("read %s: %w", path, err)
		}
		cfg, err := decodeProviderConfig(bytes.NewReader(data))
		if err != nil {
			return providerFile{}, fmt.Errorf("read %s: %w", path, err)
		}
		return cfg, nil
	}

	return providerFile{
		DefaultProvider: defaultProvider,
		Providers:       map[string]providerConfig{defaultProvider: defaultProviderConfig()},
	}, nil
}

func decodeProviderConfig(reader io.Reader) (providerFile, error) {
	cfg := providerFile{DefaultProvider: defaultProvider}
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return providerFile{}, fmt.Errorf("invalid configuration JSON: %w", err)
	}

	if err := decoder.Decode(new(any)); err != io.EOF {
		return providerFile{}, errors.New("configuration must contain one JSON object")
	}

	if len(cfg.Providers) == 0 {
		return providerFile{}, errors.New("configuration requires providers")
	}

	for name, provider := range cfg.Providers {
		if !validProviderName(name) {
			return providerFile{}, fmt.Errorf("invalid provider name %q", name)
		}

		if err := validateProviderSettings(provider.BaseURL, provider.APIKeyEnv, provider.Models); err != nil {
			return providerFile{}, fmt.Errorf("provider %q: %w", name, err)
		}

		profile, err := resolveProtocolProfile(name, provider.Profile)
		if err != nil {
			return providerFile{}, fmt.Errorf("provider %q: %w", name, err)
		}

		provider.Profile = profile
		cfg.Providers[name] = provider
	}

	if _, exists := cfg.Providers[cfg.DefaultProvider]; !exists {
		return providerFile{}, fmt.Errorf("default_provider %q is not configured", cfg.DefaultProvider)
	}

	return cfg, nil
}

func validProviderName(name string) bool {
	if name == "" || !(name[0] >= 'a' && name[0] <= 'z' || name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for _, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-' || char == '_') {
			return false
		}
	}
	return true
}

func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for index, char := range name {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char == '_' || index > 0 && char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func validEndpoint(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return false
	}
	return parsed.Scheme == "https" || parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1")
}
