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
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCodexURL    = "https://chatgpt.com/backend-api/codex/responses"
	defaultHTTPTimeout = 10 * time.Minute
	maxRequestAttempts = 3
	maxRetryDelay      = 5 * time.Second
)

type codexClient struct {
	httpClient     *http.Client
	endpoint       string
	stdout         io.Writer
	allowSubagents bool
}

type streamResult struct {
	items       []json.RawMessage
	wrote       bool
	totalTokens int64
}

type sseEvent struct {
	Type        string          `json:"type"`
	Code        string          `json:"code"`
	Message     string          `json:"message"`
	Delta       string          `json:"delta"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
	Response    *sseResponse    `json:"response"`
	Error       json.RawMessage `json:"error"`
}

type sseResponse struct {
	Status string            `json:"status"`
	Error  json.RawMessage   `json:"error"`
	Output []json.RawMessage `json:"output"`
	Usage  *tokenUsage       `json:"usage"`
}

type tokenUsage struct {
	TotalTokens int64 `json:"total_tokens"`
}

type sseCollector struct {
	stdout    io.Writer
	items     map[int]json.RawMessage
	wrote     bool
	completed bool
	tokens    int64
}

type httpStatusError struct {
	status        int
	body          string
	retryAfter    time.Duration
	hasRetryAfter bool
}

type providerFailure struct {
	context string
	code    string
	detail  string
}

func (e *providerFailure) Error() string {
	return e.context + ": " + e.detail
}

var errIncompleteStream = errors.New("Codex stream ended before response.completed")
var errStreamRead = errors.New("read Codex stream")
var errOutputWrite = errors.New("write Codex output")

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
	return c.withCredentials(func(creds credentials) (streamResult, error) {
		return c.streamWithCredentials(ctx, sess, instructions, creds)
	})
}

func (c *codexClient) compact(ctx context.Context, sess *session, instructions string) (json.RawMessage, error) {
	result, err := c.withCredentials(func(creds credentials) (streamResult, error) {
		return c.compactWithCredentials(ctx, sess, instructions, creds)
	})
	if err != nil {
		return nil, err
	}
	var compaction json.RawMessage
	for _, raw := range result.items {
		var item struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, fmt.Errorf("parse Codex compaction output: %w", err)
		}
		if item.Type != "compaction" && item.Type != "compaction_summary" {
			continue
		}
		if compaction != nil {
			return nil, errors.New("Codex compaction returned more than one compaction item")
		}
		if item.EncryptedContent == "" {
			return nil, errors.New("Codex compaction returned empty encrypted content")
		}
		compaction = raw
	}
	if compaction == nil {
		return nil, fmt.Errorf("Codex compaction returned no compaction item in %d output items", len(result.items))
	}
	return compaction, nil
}

func (c *codexClient) withCredentials(request func(credentials) (streamResult, error)) (streamResult, error) {
	first, err := loadCredentials()
	if err != nil {
		return streamResult{}, err
	}
	result, err := request(first)
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
	return request(second)
}

func (c *codexClient) streamWithCredentials(ctx context.Context, sess *session, instructions string, creds credentials) (streamResult, error) {
	return c.requestWithCredentials(ctx, sess, instructions, creds, false)
}

func (c *codexClient) requestWithCredentials(ctx context.Context, sess *session, instructions string, creds credentials, compaction bool) (streamResult, error) {
	for attempt := 0; attempt < maxRequestAttempts; attempt++ {
		result, err := c.requestOnce(ctx, sess, instructions, creds, compaction)
		if err == nil || result.wrote || attempt == maxRequestAttempts-1 || !retryableRequestError(err) {
			return result, err
		}
		delay := time.Duration(200<<attempt) * time.Millisecond
		var statusErr *httpStatusError
		if errors.As(err, &statusErr) && statusErr.hasRetryAfter {
			delay = statusErr.retryAfter
		}
		if delay > maxRetryDelay {
			delay = maxRetryDelay
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return streamResult{}, ctx.Err()
		case <-timer.C:
		}
	}
	return streamResult{}, errors.New("Codex request retry limit reached")
}

func retryableRequestError(err error) bool {
	if errors.Is(err, errOutputWrite) {
		return false
	}
	var providerErr *providerFailure
	if errors.As(err, &providerErr) {
		switch providerErr.code {
		case "rate_limit_exceeded", "requests_limit_reached", "slow_down", "server_error", "overloaded", "server_is_overloaded", "service_unavailable", "internal_error", "timeout":
			return true
		default:
			return false
		}
	}
	var statusErr *httpStatusError
	if errors.As(err, &statusErr) {
		body := strings.ToLower(statusErr.body)
		quotaFailure := strings.Contains(body, "quota") ||
			strings.Contains(body, "usage_limit") ||
			strings.Contains(body, "credit_balance_exhausted") ||
			strings.Contains(body, "billing_hard_limit")
		if statusErr.status == http.StatusTooManyRequests && quotaFailure {
			return false
		}
		return statusErr.status == http.StatusRequestTimeout || statusErr.status == http.StatusTooManyRequests || statusErr.status >= 500
	}
	var netErr net.Error
	return errors.Is(err, errIncompleteStream) || errors.Is(err, errStreamRead) || errors.Is(err, io.EOF) || (errors.As(err, &netErr) && netErr.Temporary())
}

func (c *codexClient) requestOnce(ctx context.Context, sess *session, instructions string, creds credentials, compaction bool) (streamResult, error) {
	effort := sess.Effort
	if sess.RequestEffort != "" {
		effort = sess.RequestEffort
	}
	body := map[string]any{
		"model":               modelID(sess.Model),
		"store":               false,
		"stream":              true,
		"instructions":        instructions,
		"input":               sess.History,
		"tools":               toolDefinitions(c.allowSubagents),
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"reasoning": map[string]any{
			"effort":  effortIDs[effort],
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
	req.Header.Set("x-codex-beta-features", "remote_compaction_v2")
	if compaction {
		req.Header.Set("x-codex-turn-metadata", `{"request_kind":"compaction"}`)
	}

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
		retryAfter, hasRetryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
		return streamResult{}, &httpStatusError{
			status: resp.StatusCode, body: strings.TrimSpace(string(b)),
			retryAfter: retryAfter, hasRetryAfter: hasRetryAfter,
		}
	}
	result, err := c.readSSE(resp.Body)
	if errors.Is(err, context.DeadlineExceeded) {
		return result, fmt.Errorf("Codex request timed out after %s; use --timeout to change the limit", c.httpClient.Timeout)
	}
	return result, err
}

func parseRetryAfter(value string) (time.Duration, bool) {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		if seconds > int64(maxRetryDelay/time.Second) {
			return maxRetryDelay, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(when)), true
	}
	return 0, false
}

func (c *codexClient) compactWithCredentials(ctx context.Context, sess *session, instructions string, creds credentials) (streamResult, error) {
	trigger, err := json.Marshal(map[string]string{"type": "compaction_trigger"})
	if err != nil {
		return streamResult{}, err
	}
	compactSession := *sess
	compactSession.History = append(append([]json.RawMessage(nil), sess.History...), trigger)
	quietClient := *c
	quietClient.stdout = io.Discard
	return quietClient.requestWithCredentials(ctx, &compactSession, instructions, creds, true)
}

func (c *codexClient) readSSE(r io.Reader) (streamResult, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	collector := sseCollector{stdout: c.stdout, items: make(map[int]json.RawMessage)}
	var dataLines []string

	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			if line == "" {
				if dispatchErr := collector.dispatch(&dataLines); dispatchErr != nil {
					return streamResult{wrote: collector.wrote}, dispatchErr
				}
			} else if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return streamResult{wrote: collector.wrote}, fmt.Errorf("%w: %w", errStreamRead, err)
			}
			if dispatchErr := collector.dispatch(&dataLines); dispatchErr != nil {
				return streamResult{wrote: collector.wrote}, dispatchErr
			}
			break
		}
	}
	if !collector.completed {
		return streamResult{wrote: collector.wrote}, errIncompleteStream
	}
	return collector.result(), nil
}

func (collector *sseCollector) dispatch(dataLines *[]string) error {
	if len(*dataLines) == 0 {
		return nil
	}
	data := strings.Join(*dataLines, "\n")
	*dataLines = (*dataLines)[:0]
	return collector.consume(data)
}

func (collector *sseCollector) consume(data string) error {
	if data == "[DONE]" {
		return nil
	}

	var event sseEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return fmt.Errorf("parse Codex stream event: %w", err)
	}
	switch event.Type {
	case "response.output_text.delta":
		return collector.writeDelta(event.Delta)
	case "response.output_item.done":
		collector.collectItem(event.OutputIndex, event.Item)
	case "response.completed":
		return collector.complete(event.Response)
	case "response.failed", "error":
		return collector.fail(event)
	}
	return nil
}

func (collector *sseCollector) writeDelta(delta string) error {
	if delta == "" {
		return nil
	}
	if _, err := io.WriteString(collector.stdout, delta); err != nil {
		return fmt.Errorf("%w: %w", errOutputWrite, err)
	}
	collector.wrote = true
	return nil
}

func (collector *sseCollector) collectItem(index int, item json.RawMessage) {
	if len(item) > 0 {
		collector.items[index] = append(json.RawMessage(nil), item...)
	}
}

func (collector *sseCollector) complete(response *sseResponse) error {
	if response == nil {
		return errors.New("Codex response.completed is missing response")
	}
	if response.Status != "completed" {
		return newProviderFailure(fmt.Sprintf("Codex response ended with status %q", response.Status), response.Error)
	}

	collector.completed = true
	for i, item := range response.Output {
		if _, exists := collector.items[i]; !exists {
			collector.collectItem(i, item)
		}
	}
	if !collector.wrote {
		for _, item := range collector.result().items {
			var message struct {
				Type    string `json:"type"`
				Role    string `json:"role"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(item, &message); err != nil {
				return fmt.Errorf("parse Codex output item: %w", err)
			}
			if message.Type != "message" || message.Role != "assistant" {
				continue
			}
			for _, part := range message.Content {
				if part.Type == "output_text" {
					if err := collector.writeDelta(part.Text); err != nil {
						return err
					}
				}
			}
		}
	}
	if response.Usage != nil {
		collector.tokens = response.Usage.TotalTokens
	}

	return nil
}

