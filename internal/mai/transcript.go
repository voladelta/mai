package mai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"
)

func transcriptPath(sessionPath string) string {
	return strings.TrimSuffix(sessionPath, ".json") + ".transcript.jsonl"
}

func openTranscript(path string, flags int) (*os.File, error) {
	// Nonblocking open lets validation reject a FIFO without waiting for a writer.
	file, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = errors.New("transcript is not a regular file")
	}
	if err == nil && flags&os.O_RDWR != 0 {
		err = file.Chmod(0o600)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func appendTranscript(path string, committedEnd int64, entries []transcriptEntry) (int64, error) {
	file, err := openTranscript(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() < committedEnd {
		return 0, errors.New("transcript is shorter than its committed position")
	}
	// A prior append may have reached disk before the session checkpoint.
	if err := file.Truncate(committedEnd); err != nil {
		return 0, err
	}
	if _, err := file.Seek(committedEnd, io.SeekStart); err != nil {
		return 0, err
	}
	encoder := json.NewEncoder(file)
	for _, entry := range entries {
		if err := encoder.Encode(entry); err != nil {
			return 0, err
		}
	}
	if err := file.Sync(); err != nil {
		return 0, err
	}
	end, err := file.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	return end, nil
}

// A transcript entry contains only text that was visible to the model.
// Reasoning, compaction, and configuration items never enter this archive.
type transcriptEntry struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	Name   string `json:"name,omitempty"`
	CallID string `json:"call_id,omitempty"`
}

func visibleHistoryEntries(history []json.RawMessage) ([]transcriptEntry, error) {
	var entries []transcriptEntry
	for _, raw := range history {
		entry, visible, err := visibleTranscriptEntry(raw)
		if err != nil {
			return nil, err
		}
		if visible {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func archiveTranscript(sessionPath string, current, next *session) error {
	live, err := visibleHistoryEntries(current.History[current.TranscriptSkip:])
	if err != nil {
		return fmt.Errorf("archive history for transcript: %w", err)
	}

	archived := make([]transcriptEntry, 0, len(current.Transcript)+len(live))
	if current.TranscriptEnd == 0 {
		archived = append(archived, current.Transcript...)
	}
	archived = append(archived, live...)

	if sessionPath == "" {
		next.Transcript = archived
	} else if len(archived) > 0 || current.TranscriptEnd > 0 {
		path := transcriptPath(sessionPath)
		end, err := appendTranscript(path, current.TranscriptEnd, archived)
		if err != nil {
			return fmt.Errorf("archive conversation transcript: %w", err)
		}
		next.Transcript = nil
		next.TranscriptEnd = end
		next.transcriptPath = path
	}
	next.TranscriptSkip = len(next.History)
	return nil
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
		content, err := transcriptText(item.Content, true)
		if err != nil {
			return transcriptEntry{}, false, err
		}
		return transcriptEntry{Kind: item.Role, Text: content}, content != "", nil
	case item.Type == "function_call":
		return transcriptEntry{Kind: "tool_call", Name: item.Name, CallID: item.CallID, Text: item.Arguments}, true, nil
	case item.Type == "function_call_output":
		output, err := transcriptText(item.Output, false)
		if err != nil {
			return transcriptEntry{}, false, err
		}
		return transcriptEntry{Kind: "tool_result", CallID: item.CallID, Text: output}, output != "", nil
	default:
		return transcriptEntry{}, false, nil
	}
}

func assistantResponseText(items []json.RawMessage, separator string) (string, error) {
	var texts []string
	for _, raw := range items {
		entry, visible, err := visibleTranscriptEntry(raw)
		if err != nil {
			return "", err
		}
		if visible && entry.Kind == "assistant" {
			texts = append(texts, entry.Text)
		}
	}
	return strings.Join(texts, separator), nil
}

func transcriptText(raw json.RawMessage, includeOutputText bool) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return "", err
		}
		return text, nil
	}
	if raw[0] != '[' {
		return "", nil
	}

	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", err
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
		if part.Type == "input_text" || (includeOutputText && part.Type == "output_text") {
			texts = append(texts, part.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
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
	if sess.TranscriptEnd > 0 {
		file, err := openTranscript(sess.transcriptPath, os.O_RDONLY)
		if err != nil {
			return json.RawMessage(`{"error":"cannot read saved transcript"}`)
		}
		defer file.Close()
		decoder := json.NewDecoder(io.LimitReader(file, sess.TranscriptEnd))
		for {
			var entry transcriptEntry
			err := decoder.Decode(&entry)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return json.RawMessage(`{"error":"saved transcript contains an invalid entry"}`)
			}
			add(entry)
		}
	}
	live, err := visibleHistoryEntries(sess.History[sess.TranscriptSkip:])
	if err != nil {
		return json.RawMessage(`{"error":"history contains an invalid item"}`)
	}
	for _, entry := range live {
		add(entry)
	}
	encoded, _ := json.Marshal(result)
	return encoded
}

func transcriptExcerpt(text string, matchByte int) string {
	runes := []rune(text)
	if len(runes) <= 2048 {
		return text
	}
	matchByte = min(matchByte, len(text))
	position := utf8.RuneCountInString(text[:matchByte])
	start := max(0, position-512)
	end := start + 2048
	if end > len(runes) {
		end = len(runes)
		start = end - 2048
	}
	return string(runes[start:end])
}
