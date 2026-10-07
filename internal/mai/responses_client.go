package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Responses providers are stateless. Keep complete tool history locally and
// send portable text checkpoints instead of server-side conversation state.
type responsesClient struct {
	provider       string
	profile        protocolProfile
	httpClient     *http.Client
	endpoint       string
	apiKey         string
	stdout         io.Writer
	requestTimeout time.Duration
	contextWindow  int64
	skillsDisabled bool
}

func (a *agent) configureBackend(sess *session, provider providerConfig) error {
	if !validModelID(sess.Model) || !supportedEffort(sess.Effort) {
		return errors.New("Responses requires a model ID and effort l, h or max")
	}

	if sess.Provider == "" {
		sess.Provider = defaultProvider
	}

	var settings sessionBackend
	if sess.Backend != nil {
		settings = *sess.Backend
	} else {
		endpoint := strings.TrimRight(provider.BaseURL, "/") + "/responses"
		// MAI_BASE_URL overrides only the compiled-in default provider, never
		// a file-configured provider that shares its name.
		if provider.builtin && sess.Provider == defaultProvider && os.Getenv("MAI_BASE_URL") != "" {
			endpoint = os.Getenv("MAI_BASE_URL")
		}

		settings = sessionBackend{
			Endpoint:  endpoint,
			APIKeyEnv: provider.APIKeyEnv,
			Profile:   provider.Profile,
		}
	}

	if err := validateProviderSettings(settings.Endpoint, settings.APIKeyEnv); err != nil {
		return fmt.Errorf("provider %q: %w", sess.Provider, err)
	}

	profile, err := resolveProtocolProfile(sess.Provider, settings.Profile)
	if err != nil {
		return fmt.Errorf("provider %q: %w", sess.Provider, err)
	}

	settings.Profile = profile
	key := os.Getenv(settings.APIKeyEnv)
	if key == "" {
		return fmt.Errorf("provider %q requires %s", sess.Provider, settings.APIKeyEnv)
	}

	history, err := sess.requestHistory()
	if err != nil {
		return err
	}

	if err := validateResponsesHistory(history, profile); err != nil {
		return err
	}

	if sess.ContextTokens == 0 {
		sess.ContextTokens = estimateHistoryTokens(history)
	}

	window := int64(1_000_000)
	if value := os.Getenv("MAI_CONTEXT_WINDOW"); value != "" {
		var err error
		window, err = strconv.ParseInt(value, 10, 64)
		if err != nil || window < 32768 || window > 1_000_000 {
			return errors.New("MAI_CONTEXT_WINDOW must be 32768..1000000")
		}
	}

	a.contextWindow = window
	sess.Backend = &settings
	a.backend = &responsesClient{
		provider:       sess.Provider,
		profile:        profile,
		httpClient:     &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint:       settings.Endpoint,
		apiKey:         key,
		stdout:         a.modelOutput,
		requestTimeout: a.requestTimeout,
		contextWindow:  window,
		skillsDisabled: a.skipSkills,
	}

	return nil
}

func validateResponsesHistory(history []json.RawMessage, profile protocolProfile) error {
	_, err := validatedResponsesCallIDs(history, profile)
	return err
}

