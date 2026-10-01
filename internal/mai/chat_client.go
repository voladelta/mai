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
)

// A deliberately small text/tool Chat Completions adapter. It buffers one
// response and commits nothing until the provider reports a complete result.
// Unsupported media and opaque checkpoints fail rather than disappear.
type chatClient struct {
	httpClient     *http.Client
	endpoint       string
	model          string
	apiKey         string
	stdout         io.Writer
	allowSubagents bool
}

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (a *agent) configureBackend(sess *session) error {
	provider := os.Getenv("MAI_PROVIDER")
	mode := os.Getenv("MAI_COMPACTION")
	if mode != "" && mode != "native" && mode != "portable" {
		return errors.New("MAI_COMPACTION must be native or portable")
	}
	a.portable = mode == "portable"
	if deepseekModel(sess.Model) || strings.HasPrefix(sess.Backend, "deepseek:") {
		if provider != "" && provider != "deepseek" {
			return errors.New("DeepSeek models require MAI_PROVIDER=deepseek or an unset provider")
		}
		if mode == "native" {
			return errors.New("DeepSeek requires portable compaction")
		}
		return a.configureDeepSeek(sess)
	}
	if provider == "" || provider == "codex" {
		if sess.Backend != "" {
			return errors.New("saved chat task requires its original MAI_PROVIDER and MAI_CHAT_MODEL")
		}
		return nil
	}
	if provider != "chat" {
		return errors.New("MAI_PROVIDER must be codex, deepseek or chat (DeepSeek requires ds-flash or ds-pro)")
	}
	if mode == "native" {
		return errors.New("native compaction is unavailable for the chat provider")
	}
	endpoint := os.Getenv("MAI_CHAT_URL")
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && (parsed.Hostname() == "localhost" || parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "::1"))) {
		return errors.New("MAI_CHAT_URL must be an HTTPS completion URL (HTTP allowed for loopback)")
	}
	model := strings.TrimSpace(os.Getenv("MAI_CHAT_MODEL"))
	keyName := os.Getenv("MAI_CHAT_KEY_ENV")
	key := os.Getenv(keyName)
	window, err := strconv.ParseInt(os.Getenv("MAI_CONTEXT_WINDOW"), 10, 64)
	if model == "" || len(model) > 256 || keyName == "" || key == "" || err != nil || window < 32768 || window > 2_000_000 {
		return errors.New("chat requires MAI_CHAT_MODEL, MAI_CHAT_KEY_ENV naming a populated key variable, and MAI_CONTEXT_WINDOW (32768..2000000)")
	}
	identity := "chat:" + endpoint + ":" + model
	if sess.Backend != "" && sess.Backend != identity {
		return errors.New("saved task's chat endpoint/model differs from current configuration")
	}
	// Refuse an implicit migration from provider-specific reasoning artifacts.
	if _, err := chatHistory(sess.History, ""); err != nil {
		return fmt.Errorf("cannot use saved task with chat provider: %w", err)
	}
	sess.Backend = identity
	sess.Model = model
	a.backend = &chatClient{
		httpClient: &http.Client{Timeout: a.requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		endpoint:   endpoint, model: model, apiKey: key, stdout: a.client.stdout,
		allowSubagents: a.client.allowSubagents,
	}
	a.contextWindow = window
	a.portable = true
	return nil
}

func chatHistory(history []json.RawMessage, instructions string) ([]chatMessage, error) {
	messages := []chatMessage{}
	if instructions != "" {
		messages = append(messages, chatMessage{Role: "system", Content: instructions})
	}
	pending := map[string]bool{}
	for _, raw := range history {
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Output    json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return nil, err
		}
		switch item.Type {
		case "function_call":
			if item.CallID == "" || pending[item.CallID] {
				return nil, errors.New("invalid or duplicate tool call")
			}
			call := chatToolCall{ID: item.CallID, Type: "function"}
			call.Function.Name, call.Function.Arguments = item.Name, item.Arguments
			if len(messages) > 0 && len(messages[len(messages)-1].ToolCalls) > 0 {
				messages[len(messages)-1].ToolCalls = append(messages[len(messages)-1].ToolCalls, call)
			} else {
				messages = append(messages, chatMessage{Role: "assistant", ToolCalls: []chatToolCall{call}})
			}
			pending[item.CallID] = true
		case "function_call_output":
			if !pending[item.CallID] {
				return nil, errors.New("unpaired tool result")
			}
			text, err := transcriptText(item.Output, false)
			if err != nil {
				return nil, err
			}
			if text == "" {
				return nil, errors.New("non-text tool result is unsupported by chat provider")
			}
			messages = append(messages, chatMessage{Role: "tool", ToolCallID: item.CallID, Content: text})
			delete(pending, item.CallID)
		case "", "message":
			if len(pending) != 0 {
				return nil, errors.New("message interrupts pending tool results")
			}
			if item.Role != "user" && item.Role != "assistant" && item.Role != "developer" && item.Role != "system" {
				return nil, errors.New("unsupported message role")
			}
			text, err := portableMessageText(item.Content)
			if err != nil {
				return nil, err
			}
			role := item.Role
			if role == "developer" {
				role = "system"
			}
			messages = append(messages, chatMessage{Role: role, Content: text})
		default:
			return nil, fmt.Errorf("provider-specific or unsupported history item %q", item.Type)
		}
	}
	if len(pending) != 0 {
		return nil, errors.New("unresolved tool calls in chat history")
	}
	return messages, nil
}

