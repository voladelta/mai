package mai

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// A transcript entry contains only text that was visible to the model.
// Reasoning, compaction, and configuration items never enter this archive.
type transcriptEntry struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Name   string `json:"name,omitempty"`
	CallID string `json:"call_id,omitempty"`
}

func visibleTranscriptEntry(raw json.RawMessage) (transcriptEntry, bool, error) {
	var item struct {
		Type      string          `json:"type"`
		Role      string          `json:"role"`
		Content   json.RawMessage `json:"content"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments string          `json:"arguments"`
		Output    json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return transcriptEntry{}, false, err
	}

	switch {
	case (item.Type == "message" || item.Type == "") && (item.Role == "user" || item.Role == "assistant"):
		var content string
		if len(item.Content) > 0 && item.Content[0] == '"' {
			if err := json.Unmarshal(item.Content, &content); err != nil {
				return transcriptEntry{}, false, err
			}
		} else if len(item.Content) > 0 && item.Content[0] == '[' {
			var parts []json.RawMessage
			if err := json.Unmarshal(item.Content, &parts); err != nil {
				return transcriptEntry{}, false, err
			}
			var texts []string
			for _, rawPart := range parts {
				var part struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(rawPart, &part) != nil {
					continue
				}
				if part.Type == "input_text" || part.Type == "output_text" {
					texts = append(texts, part.Text)
				}
			}
			content = strings.Join(texts, "\n")
		}
		return transcriptEntry{Kind: item.Role, Text: content}, content != "", nil
	case item.Type == "function_call":
		return transcriptEntry{Kind: "tool_call", Name: item.Name, CallID: item.CallID, Text: item.Arguments}, true, nil
	case item.Type == "function_call_output":
		var output string
		if len(item.Output) > 0 && item.Output[0] == '"' {
			if err := json.Unmarshal(item.Output, &output); err != nil {
				return transcriptEntry{}, false, err
			}
		} else if len(item.Output) > 0 && item.Output[0] == '[' {
			var parts []json.RawMessage
			if err := json.Unmarshal(item.Output, &parts); err != nil {
				return transcriptEntry{}, false, err
			}
			var texts []string
			for _, rawPart := range parts {
				var part struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if json.Unmarshal(rawPart, &part) == nil && part.Type == "input_text" {
					texts = append(texts, part.Text)
				}
			}
			output = strings.Join(texts, "\n")
		}
		return transcriptEntry{Kind: "tool_result", CallID: item.CallID, Text: output}, output != "", nil
	default:
		return transcriptEntry{}, false, nil
	}
}

func searchTranscript(sess *session, arguments json.RawMessage, activeCallID string) json.RawMessage {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
		Start int    `json:"start"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil || strings.TrimSpace(args.Query) == "" || len(args.Query) > 256 || args.Limit < 1 || args.Limit > 20 || args.Start < 0 {
		return json.RawMessage(`{"error":"history requires a nonempty query of at most 256 bytes, a limit from 1 to 20, and a nonnegative start"}`)
	}

	type match struct {
		Index int `json:"index"`
		transcriptEntry
	}
	result := struct {
		Matches []match `json:"matches"`
		Total   int     `json:"total"`
		Next    *int    `json:"next,omitempty"`
	}{Matches: []match{}}
	needle := strings.ToLower(args.Query)
	index := 0
	lastMatch := -1
	add := func(entry transcriptEntry) {
		if activeCallID != "" && entry.Kind == "tool_call" && entry.CallID == activeCallID {
			return
		}
		position := strings.Index(strings.ToLower(entry.Text), needle)
		if position >= 0 {
			result.Total++
			if index >= args.Start && len(result.Matches) < args.Limit {
				entry.Text = transcriptExcerpt(entry.Text, position)
				result.Matches = append(result.Matches, match{Index: index, transcriptEntry: entry})
				lastMatch = index
			} else if index >= args.Start && result.Next == nil {
				next := lastMatch + 1
				result.Next = &next
			}
		}
		index++
	}
	for _, entry := range sess.Transcript {
		add(entry)
	}
	for _, raw := range sess.History[sess.TranscriptSkip:] {
		entry, visible, err := visibleTranscriptEntry(raw)
		if err != nil {
			return json.RawMessage(`{"error":"history contains an invalid item"}`)
		}
		if visible {
			add(entry)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return json.RawMessage(`{"error":"cannot encode history result"}`)
	}
	return encoded
}

func transcriptExcerpt(text string, matchByte int) string {
	runes := []rune(text)
	if len(runes) <= 2048 {
		return text
	}
	if matchByte > len(text) {
		matchByte = len(text)
	}
	position := utf8.RuneCountInString(text[:matchByte])
	start := position - 512
	if start < 0 {
		start = 0
	}
	end := start + 2048
	if end > len(runes) {
		end = len(runes)
		start = end - 2048
	}
	return string(runes[start:end])
}
