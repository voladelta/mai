package mai

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type credentials struct {
	AccessToken string
	AccountID   string
	Source      string
}

type authFile struct {
	Tokens *struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	} `json:"tokens"`
}

func loadCredentials() (credentials, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return credentials{}, fmt.Errorf("find home directory: %w", err)
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		codexHome = filepath.Join(home, ".codex")
	}
	path := filepath.Join(codexHome, "auth.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return credentials{}, loginError(path)
	}
	if err != nil {
		return credentials{}, fmt.Errorf("read Codex auth: %w", err)
	}
	var auth authFile
	if err := json.Unmarshal(b, &auth); err != nil {
		return credentials{}, fmt.Errorf("parse Codex auth: %w", err)
	}
	if auth.Tokens == nil || strings.TrimSpace(auth.Tokens.AccessToken) == "" {
		return credentials{}, loginError(path)
	}
	accountID := strings.TrimSpace(auth.Tokens.AccountID)
	if accountID == "" {
		accountID, err = accountIDFromJWT(auth.Tokens.AccessToken)
		if err != nil {
			return credentials{}, fmt.Errorf("Codex auth has no ChatGPT account ID: %w", err)
		}
	}
	return credentials{AccessToken: auth.Tokens.AccessToken, AccountID: accountID, Source: path}, nil
}

func loginError(path string) error {
	return fmt.Errorf("no ChatGPT Codex login found at %s; run: codex login", path)
}

func accountIDFromJWT(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", errors.New("access token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("decode JWT: %w", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parse JWT: %w", err)
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	accountID, _ := auth["chatgpt_account_id"].(string)
	if accountID == "" {
		return "", errors.New("JWT has no chatgpt_account_id claim")
	}
	return accountID, nil
}
