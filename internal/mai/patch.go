package mai

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const maxPatchFileBytes = 16 << 20

type patchOperation struct {
	kind     string
	path     string
	movePath string
	contents string
	chunks   []patchChunk
}

type patchChunk struct {
	anchor    string
	oldLines  []string
	newLines  []string
	endOfFile bool
}

type pendingFile struct {
	path           string
	content        string
	mode           os.FileMode
	originalExists bool
	deleted        bool
	dirty          bool
}

func applyPatch(repoRoot, patch string) (string, error) {
	resolvedRoot, err := canonicalPath(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	repoRoot = resolvedRoot
	ops, err := parsePatch(patch)
	if err != nil {
		return "", err
	}
	files := make(map[string]*pendingFile)
	var changed []string

	load := func(rel string) (*pendingFile, error) {
		abs, err := securePatchPath(repoRoot, rel)
		if err != nil {
			return nil, err
		}
		if file, ok := files[abs]; ok {
			if file.deleted {
				return nil, fmt.Errorf("%s was already deleted in this patch", rel)
			}
			return file, nil
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("refusing to patch symlink %s", rel)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", rel)
		}
		if info.Size() > maxPatchFileBytes {
			return nil, fmt.Errorf("%s exceeds the %d byte patch limit", rel, maxPatchFileBytes)
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", rel, err)
		}
		file := &pendingFile{path: abs, content: string(content), mode: info.Mode().Perm(), originalExists: true}
		files[abs] = file
		return file, nil
	}

	for _, op := range ops {
		switch op.kind {
		case "add":
			abs, err := securePatchPath(repoRoot, op.path)
			if err != nil {
				return "", err
			}
			if _, ok := files[abs]; ok {
				return "", fmt.Errorf("duplicate patch target %s", op.path)
			}
			if _, err := os.Lstat(abs); err == nil {
				return "", fmt.Errorf("cannot add %s: file already exists", op.path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("inspect %s: %w", op.path, err)
			}
			files[abs] = &pendingFile{path: abs, content: op.contents, mode: 0o644, dirty: true}
			changed = append(changed, "add "+op.path)
		case "delete":
			file, err := load(op.path)
			if err != nil {
				return "", err
			}
			file.deleted = true
			file.dirty = true
			changed = append(changed, "delete "+op.path)
		case "update":
			file, err := load(op.path)
			if err != nil {
				return "", err
			}
			updated, err := applyChunks(file.content, op.chunks)
			if err != nil {
				return "", fmt.Errorf("update %s: %w", op.path, err)
			}
			file.content = updated
			file.dirty = true
			if op.movePath == "" {
				changed = append(changed, "update "+op.path)
				continue
			}
			dest, err := securePatchPath(repoRoot, op.movePath)
			if err != nil {
				return "", err
			}
			if dest == file.path {
				return "", fmt.Errorf("move destination equals source for %s", op.path)
			}
			if _, ok := files[dest]; ok {
				return "", fmt.Errorf("duplicate patch target %s", op.movePath)
			}
			if _, err := os.Lstat(dest); err == nil {
				return "", fmt.Errorf("cannot move to %s: file already exists", op.movePath)
			} else if !errors.Is(err, os.ErrNotExist) {
				return "", fmt.Errorf("inspect %s: %w", op.movePath, err)
			}
			files[dest] = &pendingFile{path: dest, content: updated, mode: file.mode, dirty: true}
			file.deleted = true
			changed = append(changed, "move "+op.path+" -> "+op.movePath)
		default:
			return "", fmt.Errorf("unsupported patch operation %q", op.kind)
		}
	}

	var writes, deletes []*pendingFile
	for _, file := range files {
		if !file.dirty {
			continue
		}
		if file.deleted {
			if file.originalExists {
				deletes = append(deletes, file)
			}
			continue
		}
		writes = append(writes, file)
	}
	sort.Slice(writes, func(i, j int) bool { return writes[i].path < writes[j].path })
	sort.Slice(deletes, func(i, j int) bool { return deletes[i].path < deletes[j].path })
	for _, file := range writes {
		if err := atomicWriteFile(file.path, []byte(file.content), file.mode); err != nil {
			return "", err
		}
	}
	for _, file := range deletes {
		if err := os.Remove(file.path); err != nil {
			return "", fmt.Errorf("delete %s: %w", file.path, err)
		}
	}

	b, _ := json.Marshal(map[string]any{"ok": true, "changed": changed})
	return string(b), nil
}

func parsePatch(patch string) ([]patchOperation, error) {
	patch = strings.ReplaceAll(patch, "\r\n", "\n")
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "*** Begin Patch" {
		return nil, errors.New("patch must start with *** Begin Patch")
	}
	if strings.TrimSpace(lines[len(lines)-1]) != "*** End Patch" {
		return nil, errors.New("patch must end with *** End Patch")
	}
	var ops []patchOperation
	for i := 1; i < len(lines)-1; {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			i++
			continue
		}
		switch {
		case strings.HasPrefix(line, "*** Add File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Add File: "))
			i++
			var content []string
			for i < len(lines)-1 && !isFileMarker(lines[i]) {
				if !strings.HasPrefix(lines[i], "+") {
					return nil, fmt.Errorf("add file %s: line %d must start with +", path, i+1)
				}
				content = append(content, strings.TrimPrefix(lines[i], "+"))
				i++
			}
			if len(content) == 0 {
				return nil, fmt.Errorf("add file %s has no content", path)
			}
			ops = append(ops, patchOperation{kind: "add", path: path, contents: strings.Join(content, "\n") + "\n"})
		case strings.HasPrefix(line, "*** Delete File: "):
			path := strings.TrimSpace(strings.TrimPrefix(line, "*** Delete File: "))
			ops = append(ops, patchOperation{kind: "delete", path: path})
			i++
		case strings.HasPrefix(line, "*** Update File: "):
			op := patchOperation{kind: "update", path: strings.TrimSpace(strings.TrimPrefix(line, "*** Update File: "))}
			i++
			if i < len(lines)-1 && strings.HasPrefix(strings.TrimSpace(lines[i]), "*** Move to: ") {
				op.movePath = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i]), "*** Move to: "))
				i++
			}
			var segment []string
			for i < len(lines)-1 && !isFileMarker(lines[i]) {
				segment = append(segment, lines[i])
				i++
			}
			chunks, err := parseChunks(segment)
			if err != nil {
				return nil, fmt.Errorf("update file %s: %w", op.path, err)
			}
			if len(chunks) == 0 && op.movePath == "" {
				return nil, fmt.Errorf("update file %s is empty", op.path)
			}
			op.chunks = chunks
			ops = append(ops, op)
		default:
			return nil, fmt.Errorf("invalid patch marker on line %d: %s", i+1, lines[i])
		}
	}
	if len(ops) == 0 {
		return nil, errors.New("patch contains no operations")
	}
	return ops, nil
}

