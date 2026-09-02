package mai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const stateVersion = 1

type taskConfig struct {
	Model  string
	Effort string
}

type session struct {
	Version       int               `json:"version"`
	ID            string            `json:"id"`
	CWD           string            `json:"cwd"`
	RepoRoot      string            `json:"repo_root"`
	Model         string            `json:"model"`
	Effort        string            `json:"effort"`
	ContextTokens int64             `json:"context_tokens,omitempty"`
	History       []json.RawMessage `json:"history"`
}

type sessionPaths struct {
	dir      string
	current  string
	sessions string
	locks    string
}

func projectSessionPaths(repoRoot string) sessionPaths {
	dir := filepath.Join(repoRoot, ".mai")
	return sessionPaths{
		dir:      dir,
		current:  filepath.Join(dir, "current"),
		sessions: filepath.Join(dir, "sessions"),
		locks:    filepath.Join(dir, "locks"),
	}
}

func prepareSessionPaths(paths sessionPaths) error {
	for _, dir := range []string{paths.dir, paths.sessions, paths.locks} {
		info, err := os.Lstat(dir)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(dir, 0o700); err != nil {
				return fmt.Errorf("create session directory: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("inspect session directory: %w", err)
		} else if !info.IsDir() {
			return fmt.Errorf("session path is not a directory: %s", dir)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure session directory: %w", err)
		}
	}
	ignore := filepath.Join(paths.dir, ".gitignore")
	if info, err := os.Lstat(ignore); errors.Is(err, os.ErrNotExist) {
		if err := atomicWriteFile(ignore, []byte("*\n"), 0o600); err != nil {
			return fmt.Errorf("create .mai/.gitignore: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("inspect .mai/.gitignore: %w", err)
	} else if !info.Mode().IsRegular() {
		return errors.New(".mai/.gitignore is not a regular file")
	}
	return nil
}

func sessionPath(paths sessionPaths, id string) string {
	return filepath.Join(paths.sessions, id+".json")
}

func saveCurrentSession(paths sessionPaths, id string) error {
	if err := atomicWriteFile(paths.current, []byte(id+"\n"), 0o600); err != nil {
		return fmt.Errorf("save current session: %w", err)
	}
	return os.Chmod(paths.current, 0o600)
}

func loadCurrentSessionID(paths sessionPaths) (string, error) {
	if info, err := os.Lstat(paths.dir); err == nil && !info.IsDir() {
		return "", errors.New(".mai is not a directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect .mai: %w", err)
	}
	b, err := readRegularFile(paths.current)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("no saved task in this project; use --persist to save a new task")
	}
	if err != nil {
		return "", fmt.Errorf("read current session: %w", err)
	}
	id := strings.TrimSpace(string(b))
	if !validSessionID(id) {
		return "", errors.New("current session ID is invalid")
	}
	return id, nil
}

func validSessionID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	_, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	return err == nil
}

func acquireSessionLock(paths sessionPaths, id string) (*os.File, error) {
	lockPath := filepath.Join(paths.locks, id+".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session lock: %w", err)
	}
	if err := lock.Chmod(0o600); err != nil {
		lock.Close()
		return nil, fmt.Errorf("secure session lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("session %s is already running", id)
		}
		return nil, fmt.Errorf("lock session %s: %w", id, err)
	}
	return lock, nil
}

func loadSession(path string) (*session, error) {
	b, err := readRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("saved task file does not exist")
	}
	if err != nil {
		return nil, fmt.Errorf("read session: %w", err)
	}
	var out session
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if out.Version != stateVersion || !validSessionID(out.ID) || out.CWD == "" || out.RepoRoot == "" || out.ContextTokens < 0 {
		return nil, fmt.Errorf("saved session is incomplete or unsupported")
	}
	if _, ok := modelIDs[out.Model]; !ok {
		return nil, fmt.Errorf("saved session has invalid model %q", out.Model)
	}
	if _, ok := effortIDs[out.Effort]; !ok {
		return nil, fmt.Errorf("saved session has invalid effort %q", out.Effort)
	}
	if out.ContextTokens == 0 && len(out.History) > 0 {
		out.ContextTokens = estimateHistoryTokens(out.History)
	}
	return &out, nil
}

func readRegularFile(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return io.ReadAll(file)
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
		sess.appendEstimatedHistory(output)
	}
	return nil
}

func (sess *session) appendEstimatedHistory(items ...json.RawMessage) {
	sess.History = append(sess.History, items...)
	for _, item := range items {
		sess.ContextTokens += estimateHistoryItemTokens(item)
	}
}

func estimateHistoryTokens(items []json.RawMessage) int64 {
	var total int64
	for _, item := range items {
		total += estimateHistoryItemTokens(item)
	}
	return total
}

func estimateHistoryItemTokens(item json.RawMessage) int64 {
	var envelope struct {
		Type             string `json:"type"`
		EncryptedContent string `json:"encrypted_content"`
	}
	if json.Unmarshal(item, &envelope) == nil &&
		(envelope.Type == "compaction" || envelope.Type == "compaction_summary") {
		// Codex estimates the decoded encrypted payload, less fixed envelope overhead.
		bytes := int64(len(envelope.EncryptedContent))*3/4 - 650
		if bytes < 0 {
			bytes = 0
		}
		return (bytes + 3) / 4
	}
	return (int64(len(item)) + 3) / 4
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
