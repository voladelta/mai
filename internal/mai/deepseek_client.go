package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// DeepSeek Responses is stateless. Keep its plain reasoning and complete tool
// history locally and send plain checkpoints on subsequent requests.
type deepseekClient struct {
	httpClient     *http.Client
	endpoint       string
	apiKey         string
	stdout         io.Writer
	allowSubagents bool
	requestTimeout time.Duration
	contextWindow  int64
}

func (a *agent) configureBackend(sess *session) error {
	if !deepseekModel(sess.Model) || !deepseekEffort(sess.Effort) {
		return errors.New("DeepSeek requires ds-flash or ds-pro and effort l, h or max")
	}
	endpoint := os.Getenv("MAI_DEEPSEEK_URL")
	if endpoint == "" {
		endpoint = "https://api.deepseek.com/responses"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"))) {
		return errors.New("MAI_DEEPSEEK_URL must be an HTTPS Responses URL (HTTP allowed for loopback)")
	}
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		return errors.New("DeepSeek requires DEEPSEEK_API_KEY")
	}
	if err := validateDeepSeekHistory(sess.History, sess.Model); err != nil {
		return err
	}
	window := int64(1_000_000)
	if value := os.Getenv("MAI_CONTEXT_WINDOW"); value != "" {
		window, err = strconv.ParseInt(value, 10, 64)
		if err != nil || window < 32768 || window > 1_000_000 {
			return errors.New("DeepSeek MAI_CONTEXT_WINDOW must be 32768..1000000")
		}
	}
	a.contextWindow = window
	a.backend = &deepseekClient{
		httpClient: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint:   endpoint, apiKey: key, stdout: a.modelOutput, allowSubagents: a.allowSubagents,
		requestTimeout: a.requestTimeout, contextWindow: window,
	}
	return nil
}

