package mai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const stateVersion = 1

type config struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

type session struct {
	Version  int               `json:"version"`
	ID       string            `json:"id"`
	CWD      string            `json:"cwd"`
	RepoRoot string            `json:"repo_root"`
	Model    string            `json:"model"`
	Effort   string            `json:"effort"`
	History  []json.RawMessage `json:"history"`
}

type paths struct {
	config        string
	session       string
	legacyConfig  string
	legacySession string
}

func statePaths() (paths, error) {
	if dir := os.Getenv("MAI_STATE_DIR"); dir != "" {
		dir = filepath.Clean(dir)
		return paths{
			config:  filepath.Join(dir, "config.json"),
			session: filepath.Join(dir, "session.json"),
		}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return paths{}, fmt.Errorf("find home directory: %w", err)
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	} else if !filepath.IsAbs(configHome) {
		return paths{}, errors.New("XDG_CONFIG_HOME must be an absolute path")
	}
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		stateHome = filepath.Join(home, ".local", "state")
	} else if !filepath.IsAbs(stateHome) {
		return paths{}, errors.New("XDG_STATE_HOME must be an absolute path")
	}
	legacyDir := filepath.Join(home, ".mai")
	return paths{
		config:        filepath.Join(configHome, "mai", "config.json"),
		session:       filepath.Join(stateHome, "mai", "session.json"),
		legacyConfig:  filepath.Join(legacyDir, "config.json"),
		legacySession: filepath.Join(legacyDir, "session.json"),
	}, nil
}

func migrateLegacyState(state paths) error {
	if state.legacyConfig == "" {
		return nil
	}
	if err := migrateFile(state.legacyConfig, state.config); err != nil {
		return fmt.Errorf("migrate %s: %w", state.legacyConfig, err)
	}
	if err := migrateFile(state.legacySession, state.session); err != nil {
		return fmt.Errorf("migrate %s: %w", state.legacySession, err)
	}
	return nil
}

func migrateFile(source, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	b, err := os.ReadFile(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".mai-migrate-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(b); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(tempName, destination); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return nil
}

func loadConfig(path string) (config, error) {
	out := config{Model: "luna", Effort: "m"}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("parse %s: %w", path, err)
	}
	if _, ok := modelIDs[out.Model]; !ok {
		return out, fmt.Errorf("config has invalid model %q", out.Model)
	}
	if _, ok := effortIDs[out.Effort]; !ok {
		return out, fmt.Errorf("config has invalid effort %q", out.Effort)
	}
	return out, nil
}

func loadSession(path string) (*session, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("no saved task; run mai \"prompt\" first")
	}
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	var out session
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if out.Version != stateVersion || out.ID == "" || out.CWD == "" || out.RepoRoot == "" {
		return nil, fmt.Errorf("saved session is incomplete or unsupported")
	}
	if _, ok := modelIDs[out.Model]; !ok {
		return nil, fmt.Errorf("saved session has invalid model %q", out.Model)
	}
	if _, ok := effortIDs[out.Effort]; !ok {
		return nil, fmt.Errorf("saved session has invalid effort %q", out.Effort)
	}
	return &out, nil
}

func saveJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("secure state directory: %w", err)
	}
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode state: %w", err)
	}
	b = append(b, '\n')
	if err := atomicWriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return os.Chmod(path, 0o600)
}

func repairInterruptedToolCalls(sess *session) error {
	pending := make(map[string]functionCall)
	var order []string
	for _, raw := range sess.History {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("parse saved history: %w", err)
		}
		switch item.Type {
		case "function_call":
			if item.CallID != "" {
				if _, exists := pending[item.CallID]; exists {
					continue
				}
				pending[item.CallID] = functionCall{CallID: item.CallID, Name: item.Name}
				order = append(order, item.CallID)
			}
		case "function_call_output":
			delete(pending, item.CallID)
		}
	}
	for _, callID := range order {
		call, exists := pending[callID]
		if !exists {
			continue
		}
		recovery := map[string]any{
			"outcome":     "unknown",
			"error":       "the previous mai process stopped before this tool call output was saved; the tool outcome is unknown",
			"instruction": interruptedToolInstruction(call.Name),
		}
		recoveryJSON, err := json.Marshal(recovery)
		if err != nil {
			return err
		}
		output, err := json.Marshal(map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  string(recoveryJSON),
		})
		if err != nil {
			return err
		}
		sess.History = append(sess.History, output)
	}
	return nil
}

func interruptedToolInstruction(name string) string {
	switch name {
	case "apply_patch":
		return "Inspect the repository and reconcile the requested patch with the current files before you retry apply_patch."
	case "bash":
		return "Inspect the command effects. Do not repeat a command that can have non-idempotent effects without user confirmation."
	default:
		return "Inspect the relevant state before you retry the tool."
	}
}

func newSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate session ID: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16])), nil
}