func validatedResponsesCallIDs(history []json.RawMessage, profile protocolProfile) (map[string]bool, error) {
	pending := map[string]bool{}
	seen := map[string]bool{}
	for _, raw := range history {
		var item struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Encrypted string          `json:"encrypted_content"`
			Content   json.RawMessage `json:"content"`
			Summary   json.RawMessage `json:"summary"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, errors.New("invalid Responses history item")
		}
		switch item.Type {
		case "", "message", "reasoning":
			if item.Encrypted != "" && profile == profileDeepSeek {
				return nil, errors.New("DeepSeek cannot replay encrypted reasoning; start a new task")
			}
			if item.Type == "reasoning" {
				if profile != profileDeepSeek {
					if len(item.Summary) > 0 && string(item.Summary) != "null" {
						var summary []json.RawMessage
						if err := json.Unmarshal(item.Summary, &summary); err != nil {
							return nil, errors.New("invalid Responses reasoning summary")
						}
						for _, rawPart := range summary {
							var text string
							if json.Unmarshal(rawPart, &text) == nil {
								continue
							}
							var part struct {
								Type string `json:"type"`
								Text string `json:"text"`
							}
							if err := json.Unmarshal(rawPart, &part); err != nil || part.Type != "summary_text" {
								return nil, errors.New("invalid Responses reasoning summary part")
							}
						}
						continue
					}
					if item.Encrypted != "" {
						continue
					}
				}
				var parts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(item.Content, &parts); err != nil || len(parts) == 0 {
					return nil, errors.New("Responses requires plain-text reasoning content")
				}
				for _, part := range parts {
					if part.Type != "reasoning_text" {
						return nil, errors.New("Responses cannot replay opaque reasoning content")
					}
				}
			}
		case "function_call":
			if item.CallID == "" || seen[item.CallID] || !json.Valid([]byte(item.Arguments)) {
				return nil, errors.New("invalid Responses tool call history")
			}
			if item.Name == "" {
				return nil, errors.New("Model returned an incomplete function call")
			}
			seen[item.CallID], pending[item.CallID] = true, true
		case "function_call_output":
			if !pending[item.CallID] {
				return nil, errors.New("unpaired Responses tool output")
			}
			delete(pending, item.CallID)
		default:
			return nil, fmt.Errorf("Responses cannot replay history type %q; start a new task", item.Type)
		}
	}
	if len(pending) != 0 {
		return nil, errors.New("Responses history has unresolved tool calls")
	}
	return seen, nil
}

func (c *responsesClient) stream(ctx context.Context, sess *session, instructions string) (streamResult, error) {
	return c.request(ctx, sess, instructions, true)
}

func (c *responsesClient) summarize(ctx context.Context, sess *session, source string) (string, *tokenUsage, error) {
	copySession := *sess
	copySession.History = nil
	copySession.ContextEdits = nil
	copySession.ReasoningStart = 0
	if err := appendUserPrompt(&copySession, source); err != nil {
		return "", nil, err
	}
	result, err := c.request(ctx, &copySession, checkpointInstructions, false)
	if err != nil {
		return "", nil, err
	}
	text, err := assistantResponseText(result.items, "")
	return text, result.usage, err
}

func (c *responsesClient) request(ctx context.Context, sess *session, instructions string, toolsAllowed bool) (streamResult, error) {
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
	seen, err := validatedResponsesCallIDs(history, c.profile)
	if err != nil {
		return streamResult{}, err
	}
	if !validModelID(sess.Model) || !supportedEffort(sess.Effort) {
		return streamResult{}, errors.New("Responses requires a model ID and effort l, h or max")
	}
	effort := effortIDs[sess.Effort]
	if c.profile == profileOpenRouter && sess.Effort == "max" {
		effort = "xhigh"
	}
	body := map[string]any{
		"model":             sess.Model,
		"input":             history,
		"instructions":      instructions,
		"stream":            true,
		"max_output_tokens": 32768,
		"reasoning":         map[string]string{"effort": effort},
		"tool_choice":       "none",
	}
	if toolsAllowed {
		var tools []map[string]any
		for _, definition := range toolDefinitions() {
			if definition["name"] == "read_skill" && c.skillsDisabled {
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
		return streamResult{}, errors.New("Responses request exceeds 48 MiB; reduce image/history input")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return streamResult{}, errors.New("invalid Responses request URL")
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
		return streamResult{}, fmt.Errorf("provider %q Responses request failed (network or timeout)", c.provider)
	}
	defer response.Body.Close()
	var reader io.Reader = response.Body
	if idleTimer != nil {
		idleTimer.Reset(c.requestTimeout)
		reader = idleResetReader{reader: response.Body, timer: idleTimer, timeout: c.requestTimeout}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return streamResult{}, fmt.Errorf("provider %q Responses returned HTTP %d", c.provider, response.StatusCode)
	}
	output := c.stdout
	if !toolsAllowed {
		output = io.Discard
	}
	// Failed streams never admit tool calls.
	parser := responseStream{stdout: output}
	result, err := parser.readSSE(io.LimitReader(reader, 16<<20))
	if err != nil {
		return streamResult{wrote: result.wrote}, fmt.Errorf("provider %q Responses stream failed or incomplete", c.provider)
	}
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
			return streamResult{}, errors.New("invalid Responses response item")
		}
		if item.Status != "" && item.Status != "completed" {
			return streamResult{}, errors.New("unfinished Responses response item")
		}
		if item.Type == "message" {
			text, err := portableMessageText(item.Content)
			if err != nil || item.Role != "assistant" || text == "" {
				return streamResult{}, errors.New("invalid Responses assistant message")
			}
		}
		if item.Type == "function_call" {
			if !toolsAllowed || item.CallID == "" || seen[item.CallID] || item.Name == "" || !json.Valid([]byte(item.Arguments)) {
				return streamResult{}, errors.New("invalid or unexpected Responses tool call")
			}
			seen[item.CallID] = true
		}
		if item.Type != "function_call" && item.Type != "reasoning" && item.Type != "message" {
			return streamResult{}, errors.New("unsupported Responses response item")
		}
		if item.Type == "reasoning" {
			if c.profile == profileDeepSeek {
				// DeepSeek replay requires plain reasoning without opaque fields.
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					return streamResult{}, errors.New("invalid Responses reasoning")
				}
				delete(fields, "encrypted_content")
				delete(fields, "summary")
				plain, err := json.Marshal(fields)
				if err != nil {
					return streamResult{}, err
				}
				result.items[index] = plain
			}
			if err := validateResponsesHistory([]json.RawMessage{result.items[index]}, c.profile); err != nil {
				return streamResult{}, err
			}
		}
	}
	return result, nil
}
