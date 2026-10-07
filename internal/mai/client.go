package mai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const defaultHTTPTimeout = 10 * time.Minute

type responseStream struct{ stdout io.Writer }

type streamResult struct {
	items       []json.RawMessage
	wrote       bool
	totalTokens int64
	usage       *tokenUsage
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
	TotalTokens        int64  `json:"total_tokens"`
	InputTokens        *int64 `json:"input_tokens,omitempty"`
	OutputTokens       *int64 `json:"output_tokens,omitempty"`
	InputTokensDetails *struct {
		CachedTokens *int64 `json:"cached_tokens,omitempty"`
	} `json:"input_tokens_details,omitempty"`
}

type sseCollector struct {
	stdout    io.Writer
	items     map[int]json.RawMessage
	wrote     bool
	completed bool
	usage     *tokenUsage
}

var errIncompleteStream = errors.New("Responses stream ended before response.completed")
var errStreamRead = errors.New("read Responses stream")
var errOutputWrite = errors.New("write Responses output")

type idleResetReader struct {
	reader  io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (r idleResetReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

func (c *responseStream) readSSE(r io.Reader) (streamResult, error) {
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
		return fmt.Errorf("parse Responses stream event: %w", err)
	}
	switch event.Type {
	case "response.output_text.delta":
		return collector.writeDelta(event.Delta)
	case "response.output_item.done":
		collector.collectItem(event.OutputIndex, event.Item)
	case "response.completed":
		return collector.complete(event.Response)
	case "response.failed", "response.incomplete", "error":
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
		collector.items[index] = item
	}
}

func (collector *sseCollector) complete(response *sseResponse) error {
	if response == nil {
		return errors.New("Responses response.completed is missing response")
	}
	if response.Status != "completed" {
		return fmt.Errorf("Responses response ended with status %q: %s", response.Status, compactJSON(response.Error))
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
				return fmt.Errorf("parse Responses output item: %w", err)
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
		collector.usage = response.Usage
	}

	return nil
}

func (collector *sseCollector) fail(event sseEvent) error {
	if event.Response != nil {
		return fmt.Errorf("Responses response failed: %s", compactJSON(event.Response.Error))
	}
	if len(event.Error) == 0 && (event.Code != "" || event.Message != "") {
		return fmt.Errorf("Responses stream failed: %s (%s)", event.Message, event.Code)
	}
	return fmt.Errorf("Responses stream failed: %s", compactJSON(event.Error))
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
	result := streamResult{items: ordered, wrote: collector.wrote, usage: collector.usage}
	if collector.usage != nil {
		result.totalTokens = collector.usage.TotalTokens
	}
	return result
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
			"description": "Run a command with bash -c in the task working directory. Every call is a fresh non-login shell: cd, exports, and variables do not persist, and stdin is unavailable, so avoid interactive commands. A nonzero exit is a result to interpret, not a tool error. Default timeout 2 minutes, maximum 10. Returns bounded head-and-tail output, exit code, timeout, duration, byte counts, and truncation state; truncated streams also return private capture file paths (32 MiB limit each). Before deleting or moving, resolve and verify the absolute target path, and guard variables in such paths with ${VAR:?}. Use read, write, and edit for text files, not cat, heredocs, or sed.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"description": map[string]string{"type": "string", "description": "Optional active-voice label of 5-10 words for this call. Provide before command."},
					"command":     map[string]string{"type": "string", "description": "The Bash command to run."},
					"timeout_ms":  map[string]any{"type": "integer", "minimum": 1, "maximum": 600000},
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
			"type": "function", "name": "read",
			"description": "Read a repository UTF-8 text file and observe its current version for write/edit. Paths are relative to the repository root. Returns numbered lines (`N: text`) and a footer giving the shown range and the offset to continue from. At most 2000 lines and 50 KiB per call; lines longer than 2000 characters are truncated (use Bash to inspect the rest); files at most 16 MiB.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"file_path": map[string]string{"type": "string", "description": "File path relative to the repository root."},
					"offset":    map[string]any{"type": "integer", "minimum": 1, "description": "First line, 1-based. Defaults to 1."},
					"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 2000, "description": "Maximum lines. Defaults to 2000."},
				},
				"required": []string{"file_path"},
			},
		},
		{
			"type": "function", "name": "write",
			"description": "Create or fully replace a repository UTF-8 text file, at most 16 MiB. Paths are relative to the repository root. Read existing files first with read; Bash reads do not count. Prefer edit for targeted changes. A file changed since observation requires rereading. Each call commits one file.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"file_path": map[string]string{"type": "string", "description": "File path relative to the repository root. Provide before content."},
					"content":   map[string]string{"type": "string", "description": "Complete UTF-8 contents, including intended newlines. Empty string creates or truncates an empty file."},
				},
				"required": []string{"file_path", "content"},
			},
		},
		{
			"type": "function", "name": "edit",
			"description": "Edit a repository UTF-8 text file by exact literal replacement. Paths are relative to the repository root. Read first unless just created/edited in this session. Whitespace must match; CRLF/LF are normalized and original line endings preserved. Reread if stale. Each call commits one file.",
			"parameters": map[string]any{
				"type": "object", "additionalProperties": false,
				"properties": map[string]any{
					"file_path":   map[string]string{"type": "string", "description": "File path relative to the repository root. Provide before old_string and new_string."},
					"old_string":  map[string]any{"type": "string", "minLength": 1, "description": "Literal text to replace. Include enough surrounding context for exactly one match."},
					"new_string":  map[string]string{"type": "string", "description": "Replacement text. Empty string deletes the match."},
					"replace_all": map[string]any{"type": "boolean", "description": "Replace every match. Defaults to false, which requires exactly one match."},
				},
				"required": []string{"file_path", "old_string", "new_string"},
			},
		},
	}
}