func isFileMarker(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "*** Add File: ") ||
		strings.HasPrefix(line, "*** Delete File: ") ||
		strings.HasPrefix(line, "*** Update File: ")
}

func parseChunks(lines []string) ([]patchChunk, error) {
	var chunks []patchChunk
	var current *patchChunk
	flush := func() {
		if current != nil {
			chunks = append(chunks, *current)
			current = nil
		}
	}
	for i, line := range lines {
		if line == "@@" || strings.HasPrefix(line, "@@ ") {
			flush()
			current = &patchChunk{anchor: strings.TrimSpace(strings.TrimPrefix(line, "@@"))}
			continue
		}
		if current == nil {
			current = &patchChunk{}
		}
		if line == "*** End of File" {
			current.endOfFile = true
			continue
		}
		if line == "" {
			return nil, fmt.Errorf("line %d has no patch prefix", i+1)
		}
		switch line[0] {
		case ' ':
			current.oldLines = append(current.oldLines, line[1:])
			current.newLines = append(current.newLines, line[1:])
		case '-':
			current.oldLines = append(current.oldLines, line[1:])
		case '+':
			current.newLines = append(current.newLines, line[1:])
		default:
			return nil, fmt.Errorf("line %d has invalid prefix %q", i+1, line[0])
		}
	}
	flush()
	return chunks, nil
}

func applyChunks(content string, chunks []patchChunk) (string, error) {
	lines, trailingNewline := splitFileLines(content)
	cursor := 0
	for index, chunk := range chunks {
		start := cursor
		if chunk.anchor != "" {
			anchorAt := findLine(lines, chunk.anchor, cursor)
			if anchorAt < 0 {
				return "", fmt.Errorf("chunk %d anchor %q not found", index+1, chunk.anchor)
			}
			start = anchorAt + 1
		}
		at := findSequence(lines, chunk.oldLines, start)
		if at < 0 {
			return "", fmt.Errorf("chunk %d context not found", index+1)
		}
		if chunk.endOfFile && at+len(chunk.oldLines) != len(lines) {
			return "", fmt.Errorf("chunk %d does not reach end of file", index+1)
		}
		replacement := append([]string(nil), chunk.newLines...)
		lines = append(lines[:at], append(replacement, lines[at+len(chunk.oldLines):]...)...)
		cursor = at + len(replacement)
	}
	return joinFileLines(lines, trailingNewline), nil
}

func splitFileLines(content string) ([]string, bool) {
	if content == "" {
		return nil, false
	}
	trailing := strings.HasSuffix(content, "\n")
	if trailing {
		content = strings.TrimSuffix(content, "\n")
	}
	return strings.Split(content, "\n"), trailing
}

func joinFileLines(lines []string, trailing bool) string {
	out := strings.Join(lines, "\n")
	if trailing {
		out += "\n"
	}
	return out
}

func findLine(lines []string, want string, start int) int {
	for i := start; i < len(lines); i++ {
		if lines[i] == want {
			return i
		}
	}
	return -1
}

func findSequence(lines, want []string, start int) int {
	if len(want) == 0 {
		if start <= len(lines) {
			return start
		}
		return -1
	}
	for i := start; i+len(want) <= len(lines); i++ {
		match := true
		for j := range want {
			if lines[i+j] != want[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func securePatchPath(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("invalid patch path %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("patch path escapes repository: %s", rel)
	}
	abs := filepath.Join(root, clean)
	if !pathWithin(root, abs) {
		return "", fmt.Errorf("patch path escapes repository: %s", rel)
	}
	parent := filepath.Dir(abs)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			if !pathWithin(root, resolved) {
				return "", fmt.Errorf("patch path resolves outside repository: %s", rel)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve patch path %s: %w", rel, err)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", fmt.Errorf("cannot resolve parent for %s", rel)
		}
		parent = next
	}
	return abs, nil
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mai-patch-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set mode for %s: %w", path, err)
	}
	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	keep = true
	return nil
}
