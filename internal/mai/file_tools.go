package mai

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

const (
	maxTextFileBytes   = 16 << 20
	maxReadOutputBytes = 51200
	maxReadLineChars   = 2000
	defaultReadLines   = 2000
)

// Observations belong to an agent session within a run, never to saved history.
// All agents in the run share this owner so aliases share mutation locks.
type fileTools struct {
	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	observed map[*session]map[string]fileVersion
}

type fileVersion struct {
	info   os.FileInfo
	digest [32]byte
	absent bool // a read confirmed the path does not exist
}

func (v fileVersion) equal(other fileVersion) bool {
	if v.absent || other.absent {
		return v.absent == other.absent
	}
	return os.SameFile(v.info, other.info) && v.info.ModTime().Equal(other.info.ModTime()) &&
		v.info.Mode() == other.info.Mode() && v.info.Size() == other.info.Size() && v.digest == other.digest
}

type fileToolError struct{ code, message string }

func (e *fileToolError) Error() string { return e.message }

func fsError(code, message string) error { return &fileToolError{code, message} }

func (a *agent) fileTools() *fileTools {
	a.filesOnce.Do(func() {
		if a.files == nil {
			a.files = &fileTools{locks: make(map[string]*sync.Mutex), observed: make(map[*session]map[string]fileVersion)}
		}
	})
	return a.files
}

func (f *fileTools) lock(key string) func() {
	f.mu.Lock()
	l := f.locks[key]
	if l == nil {
		l = &sync.Mutex{}
		f.locks[key] = l
	}
	f.mu.Unlock()
	l.Lock()
	return l.Unlock
}

func (f *fileTools) observation(sess *session, key string) (fileVersion, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.observed[sess][key]
	return v, ok
}

func (f *fileTools) observe(sess *session, key string, v fileVersion) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.observed[sess] == nil {
		f.observed[sess] = make(map[string]fileVersion)
	}
	f.observed[sess][key] = v
}

func (f *fileTools) forget(sess *session, key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.observed[sess], key)
}

func decodeFileArgs(arguments string, out any) error {
	if !utf8.ValidString(arguments) {
		return errors.New("arguments must be valid UTF-8 JSON")
	}
	// Optional fields may be omitted, but explicit null is outside these schemas.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(arguments), &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("expected one JSON object")
	}
	for name, value := range fields {
		if string(value) == "null" {
			return fmt.Errorf("%s must not be null", name)
		}
	}
	d := json.NewDecoder(strings.NewReader(arguments))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected one JSON object")
	}
	return nil
}

type fileRequest struct {
	path, content, old, replacement string
	offset, limit                   int
	replaceAll                      bool
}

func parseFileRequest(name, arguments string) (fileRequest, error) {
	r := fileRequest{offset: 1, limit: defaultReadLines}
	switch name {
	case "read":
		var args struct {
			Path   string `json:"file_path"`
			Offset *int   `json:"offset"`
			Limit  *int   `json:"limit"`
		}
		if err := decodeFileArgs(arguments, &args); err != nil {
			return r, err
		}
		r.path = args.Path
		if args.Offset != nil {
			r.offset = *args.Offset
		}
		if args.Limit != nil {
			r.limit = *args.Limit
		}
		if r.offset < 1 {
			return r, errors.New("offset must be a positive integer")
		}
		if r.limit < 1 {
			return r, errors.New("limit must be a positive integer")
		}
		if r.limit > defaultReadLines {
			return r, fmt.Errorf("limit must be less than or equal to %d", defaultReadLines)
		}
	case "write":
		var args struct {
			Path    string  `json:"file_path"`
			Content *string `json:"content"`
		}
		if err := decodeFileArgs(arguments, &args); err != nil {
			return r, err
		}
		if args.Content == nil {
			return r, errors.New("content must be a string")
		}
		r.path, r.content = args.Path, *args.Content
		if !textContent(r.content) {
			return r, errors.New("content must be UTF-8 text without NUL bytes")
		}
		if len(r.content) > maxTextFileBytes {
			return r, fsError("FS_TOO_LARGE", "content exceeds the 16 MiB file limit")
		}
	case "edit":
		var args struct {
			Path string  `json:"file_path"`
			Old  *string `json:"old_string"`
			New  *string `json:"new_string"`
			All  bool    `json:"replace_all"`
		}
		if err := decodeFileArgs(arguments, &args); err != nil {
			return r, err
		}
		if args.Old == nil || *args.Old == "" {
			return r, errors.New("old_string must be a non-empty string")
		}
		if args.New == nil {
			return r, errors.New("new_string must be a string")
		}
		r.path, r.old, r.replacement, r.replaceAll = args.Path, *args.Old, *args.New, args.All
		if !textContent(r.old) || !textContent(r.replacement) {
			return r, errors.New("edit arguments must be UTF-8 text without NUL bytes")
		}
		if len(r.old) > maxTextFileBytes || len(r.replacement) > maxTextFileBytes {
			return r, fsError("FS_TOO_LARGE", "edit arguments exceed the 16 MiB file limit")
		}
		if normalizeText(r.old) == normalizeText(r.replacement) {
			return r, errors.New("old_string and new_string must differ")
		}
	default:
		return r, errors.New("unknown file tool")
	}
	if strings.TrimSpace(r.path) == "" {
		return r, errors.New("file_path must be a non-empty string")
	}
	return r, nil
}

