package mai

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	defaultCodexURL    = "https://chatgpt.com/backend-api/codex/responses"
	defaultHTTPTimeout = 10 * time.Minute
)

type codexClient struct {
	httpClient *http.Client
	endpoint   string
	stdout     io.Writer
}

type streamResult struct {
	items []json.RawMessage
	wrote bool
}

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("Codex backend returned HTTP %d", e.status)
	}
	return fmt.Sprintf("Codex backend returned HTTP %d: %s", e.status, e.body)
}

func newCodexClient(stdout io.Writer, timeout time.Duration) *codexClient {
	endpoint := os.Getenv("MAI_CODEX_URL")
	if endpoint == "" {
		endpoint = defaultCodexURL
	}
	return &codexClient{
		httpClient: &http.Client{Timeout: timeout},
		endpoint:   endpoint,
		stdout:     stdout,
	}
}

func (c *codexClient) stream(ctx context.Context, sess *session, instructions string) (streamResult, error) {
	first, err := loadCredentials()
	if err != nil {
		return streamResult{}, err
	}
	result, err := c.streamWithCredentials(ctx, sess, instructions, first)
	var statusErr *httpStatusError
	if !errors.As(err, &statusErr) || statusErr.status != http.StatusUnauthorized {
		return result, err
	}

	second, reloadErr := loadCredentials()
	if reloadErr != nil {
		return streamResult{}, reloadErr
	}
	if sha256.Sum256([]byte(first.AccessToken)) == sha256.Sum256([]byte(second.AccessToken)) {
		return streamResult{}, loginError(second.Source)
	}
	return c.streamWithCredentials(ctx, sess, instructions, second)
}

func (c *codexClient) streamWithCredentials(ctx context.Context, sess *session, instructions string, creds credentials) (streamResult, error) {
	body := map[string]any{
		"model":               modelIDs[sess.Model],
		"store":               false,
		"stream":              true,
		"instructions":        instructions,
		"input":               sess.History,
		"tools":               toolDefinitions(),
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"reasoning": map[string]any{
			"effort":  effortIDs[sess.Effort],
			"summary": "auto",
		},
		"text":             map[string]string{"verbosity": "low"},
		"include":          []string{"reasoning.encrypted_content"},
		"prompt_cache_key": sess.ID,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return streamResult{}, fmt.Errorf("encode Codex request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return streamResult{}, fmt.Errorf("create Codex request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("chatgpt-account-id", creds.AccountID)
	req.Header.Set("originator", "mai")
	req.Header.Set("User-Agent", "mai/"+version)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("session-id", sess.ID)
	req.Header.Set("x-client-request-id", sess.ID)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return streamResult{}, fmt.Errorf("Codex request timed out after %s; use --timeout to change the limit", c.httpClient.Timeout)
		}
		return streamResult{}, fmt.Errorf("call Codex backend: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return streamResult{}, &httpStatusError{status: resp.StatusCode, body: strings.TrimSpace(string(b))}
	}
	result, err := c.readSSE(resp.Body)
	if errors.Is(err, context.DeadlineExceeded) {
		return streamResult{}, fmt.Errorf("Codex request timed out after %s; use --timeout to change the limit", c.httpClient.Timeout)
	}
	return result, err
}

func (c *codexClient) readSSE(r io.Reader) (streamResult, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	items := make(map[int]json.RawMessage)
	var dataLines []string
	var wrote bool
	var terminal bool

	dispatch := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if data == "[DONE]" {
			terminal = true
			return nil
		}
		var event struct {
			Type        string          `json:"type"`
			Delta       string          `json:"delta"`
			OutputIndex int             `json:"output_index"`
			Item        json.RawMessage `json:"item"`
			Response    *struct {
				Status string            `json:"status"`
				Error  json.RawMessage   `json:"error"`
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return fmt.Errorf("parse Codex stream event: %w", err)
		}
		switch event.Type {
		case "response.output_text.delta":
			if event.Delta != "" {
				if _, err := io.WriteString(c.stdout, event.Delta); err != nil {
					return err
				}
				wrote = true
			}
		case "response.output_item.done":
			if len(event.Item) > 0 {
				items[event.OutputIndex] = append(json.RawMessage(nil), event.Item...)
			}
		case "response.completed":
			terminal = true
			if event.Response != nil {
				if len(items) == 0 {
					for i, item := range event.Response.Output {
						items[i] = append(json.RawMessage(nil), item...)
					}
				}
				if event.Response.Status != "completed" {
					return fmt.Errorf("Codex response ended with status %q: %s", event.Response.Status, compactJSON(event.Response.Error))
				}
			}
		case "response.failed", "error":
			terminal = true
			if event.Response != nil {
				return fmt.Errorf("Codex response failed: %s", compactJSON(event.Response.Error))
			}
			return fmt.Errorf("Codex stream failed: %s", compactJSON(event.Error))
		}
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if dispatchErr := dispatch(); dispatchErr != nil {
					return streamResult{}, dispatchErr
				}
			} else if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return streamResult{}, fmt.Errorf("read Codex stream: %w", err)
			}
			if dispatchErr := dispatch(); dispatchErr != nil {
				return streamResult{}, dispatchErr
			}
			break
		}
	}
	if !terminal {
		return streamResult{}, errors.New("Codex stream ended before response.completed")
	}
	indexes := make([]int, 0, len(items))
	for index := range items {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	ordered := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		ordered = append(ordered, items[index])
	}
	return streamResult{items: ordered, wrote: wrote}, nil
}

func compactJSON(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "unknown error"
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"type": "function", "name": "read_skill",
			"description": "Read the complete SKILL.md for one installed skill. Read it before using that skill or loading its supporting files.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"skill": map[string]string{"type": "string", "description": "The installed skill directory id shown in the instructions."},
				},
				"required": []string{"skill"},
			},
		},
		{
			"type": "function", "name": "read_skill_file",
			"description": "Read one supporting file from a selected skill. Read only files required by SKILL.md. Images are returned as image content; unsupported binary files fail.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"skill": map[string]string{"type": "string", "description": "The installed skill directory id shown in the instructions."},
					"path":  map[string]string{"type": "string", "description": "A relative path inside the selected skill, commonly below assets, references, or scripts."},
				},
				"required": []string{"skill", "path"},
			},
		},
		{
			"type": "function", "name": "bash",
			"description": "Run Bash in the task working directory. Returns bounded head-and-tail output, exit code, timeout, duration, original byte counts, and truncation state.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"command":    map[string]string{"type": "string", "description": "The Bash command to run."},
					"timeout_ms": map[string]any{"type": "integer", "minimum": 1, "maximum": 600000},
				},
				"required": []string{"command"},
			},
		},
		{
			"type": "function", "name": "apply_patch",
			"description": "Create, update, move, or delete repository files with a Codex-style patch bounded by *** Begin Patch and *** End Patch.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"patch": map[string]string{"type": "string", "description": "A complete Codex apply_patch document."},
				},
				"required": []string{"patch"},
			},
		},
	}
}
