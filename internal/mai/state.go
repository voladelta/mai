package mai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateVersion = 1

type config struct {
	Version int    `json:"version"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
}

type session struct {
	Version   int               `json:"version"`
	ID        string            `json:"id"`
	CWD       string            `json:"cwd"`
	RepoRoot  string            `json:"repo_root"`
	Model     string            `json:"model"`
	Effort    string            `json:"effort"`
	History   []json.RawMessage `json:"history"`
	CreatedAt time.Time         `json:"created_at"`
	UpdatedAt time.Time         `json:"updated_at"`
}

type paths struct {
	config  string
	session string
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
	dir := filepath.Join(home, ".mai")
	return paths{
		config:  filepath.Join(dir, "config.json"),
		session: filepath.Join(dir, "session.json"),
	}, nil
}

func loadConfig(path string) (config, error) {
	out := config{Version: stateVersion, Model: "luna", Effort: "max"}
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
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mai-state-*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary state: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		return fmt.Errorf("write temporary state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary state: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace state: %w", err)
	}
	keep = true
	return os.Chmod(path, 0o600)
}

func repairInterruptedToolCalls(sess *session) error {
	pending := make(map[string]bool)
	var order []string
	for _, raw := range sess.History {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return fmt.Errorf("parse saved history: %w", err)
		}
		switch item.Type {
		case "function_call":
			if item.CallID != "" && !pending[item.CallID] {
				pending[item.CallID] = true
				order = append(order, item.CallID)
			}
		case "function_call_output":
			delete(pending, item.CallID)
		}
	}
	for _, callID := range order {
		if !pending[callID] {
			continue
		}
		output, err := json.Marshal(map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  `{"ok":false,"error":"the previous mai process stopped before this tool call completed; reconsider before retrying"}`,
		})
		if err != nil {
			return err
		}
		sess.History = append(sess.History, output)
	}
	return nil
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