func textContent(text string) bool     { return utf8.ValidString(text) && !strings.ContainsRune(text, 0) }
func normalizeText(text string) string { return strings.ReplaceAll(text, "\r\n", "\n") }

// Read from one descriptor and validate both descriptor freshness and pathname
// identity. Nonblocking open prevents a raced FIFO from hanging the harness.
func readFileSnapshot(root *os.Root, path string) ([]byte, fileVersion, error) {
	var version fileVersion
	info, err := root.Lstat(path)
	if err != nil {
		return nil, version, err
	}
	if !info.Mode().IsRegular() {
		return nil, version, fsError("FS_NOT_REGULAR_FILE", "file_path must name a regular file, not a symlink or special file")
	}
	file, err := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, version, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return nil, version, err
	}
	data, err := readBoundedFile(file, maxTextFileBytes)
	if err != nil {
		if errors.Is(err, errFileTooLarge) {
			err = fsError("FS_TOO_LARGE", "file exceeds the 16 MiB file limit")
		}
		return nil, version, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, version, err
	}
	current, err := root.Lstat(path)
	if err != nil {
		return nil, version, err
	}
	if !os.SameFile(info, before) || !os.SameFile(after, current) || !current.Mode().IsRegular() ||
		!before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() || before.Mode() != after.Mode() ||
		!after.ModTime().Equal(current.ModTime()) || after.Size() != current.Size() || after.Mode() != current.Mode() {
		return nil, version, fsError("FS_STALE_VERSION", "file changed during read")
	}
	if !textContent(string(data)) {
		return nil, version, fsError("FS_NOT_TEXT", "file is not UTF-8 text without NUL bytes")
	}
	return data, fileVersion{info: current, digest: sha256.Sum256(data)}, nil
}

func renderFileRead(path string, data []byte, offset, limit int) map[string]any {
	text := normalizeText(string(data))
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	var out strings.Builder
	next, capped := offset, false
	for next <= len(lines) && next-offset < limit {
		line := lines[next-1]
		if runes := []rune(line); len(runes) > maxReadLineChars {
			line = string(runes[:maxReadLineChars]) + fmt.Sprintf("... (line truncated to %d chars)", maxReadLineChars)
		}
		entry := fmt.Sprintf("%d: %s\n", next, line)
		if out.Len()+len(entry) > maxReadOutputBytes {
			capped = true
			break
		}
		out.WriteString(entry)
		next++
	}
	result := map[string]any{
		"ok": true, "path": path, "content": out.String(), "total_lines": len(lines),
		"offset": offset, "last_line": next - 1, "truncated": next <= len(lines), "capped": capped,
	}
	if next <= len(lines) {
		result["next_offset"] = next
	}
	return result
}

// renderFileText formats a structured file tool result as the plain-text
// envelope the model sees. Failures are "Error: <message>".
func renderFileText(name string, result map[string]any) string {
	if result["ok"] != true {
		return fmt.Sprintf("Error: %v", result["error"])
	}
	path := fmt.Sprint(result["path"])
	envelope := func(body string) string {
		return fmt.Sprintf("<path>%s</path>\n<type>file</type>\n<content>\n%s\n</content>", path, body)
	}
	switch name {
	case "read":
		total, offset, last := result["total_lines"].(int), result["offset"].(int), result["last_line"].(int)
		// Script callers keep the lenient empty result; the model gets an error.
		if offset > total && !(total == 0 && offset == 1) {
			return fmt.Sprintf("Error: offset %d is out of range for %q (%d lines)", offset, path, total)
		}
		var footer string
		switch {
		case result["capped"] == true:
			footer = fmt.Sprintf("(Output capped. Showing lines %d-%d. Use offset=%d to continue.)", offset, last, last+1)
		case result["truncated"] == true:
			footer = fmt.Sprintf("(Showing lines %d-%d of %d. Use offset=%d to continue.)", offset, last, total, last+1)
		default:
			footer = fmt.Sprintf("(End of file - total %d lines)", total)
		}
		if content := strings.TrimSuffix(result["content"].(string), "\n"); content != "" {
			footer = content + "\n\n" + footer
		}
		return envelope(footer)
	case "write":
		if result["operation"] == "create" {
			return envelope("Created file")
		}
		return envelope("Updated file")
	default:
		if result["replace_all"] == true {
			return fmt.Sprintf("The file %s has been updated. All occurrences were successfully replaced.", path)
		}
		return fmt.Sprintf("The file %s has been updated successfully.", path)
	}
}