func (collector *sseCollector) fail(event sseEvent) error {
	if event.Response != nil {
		return newProviderFailure("Codex response failed", event.Response.Error)
	}
	if len(event.Error) == 0 && (event.Code != "" || event.Message != "") {
		return &providerFailure{
			context: "Codex stream failed", code: strings.ToLower(event.Code),
			detail: fmt.Sprintf("%s (%s)", event.Message, event.Code),
		}
	}
	return newProviderFailure("Codex stream failed", event.Error)
}

func newProviderFailure(context string, raw json.RawMessage) error {
	var value struct {
		Code  string `json:"code"`
		Type  string `json:"type"`
		Error *struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &value)
	code := value.Code
	if code == "" && value.Error != nil {
		code = value.Error.Code
		if code == "" {
			code = value.Error.Type
		}
	}
	if code == "" {
		code = value.Type
	}
	return &providerFailure{context: context, code: strings.ToLower(code), detail: compactJSON(raw)}
}

func (collector *sseCollector) result() streamResult {
	indexes := make([]int, 0, len(collector.items))
	for index := range collector.items {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	ordered := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		ordered = append(ordered, collector.items[index])
	}
	return streamResult{items: ordered, wrote: collector.wrote, totalTokens: collector.tokens}
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

func toolDefinitions(allowSubagents ...bool) []map[string]any {
	definitions := []map[string]any{
		{
			"type": "function", "name": "read_skill",
			"description": "Read one file from an installed skill. Omit file to read SKILL.md; read it before using the skill or loading supporting files. Images are returned as image content; unsupported binary files fail.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"path": map[string]string{"type": "string", "description": "The installed skill directory id shown in the instructions."},
					"file": map[string]string{"type": "string", "description": "Optional relative file path inside the skill. Omit to read SKILL.md."},
				},
				"required": []string{"path"},
			},
		},
		{
			"type": "function", "name": "view_image",
			"description": "View a PNG, JPEG, or GIF image inside the repository. Returns image content and dimensions. Files are limited to 8 MiB and 8192 pixels per side.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"path": map[string]string{"type": "string", "description": "Absolute path or path relative to the task working directory."},
				},
				"required": []string{"path"},
			},
		},
		{
			"type": "function", "name": "bash",
			"description": "Run Bash in the task working directory. Returns bounded head-and-tail output, exit code, timeout, duration, byte counts, and truncation state. Truncated streams also return private capture file paths; each capture has a 32 MiB limit.",
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
			"type": "function", "name": "python",
			"description": "Execute a Python cell in a persistent namespace, or reset it. Returns the last expression, bounded stdout/stderr, generation, fresh and state_lost flags. Use exactly one of code or reset:true. State does not survive Mai exit or resume.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"code":  map[string]any{"type": "string", "minLength": 1},
					"reset": map[string]any{"type": "boolean", "enum": []bool{true}},
				},
				"oneOf": []map[string]any{
					{"required": []string{"code"}},
					{"required": []string{"reset"}},
				},
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
	if len(allowSubagents) > 0 && allowSubagents[0] {
		definitions = append(definitions, map[string]any{
			"type": "function", "name": "spawn_subagent",
			"description": "Run one installed custom agent synchronously in a stateless mai subprocess. The call returns after the child completes.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"name":   map[string]string{"type": "string", "description": "The custom agent name shown in the instructions."},
					"prompt": map[string]string{"type": "string", "description": "The complete task for the child agent."},
				},
				"required": []string{"name", "prompt"},
			},
		})
	}
	return definitions
}
