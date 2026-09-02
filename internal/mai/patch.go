package mai

import (
	"crypto/rand"
	"encoding/hex"
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
	path            string
	content         string
	originalContent string
	mode            os.FileMode
	originalExists  bool
	deleted         bool
}

type patchPlan struct {
	root     *os.Root
	rootPath string
	files    map[string]*pendingFile
	changed  []string
}

type patchAction struct {
	kind string
	file *pendingFile
}

type patchCommitError struct {
	cause   error
	applied []string
	failed  string
	pending []string
}

func (err *patchCommitError) Error() string {
	return err.cause.Error()
}

func (err *patchCommitError) Unwrap() error {
	return err.cause
}

type patchWriteFunc func(*os.Root, string, []byte, os.FileMode) error
type patchRemoveFunc func(*os.Root, string) error

func applyPatch(repoRoot, patch string) (string, error) {
	return applyPatchWithIO(repoRoot, patch, atomicWriteRootFile, (*os.Root).Remove)
}

func applyPatchWithIO(repoRoot, patch string, write patchWriteFunc, remove patchRemoveFunc) (string, error) {
	resolvedRoot, err := canonicalPath(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root: %w", err)
	}
	root, err := os.OpenRoot(resolvedRoot)
	if err != nil {
		return "", fmt.Errorf("open repository root: %w", err)
	}
	defer root.Close()
	ops, err := parsePatch(patch)
	if err != nil {
		return "", err
	}
	plan := patchPlan{root: root, rootPath: resolvedRoot, files: make(map[string]*pendingFile)}
	for _, op := range ops {
		if err := plan.addOperation(op); err != nil {
			return "", err
		}
	}
	if err := plan.commitWithIO(write, remove); err != nil {
		return "", err
	}

	b, _ := json.Marshal(map[string]any{"ok": true, "changed": plan.changed})
	return string(b), nil
}

func (plan *patchPlan) addOperation(op patchOperation) error {
	switch op.kind {
	case "add":
		return plan.addFile(op)
	case "delete":
		return plan.deleteFile(op.path)
	case "update":
		return plan.updateFile(op)
	default:
		return fmt.Errorf("unsupported patch operation %q", op.kind)
	}
}