// literalEdit matches line-ending-normalized text but splices the original
// bytes, so lines outside each match keep their own endings. Newlines in the
// replacement take the ending of the line where the match starts.
func literalEdit(data []byte, request fileRequest) ([]byte, error) {
	raw := string(data)
	text, old, replacement := normalizeText(raw), normalizeText(request.old), normalizeText(request.replacement)
	count := strings.Count(text, old)
	if count == 0 {
		return nil, fsError("FS_EDIT_NOT_FOUND", "old_string was not found in the file; re-read the file and provide literal text")
	}
	if count > 1 && !request.replaceAll {
		return nil, fsError("FS_AMBIGUOUS_EDIT", fmt.Sprintf("old_string matched %d times; include more context or set replace_all to true", count))
	}
	n := 1
	if request.replaceAll {
		n = count
	}
	size := int64(len(text)) + int64(n)*(int64(len(replacement))-int64(len(old)))
	if size > maxTextFileBytes {
		return nil, fsError("FS_TOO_LARGE", "edited content exceeds the 16 MiB file limit")
	}
	// crlf holds the normalized offset of each newline that was CRLF, so a
	// normalized offset maps to raw by adding the CRs removed before it.
	var crlf []int
	for i := 0; ; {
		j := strings.Index(raw[i:], "\r\n")
		if j < 0 {
			break
		}
		crlf = append(crlf, i+j-len(crlf))
		i += j + 2
	}
	rawOffset := func(offset int) int { return offset + sort.SearchInts(crlf, offset) }
	var out strings.Builder
	copied, from := 0, 0
	for range n {
		at := from + strings.Index(text[from:], old)
		start, end := rawOffset(at), rawOffset(at+len(old))
		ending := "\n"
		if lineEndsCRLF(raw, start) {
			ending = "\r\n"
		}
		out.WriteString(raw[copied:start])
		out.WriteString(strings.ReplaceAll(replacement, "\n", ending))
		copied, from = end, at+len(old)
	}
	out.WriteString(raw[copied:])
	if out.Len() > maxTextFileBytes {
		return nil, fsError("FS_TOO_LARGE", "edited content exceeds the 16 MiB file limit")
	}
	return []byte(out.String()), nil
}

// lineEndsCRLF reports the ending of the line containing offset, or of the
// last line ending before it when that line has none.
func lineEndsCRLF(raw string, offset int) bool {
	if k := strings.IndexByte(raw[offset:], '\n'); k >= 0 {
		return k > 0 && raw[offset+k-1] == '\r'
	}
	k := strings.LastIndexByte(raw[:offset], '\n')
	return k > 0 && raw[k-1] == '\r'
}

func (a *agent) executeFileTool(ctx context.Context, sess *session, name, arguments string) json.RawMessage {
	return textToolOutput(renderFileText(name, a.runFileTool(ctx, sess, name, arguments)))
}