func validateDeepSeekHistory(history []json.RawMessage, model string) error {
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, raw := range history {
		var item struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Arguments string          `json:"arguments"`
			Encrypted string          `json:"encrypted_content"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return errors.New("invalid DeepSeek history item")
		}
		switch item.Type {
		case "", "message", "reasoning":
			if item.Encrypted != "" {
				return errors.New("DeepSeek cannot replay encrypted reasoning; start a new task")
			}
			if item.Type == "reasoning" {
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(item.Content, &parts); err != nil || len(parts) == 0 {
					return errors.New("DeepSeek requires plain-text reasoning content")
				}
				for _, part := range parts {
					if part.Type != "reasoning_text" {
						return errors.New("DeepSeek cannot replay opaque reasoning content")
					}
				}
			}
		case "function_call":
			if item.CallID == "" || seen[item.CallID] || !json.Valid([]byte(item.Arguments)) {
				return errors.New("invalid DeepSeek tool call history")
			}
			seen[item.CallID], pending[item.CallID] = true, true
		case "function_call_output":
			if !pending[item.CallID] {
				return errors.New("unpaired DeepSeek tool output")
			}
			delete(pending, item.CallID)
		default:
			return fmt.Errorf("DeepSeek cannot replay history type %q; start a new task", item.Type)
		}
		if model == "ds-pro" && bytes.Contains(raw, []byte(`"input_image"`)) {
			return errors.New("DeepSeek Pro does not support images")
		}
	}
	if len(pending) != 0 {
		return errors.New("DeepSeek history has unresolved tool calls")
	}
	return nil
}

func (c *deepseekClient) stream(ctx context.Context, sess *session, instructions string) (streamResult, error) {
	return c.request(ctx, sess, instructions, true)
}

func (c *deepseekClient) summarize(ctx context.Context, sess *session, source string) (string, *tokenUsage, error) {
	copySession := *sess
	copySession.History = nil
	copySession.ContextEdits = nil
	if err := appendUserPrompt(&copySession, source); err != nil {
		return "", nil, err
	}
	result, err := c.request(ctx, &copySession, checkpointInstructions, false)
	if err != nil {
		return "", nil, err
	}
	var text strings.Builder
	for _, raw := range result.items {
		entry, visible, err := visibleTranscriptEntry(raw)
		if err != nil {
			return "", result.usage, err
		}
		if visible && entry.Kind == "assistant" {
			text.WriteString(entry.Text)
		}
	}
	return text.String(), result.usage, nil
}

func (c *deepseekClient) request(ctx context.Context, sess *session, instructions string, toolsAllowed bool) (streamResult, error) {
	history, err := sess.requestHistory()
	if err != nil {
		return streamResult{}, err
	}
	window := c.contextWindow
	if window == 0 {
		window = modelContextWindow
	}
	if toolsAllowed && sess.ContextTokens >= window/2 {
		if hint := contextEditHint(history); hint != nil {
			history = append(history, hint)
		}
	}
	if err := validateDeepSeekHistory(history, sess.Model); err != nil {
		return streamResult{}, err
	}
	if !deepseekEffort(sess.Effort) {
		return streamResult{}, errors.New("DeepSeek effort must be l, h or max")
	}
	body := map[string]any{
		"model": modelID(sess.Model), "input": history, "instructions": instructions,
		"stream": true, "max_output_tokens": 32768,
		"reasoning":   map[string]string{"effort": effortIDs[sess.Effort]},
		"tool_choice": "none",
	}
	if toolsAllowed {
		var tools []map[string]any
		for _, definition := range toolDefinitions(c.allowSubagents) {
			if sess.Model == "ds-pro" && definition["name"] == "view_image" {
				continue
			}
			tools = append(tools, definition)
		}
		body["tools"], body["tool_choice"] = tools, "auto"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return streamResult{}, err
	}
	if len(payload) > 48<<20 {
		return streamResult{}, errors.New("DeepSeek request exceeds 48 MiB; reduce image/history input")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return streamResult{}, errors.New("invalid DeepSeek request URL")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var idleTimer *time.Timer
	if c.requestTimeout > 0 {
		idleTimer = time.AfterFunc(c.requestTimeout, cancel)
		defer idleTimer.Stop()
	}
	req = req.WithContext(requestCtx)
	response, err := c.httpClient.Do(req)
	if err != nil {
		return streamResult{}, errors.New("DeepSeek Responses request failed (network or timeout)")
	}
	defer response.Body.Close()
	var reader io.Reader = response.Body
	if idleTimer != nil {
		idleTimer.Reset(c.requestTimeout)
		reader = idleResetReader{reader: response.Body, timer: idleTimer, timeout: c.requestTimeout}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return streamResult{}, fmt.Errorf("DeepSeek Responses returned HTTP %d", response.StatusCode)
	}
	output := c.stdout
	if !toolsAllowed {
		output = io.Discard
	}
	// Failed streams never admit tool calls.
	parser := responseStream{stdout: output}
	result, err := parser.readSSE(io.LimitReader(reader, 16<<20))
	if err != nil {
		return streamResult{wrote: result.wrote}, errors.New("DeepSeek Responses stream failed or incomplete")
	}
	seen := map[string]bool{}
	for index, raw := range result.items {
		var item struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Status    string          `json:"status"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return streamResult{}, errors.New("invalid DeepSeek response item")
		}
		if item.Status != "" && item.Status != "completed" {
			return streamResult{}, errors.New("unfinished DeepSeek response item")
		}
		if item.Type == "message" {
			text, err := portableMessageText(item.Content)
			if err != nil || item.Role != "assistant" || text == "" {
				return streamResult{}, errors.New("invalid DeepSeek assistant message")
			}
		}
		if item.Type == "function_call" && (!toolsAllowed || item.CallID == "" || seen[item.CallID] || item.Name == "" || !json.Valid([]byte(item.Arguments))) {
			return streamResult{}, errors.New("invalid or unexpected DeepSeek tool call")
		}
		seen[item.CallID] = true
		if item.Type != "function_call" && item.Type != "reasoning" && item.Type != "message" {
			return streamResult{}, errors.New("unsupported DeepSeek response item")
		}
		if item.Type == "reasoning" {
			// Live Responses also supplies an opaque encrypted field. Replay
			// only the documented plain content after proving it is present.
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				return streamResult{}, errors.New("invalid DeepSeek reasoning")
			}
			delete(fields, "encrypted_content")
			delete(fields, "summary")
			plain, err := json.Marshal(fields)
			if err != nil {
				return streamResult{}, err
			}
			if err := validateDeepSeekHistory([]json.RawMessage{plain}, sess.Model); err != nil {
				return streamResult{}, err
			}
			result.items[index] = plain
		}
	}
	return result, nil
}