func portableMessageText(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
	}
	var texts []string
	for _, part := range parts {
		if part.Type != "input_text" && part.Type != "output_text" {
			return "", errors.New("media is unsupported by portable text context")
		}
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n"), nil
}

func (c *chatClient) stream(ctx context.Context, sess *session, instructions string) (streamResult, error) {
	history, err := sess.requestHistory()
	if err != nil {
		return streamResult{}, err
	}
	messages, err := chatHistory(history, instructions)
	if err != nil {
		return streamResult{}, err
	}
	return c.complete(ctx, messages, true)
}

func (c *chatClient) summarize(ctx context.Context, _ *session, source string) (string, *tokenUsage, error) {
	result, err := c.complete(ctx, []chatMessage{{Role: "system", Content: checkpointInstructions}, {Role: "user", Content: source}}, false)
	if err != nil {
		return "", nil, err
	}
	for _, item := range result.items {
		entry, visible, err := visibleTranscriptEntry(item)
		if err != nil {
			return "", nil, err
		}
		if visible && entry.Kind == "assistant" {
			return entry.Text, result.usage, nil
		}
	}
	return "", result.usage, errors.New("chat summary has no text")
}

func (c *chatClient) complete(ctx context.Context, messages []chatMessage, toolsAllowed bool) (streamResult, error) {
	body := map[string]any{"model": c.model, "messages": messages, "stream": false, "max_tokens": 4096}
	// DeepSeek enables provider-specific thinking state by default. This text
	// adapter explicitly uses non-thinking mode instead of dropping required
	// reasoning_content between tool turns.
	if endpoint, err := url.Parse(c.endpoint); err == nil && endpoint.Hostname() == "api.deepseek.com" {
		body["thinking"] = map[string]string{"type": "disabled"}
	}
	if toolsAllowed {
		var tools []map[string]any
		for _, definition := range toolDefinitions(c.allowSubagents) {
			function := map[string]any{"name": definition["name"], "description": definition["description"], "parameters": definition["parameters"]}
			tools = append(tools, map[string]any{"type": "function", "function": function})
		}
		body["tools"], body["tool_choice"] = tools, "auto"
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return streamResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return streamResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient.Do(req)
	if err != nil {
		return streamResult{}, errors.New("chat provider request failed (network or timeout)")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return streamResult{}, fmt.Errorf("chat provider returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return streamResult{}, errors.New("chat provider response unreadable or exceeds 1 MiB")
	}
	var result struct {
		Choices []struct {
			Message      chatMessage `json:"message"`
			FinishReason string      `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			TotalTokens         int64  `json:"total_tokens"`
			PromptTokens        *int64 `json:"prompt_tokens"`
			CompletionTokens    *int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens *int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &result); err != nil || len(result.Choices) != 1 {
		return streamResult{}, errors.New("invalid chat provider completion")
	}
	choice := result.Choices[0]
	if toolsAllowed && choice.Message.ReasoningContent != nil && *choice.Message.ReasoningContent != "" {
		return streamResult{}, errors.New("chat provider requires unsupported thinking state; use a non-thinking configuration")
	}
	if choice.Message.Role != "assistant" || (choice.FinishReason != "stop" && choice.FinishReason != "tool_calls") {
		return streamResult{}, errors.New("chat completion truncated or unfinished")
	}
	if !toolsAllowed && len(choice.Message.ToolCalls) != 0 {
		return streamResult{}, errors.New("summary unexpectedly requested tools")
	}
	items := []json.RawMessage{}
	if choice.Message.Content != "" {
		item, _ := json.Marshal(map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": choice.Message.Content}}})
		items = append(items, item)
	}
	seen := map[string]bool{}
	for _, call := range choice.Message.ToolCalls {
		if call.Type != "function" || call.ID == "" || seen[call.ID] || call.Function.Name == "" || !json.Valid([]byte(call.Function.Arguments)) {
			return streamResult{}, errors.New("invalid chat tool call")
		}
		seen[call.ID] = true
		item, _ := json.Marshal(functionCall{Type: "function_call", CallID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
		items = append(items, item)
	}
	var usage *tokenUsage
	var totalTokens int64
	if result.Usage != nil {
		usage = &tokenUsage{TotalTokens: result.Usage.TotalTokens, InputTokens: result.Usage.PromptTokens, OutputTokens: result.Usage.CompletionTokens}
		totalTokens = usage.TotalTokens
		if cached := result.Usage.PromptTokensDetails.CachedTokens; cached != nil {
			usage.InputTokensDetails = &struct {
				CachedTokens *int64 `json:"cached_tokens,omitempty"`
			}{CachedTokens: cached}
		}
	}
	wrote := false
	if toolsAllowed && choice.Message.Content != "" {
		if _, err := io.WriteString(c.stdout, choice.Message.Content); err != nil {
			return streamResult{}, err
		}
		wrote = true
	}
	return streamResult{items: items, wrote: wrote, totalTokens: totalTokens, usage: usage}, nil
}