// runFileTool returns the structured result. Lua host calls consume it
// directly; the model sees renderFileText.
func (a *agent) runFileTool(ctx context.Context, sess *session, name, arguments string) map[string]any {
	request, err := parseFileRequest(name, arguments)
	if err != nil {
		var typed *fileToolError
		if !errors.As(err, &typed) {
			err = fsError("FS_INVALID_ARGUMENTS", err.Error())
		}
		return fileToolFailure(name, "", err)
	}
	if err := ctx.Err(); err != nil {
		return fileToolFailure(name, "", err)
	}
	repo, err := canonicalPath(sess.RepoRoot)
	if err != nil {
		return fileToolFailure(name, request.path, err)
	}
	path, err := secureFilePath(repo, request.path)
	if err != nil {
		return fileToolFailure(name, request.path, fsError("FS_INVALID_PATH", err.Error()))
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return fileToolFailure(name, request.path, err)
	}
	defer root.Close()
	f := a.fileTools()
	key := filepath.Join(repo, path)
	unlock := f.lock(key)
	defer unlock()
	if err := ctx.Err(); err != nil {
		return fileToolFailure(name, request.path, err)
	}
	fmt.Fprintf(a.stderr, "→ %s: %s\n", name, oneLine(path, 180))
	data, current, err := readFileSnapshot(root, path)
	if name == "read" {
		if err != nil {
			f.forget(sess, key)
			if errors.Is(err, os.ErrNotExist) {
				f.observe(sess, key, fileVersion{absent: true})
			}
			return fileToolFailure(name, request.path, err)
		}
		if err := ctx.Err(); err != nil {
			return fileToolFailure(name, request.path, err)
		}
		f.observe(sess, key, current)
		return renderFileRead(path, data, request.offset, request.limit)
	}
	create := errors.Is(err, os.ErrNotExist)
	observed, seen := f.observation(sess, key)
	absent := seen && observed.absent
	if absent {
		seen = false // confirmed absence authorizes creation, not editing
	}
	if create && seen {
		return fileToolFailure(name, request.path, fsError("FS_STALE_VERSION", "file was deleted"))
	}
	if err != nil && !(create && name == "write") {
		return fileToolFailure(name, request.path, err)
	}
	if !create {
		if !seen {
			return fileToolFailure(name, request.path, fsError("FS_NOT_OBSERVED", "file has not been read"))
		}
		if !observed.equal(current) {
			return fileToolFailure(name, request.path, fsError("FS_STALE_VERSION", "file changed since it was read"))
		}
	}
	content := []byte(request.content)
	mode := os.FileMode(0o644)
	if !create {
		mode = current.info.Mode().Perm()
	}
	if name == "edit" {
		content, err = literalEdit(data, request)
		if err != nil {
			return fileToolFailure(name, request.path, err)
		}
	}
	beforePublish := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Refuse parent aliases retargeted after initial resolution.
		resolved, err := secureFilePath(repo, request.path)
		if err != nil || resolved != path {
			return fsError("FS_STALE_VERSION", "file path changed")
		}
		if create {
			return nil
		}
		_, latest, err := readFileSnapshot(root, path)
		if err != nil || !current.equal(latest) {
			return fsError("FS_STALE_VERSION", "file changed before publication")
		}
		return nil
	}
	if err := publishRootFile(root, path, content, mode, create, beforePublish); err != nil {
		if create && errors.Is(err, os.ErrExist) {
			err = fsError("FS_NOT_OBSERVED", "file appeared during creation")
		}
		return fileToolFailure(name, request.path, err)
	}
	// Publication has committed. A failed post-write observation must not report
	// a failed mutation or authorize editing a version written by someone else.
	f.forget(sess, key)
	if after, version, err := readFileSnapshot(root, path); err == nil && string(after) == string(content) {
		f.observe(sess, key, version)
	}
	operation := "update"
	if create {
		operation = "create"
	}
	return map[string]any{"ok": true, "path": path, "operation": operation, "replace_all": request.replaceAll}
}

// readRawFile returns a repository file's UTF-8 text unmodified, with the same
// boundary, symlink, size, and text checks as read. The whole file is returned,
// so it counts as an observation for write and edit, which still refuse a file
// that changed after this read.
func (a *agent) readRawFile(ctx context.Context, sess *session, requested string) map[string]any {
	if err := ctx.Err(); err != nil {
		return fileToolFailure("read", requested, err)
	}
	repo, err := canonicalPath(sess.RepoRoot)
	if err != nil {
		return fileToolFailure("read", requested, err)
	}
	path, err := secureFilePath(repo, requested)
	if err != nil {
		return fileToolFailure("read", requested, fsError("FS_INVALID_PATH", err.Error()))
	}
	root, err := os.OpenRoot(repo)
	if err != nil {
		return fileToolFailure("read", requested, err)
	}
	defer root.Close()
	fmt.Fprintf(a.stderr, "→ read_text: %s\n", oneLine(path, 180))
	f := a.fileTools()
	key := filepath.Join(repo, path)
	unlock := f.lock(key)
	defer unlock()
	data, version, err := readFileSnapshot(root, path)
	if err != nil {
		f.forget(sess, key)
		return fileToolFailure("read", requested, err)
	}
	f.observe(sess, key, version)
	return map[string]any{"ok": true, "path": path, "text": string(data)}
}

func fileToolFailure(name, path string, err error) map[string]any {
	code := "FS_IO_ERROR"
	var typed *fileToolError
	if errors.As(err, &typed) {
		code = typed.code
	} else if errors.Is(err, os.ErrNotExist) {
		code = "FS_NOT_FOUND"
	} else if errors.Is(err, os.ErrPermission) {
		code = "FS_PERMISSION_DENIED"
	} else if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = "FS_CANCELLED"
	}
	message := err.Error()
	if path != "" {
		verb := "modify"
		if name == "read" {
			verb = "read"
		}
		target := fmt.Sprintf("cannot %s %q: ", verb, path)
		switch code {
		case "FS_NOT_OBSERVED":
			message = target + "file has not been read — read the file, then retry"
		case "FS_STALE_VERSION":
			message = target + message + " — re-read the file, then retry"
		case "FS_NOT_FOUND":
			message = target + "not found"
		case "FS_NOT_REGULAR_FILE":
			message = target + "not a regular file"
		default:
			message = target + message
		}
	}
	return map[string]any{"ok": false, "code": code, "error": message}
}