func (plan *patchPlan) addFile(op patchOperation) error {
	path, err := securePatchPath(plan.rootPath, op.path)
	if err != nil {
		return err
	}
	if _, ok := plan.files[path]; ok {
		return fmt.Errorf("duplicate patch target %s", op.path)
	}
	if _, err := plan.root.Lstat(path); err == nil {
		return fmt.Errorf("cannot add %s: file already exists", op.path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", op.path, err)
	}
	plan.files[path] = &pendingFile{path: path, content: op.contents, mode: 0o644}
	plan.changed = append(plan.changed, "add "+op.path)
	return nil
}

func (plan *patchPlan) deleteFile(path string) error {
	file, err := plan.loadFile(path)
	if err != nil {
		return err
	}
	file.deleted = true
	plan.changed = append(plan.changed, "delete "+path)
	return nil
}

func (plan *patchPlan) updateFile(op patchOperation) error {
	file, err := plan.loadFile(op.path)
	if err != nil {
		return err
	}
	updated, err := applyChunks(file.content, op.chunks)
	if err != nil {
		return fmt.Errorf("update %s: %w", op.path, err)
	}
	file.content = updated
	if op.movePath == "" {
		plan.changed = append(plan.changed, "update "+op.path)
		return nil
	}
	return plan.moveFile(file, op.path, op.movePath)
}

func (plan *patchPlan) moveFile(file *pendingFile, source, destination string) error {
	dest, err := securePatchPath(plan.rootPath, destination)
	if err != nil {
		return err
	}
	if dest == file.path {
		return fmt.Errorf("move destination equals source for %s", source)
	}
	if _, ok := plan.files[dest]; ok {
		return fmt.Errorf("duplicate patch target %s", destination)
	}
	if _, err := plan.root.Lstat(dest); err == nil {
		return fmt.Errorf("cannot move to %s: file already exists", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect %s: %w", destination, err)
	}
	plan.files[dest] = &pendingFile{path: dest, content: file.content, mode: file.mode}
	file.deleted = true
	plan.changed = append(plan.changed, "move "+source+" -> "+destination)
	return nil
}

func (plan *patchPlan) loadFile(rel string) (*pendingFile, error) {
	path, err := securePatchPath(plan.rootPath, rel)
	if err != nil {
		return nil, err
	}
	if file, ok := plan.files[path]; ok {
		if file.deleted {
			return nil, fmt.Errorf("%s was already deleted in this patch", rel)
		}
		return file, nil
	}
	info, err := plan.root.Lstat(path)
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
	content, err := plan.root.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", rel, err)
	}
	file := &pendingFile{
		path: path, content: string(content), originalContent: string(content),
		mode: info.Mode().Perm(), originalExists: true,
	}
	plan.files[path] = file
	return file, nil
}

func (plan *patchPlan) commit() error {
	return plan.commitWithIO(atomicWriteRootFile, (*os.Root).Remove)
}

func (plan *patchPlan) commitWithIO(write patchWriteFunc, remove patchRemoveFunc) error {
	actions, err := plan.commitActions()
	if err != nil {
		return err
	}
	if err := plan.checkCurrentFiles(); err != nil {
		return err
	}
	applied := make([]string, 0, len(actions))
	for index, action := range actions {
		var actionErr error
		switch action.kind {
		case "write":
			actionErr = write(plan.root, action.file.path, []byte(action.file.content), action.file.mode)
		case "delete":
			actionErr = remove(plan.root, action.file.path)
		}
		if actionErr != nil {
			name := action.String()
			return &patchCommitError{
				cause: fmt.Errorf("%s: %w", name, actionErr), applied: applied, failed: name,
				pending: patchActionNames(actions[index+1:]),
			}
		}
		applied = append(applied, action.String())
	}
	return nil
}

func (plan *patchPlan) commitActions() ([]patchAction, error) {
	var writes, deletes []*pendingFile
	for _, file := range plan.files {
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
	writeSet := make(map[string]bool, len(writes))
	for _, file := range writes {
		writeSet[file.path] = true
	}
	for _, file := range writes {
		for parent := filepath.Dir(file.path); parent != "."; parent = filepath.Dir(parent) {
			if writeSet[parent] {
				return nil, fmt.Errorf("patch writes both file %s and its descendant %s", parent, file.path)
			}
		}
	}
	actions := make([]patchAction, 0, len(writes)+len(deletes))
	for _, file := range writes {
		actions = append(actions, patchAction{kind: "write", file: file})
	}
	for _, file := range deletes {
		actions = append(actions, patchAction{kind: "delete", file: file})
	}
	return actions, nil
}

func (plan *patchPlan) checkCurrentFiles() error {
	paths := make([]string, 0, len(plan.files))
	for path, file := range plan.files {
		if file.originalExists || !file.deleted {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		file := plan.files[path]
		if !file.originalExists {
			if _, err := plan.root.Lstat(path); err == nil {
				return fmt.Errorf("patch target changed before commit: %s now exists", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("inspect %s before commit: %w", path, err)
			}
			continue
		}
		info, err := plan.root.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect %s before commit: %w", path, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != file.mode {
			return fmt.Errorf("patch source changed before commit: %s", path)
		}
		content, err := plan.root.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s before commit: %w", path, err)
		}
		if string(content) != file.originalContent {
			return fmt.Errorf("patch source changed before commit: %s", path)
		}
	}
	return nil
}

func (action patchAction) String() string {
	return action.kind + " " + action.file.path
}

func patchActionNames(actions []patchAction) []string {
	names := make([]string, 0, len(actions))
	for _, action := range actions {
		names = append(names, action.String())
	}
	return names
}

type patchParser struct {
	lines []string
	index int
	end   int
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
	parser := patchParser{lines: lines, index: 1, end: len(lines) - 1}
	var ops []patchOperation
	for parser.index < parser.end {
		line := strings.TrimSpace(lines[parser.index])
		if line == "" {
			parser.index++
			continue
		}
		op, err := parser.parseOperation(line)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
	}
	if len(ops) == 0 {
		return nil, errors.New("patch contains no operations")
	}
	return ops, nil
}

func (parser *patchParser) parseOperation(marker string) (patchOperation, error) {
	switch {
	case strings.HasPrefix(marker, "*** Add File: "):
		return parser.parseAdd(strings.TrimSpace(strings.TrimPrefix(marker, "*** Add File: ")))
	case strings.HasPrefix(marker, "*** Delete File: "):
		parser.index++
		return patchOperation{kind: "delete", path: strings.TrimSpace(strings.TrimPrefix(marker, "*** Delete File: "))}, nil
	case strings.HasPrefix(marker, "*** Update File: "):
		return parser.parseUpdate(strings.TrimSpace(strings.TrimPrefix(marker, "*** Update File: ")))
	default:
		return patchOperation{}, fmt.Errorf("invalid patch marker on line %d: %s", parser.index+1, parser.lines[parser.index])
	}
}

func (parser *patchParser) parseAdd(path string) (patchOperation, error) {
	parser.index++
	var content []string
	for parser.index < parser.end && !isFileMarker(parser.lines[parser.index]) {
		line := parser.lines[parser.index]
		if !strings.HasPrefix(line, "+") {
			return patchOperation{}, fmt.Errorf("add file %s: line %d must start with +", path, parser.index+1)
		}
		content = append(content, strings.TrimPrefix(line, "+"))
		parser.index++
	}
	if len(content) == 0 {
		return patchOperation{}, fmt.Errorf("add file %s has no content", path)
	}
	return patchOperation{kind: "add", path: path, contents: strings.Join(content, "\n") + "\n"}, nil
}

func (parser *patchParser) parseUpdate(path string) (patchOperation, error) {
	op := patchOperation{kind: "update", path: path}
	parser.index++
	if parser.index < parser.end && strings.HasPrefix(strings.TrimSpace(parser.lines[parser.index]), "*** Move to: ") {
		op.movePath = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(parser.lines[parser.index]), "*** Move to: "))
		parser.index++
	}
	segment := parser.readSegment()
	chunks, err := parseChunks(segment)
	if err != nil {
		return patchOperation{}, fmt.Errorf("update file %s: %w", op.path, err)
	}
	if len(chunks) == 0 && op.movePath == "" {
		return patchOperation{}, fmt.Errorf("update file %s is empty", op.path)
	}
	op.chunks = chunks
	return op, nil
}

func (parser *patchParser) readSegment() []string {
	var segment []string
	for parser.index < parser.end && !isFileMarker(parser.lines[parser.index]) {
		segment = append(segment, parser.lines[parser.index])
		parser.index++
	}
	return segment
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
	return clean, nil
}

func atomicWriteRootFile(root *os.Root, path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	var tmp *os.File
	var tmpPath string
	for range 100 {
		var suffix [12]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return fmt.Errorf("create temporary name for %s: %w", path, err)
		}
		tmpPath = filepath.Join(dir, ".mai-"+hex.EncodeToString(suffix[:]))
		var err error
		tmp, err = root.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("create temporary file for %s: %w", path, err)
		}
	}
	if tmp == nil {
		return fmt.Errorf("create temporary file for %s: too many name conflicts", path)
	}
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = root.Remove(tmpPath)
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
	if err := root.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	keep = true
	return nil
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mai-*")
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
