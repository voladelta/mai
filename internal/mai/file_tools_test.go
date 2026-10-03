package mai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q", path, data, want)
	}
}

func fileTestAgent(t *testing.T) (*agent, *session) {
	t.Helper()
	return newAgent(io.Discard, io.Discard, "", time.Second, false), deepseekTestSession(t)
}

func fileCall(t *testing.T, a *agent, sess *session, name string, args any) map[string]any {
	t.Helper()
	return fileCallContext(t, context.Background(), a, sess, name, args)
}

func fileCallContext(t *testing.T, ctx context.Context, a *agent, sess *session, name string, args any) map[string]any {
	t.Helper()
	raw := a.executeTool(ctx, sess, functionCall{Name: name, Arguments: string(mustJSON(t, args))})
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func requireFileOK(t *testing.T, result map[string]any) {
	t.Helper()
	if result["ok"] != true {
		t.Fatalf("file operation failed: %v", result)
	}
}

func requireFileCode(t *testing.T, result map[string]any, code string) {
	t.Helper()
	if result["ok"] != false || result["code"] != code {
		t.Fatalf("want %s refusal, got %v", code, result)
	}
}

func TestFileToolsCreateEditOverwriteAndRootPaths(t *testing.T) {
	a, sess := fileTestAgent(t)
	sess.CWD = filepath.Join(sess.RepoRoot, "sub")
	if err := os.Mkdir(sess.CWD, 0o755); err != nil {
		t.Fatal(err)
	}
	result := fileCall(t, a, sess, "write", map[string]any{"file_path": "nested/file", "content": "first\n"})
	requireFileOK(t, result)
	if result["operation"] != "create" {
		t.Fatal(result)
	}
	requireFileOK(t, fileCall(t, a, sess, "edit", map[string]any{"file_path": "nested/file", "old_string": "first", "new_string": "second"}))
	assertContent(t, filepath.Join(sess.RepoRoot, "nested/file"), "second\n")
	result = fileCall(t, a, sess, "write", map[string]any{"file_path": "nested/file", "content": ""})
	requireFileOK(t, result)
	if result["operation"] != "update" {
		t.Fatal(result)
	}
	assertContent(t, filepath.Join(sess.RepoRoot, "nested/file"), "")
	if _, err := os.Stat(filepath.Join(sess.CWD, "nested")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong path base: %v", err)
	}
	if output := a.executeTool(context.Background(), sess, functionCall{Name: "apply_patch", Arguments: "{}"}); !strings.Contains(string(output), "unknown tool") {
		t.Fatalf("retired tool executes: %s", output)
	}
}

func TestFileToolsRequireOwnObservationsAndInvalidateStaleVersions(t *testing.T) {
	for _, tool := range []string{"write", "edit"} {
		t.Run(tool, func(t *testing.T) {
			a, sess := fileTestAgent(t)
			path := filepath.Join(sess.RepoRoot, "file")
			mustWrite(t, path, "original")
			args := map[string]any{"file_path": "file", "content": "updated"}
			if tool == "edit" {
				args = map[string]any{"file_path": "file", "old_string": "original", "new_string": "updated"}
			}
			requireFileCode(t, fileCall(t, a, sess, tool, args), "FS_NOT_OBSERVED")
			// A Bash read is evidence for the model, not an fs observation.
			a.executeTool(context.Background(), sess, functionCall{Name: "bash", Arguments: `{"command":"cat file"}`})
			requireFileCode(t, fileCall(t, a, sess, tool, args), "FS_NOT_OBSERVED")
			requireFileOK(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "file", "limit": 1}))
			// Even equal-sized changes with restored mtime must fail the digest check.
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			mustWrite(t, path, "external")
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			requireFileCode(t, fileCall(t, a, sess, tool, args), "FS_STALE_VERSION")
			assertContent(t, path, "external")
			mustWrite(t, path, "original")
			requireFileOK(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "file"}))
			requireFileOK(t, fileCall(t, a, sess, tool, args))
			assertContent(t, path, "updated")
		})
	}
}

func TestFileToolsDeletedFileNeedsFreshObservation(t *testing.T) {
	a, sess := fileTestAgent(t)
	path := filepath.Join(sess.RepoRoot, "file")
	requireFileOK(t, fileCall(t, a, sess, "write", map[string]any{"file_path": "file", "content": "before"}))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	requireFileCode(t, fileCall(t, a, sess, "write", map[string]any{"file_path": "file", "content": "after"}), "FS_STALE_VERSION")
	requireFileCode(t, fileCall(t, a, sess, "edit", map[string]any{"file_path": "file", "old_string": "before", "new_string": "after"}), "FS_STALE_VERSION")
	requireFileCode(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "file"}), "FS_NOT_FOUND")
	requireFileOK(t, fileCall(t, a, sess, "write", map[string]any{"file_path": "file", "content": "after"}))
	assertContent(t, path, "after")
}

func TestFileToolsLiteralMatchingAndLineEndings(t *testing.T) {
	cases := []struct {
		name, content, old, replacement, want, code string
		all                                         bool
	}{
		{"ambiguous", "a\na\n", "a", "b", "a\na\n", "FS_AMBIGUOUS_EDIT", false},
		{"missing", "a", "z", "b", "a", "FS_EDIT_NOT_FOUND", false},
		{"whitespace", "\ta\n", " a", "b", "\ta\n", "FS_EDIT_NOT_FOUND", false},
		{"all", "a\na\n", "a", "b", "b\nb\n", "", true},
		{"context", "func a() { return 30 }\nfunc b() { return 30 }\n", "func b() { return 30 }", "func b() { return 45 }", "func a() { return 30 }\nfunc b() { return 45 }\n", "", false},
		{"delete", "a\nb", "a\n", "", "b", "", false},
		{"crlf", "a\r\nb\r\n", "a\nb", "c\nd", "c\r\nd\r\n", "", false},
		{"lf", "a\nb", "a\r\nb", "c\r\nd", "c\nd", "", false},
		{"unicode", "café 猫", "猫", "犬", "café 犬", "", false},
		{"no-final-newline", "a", "a", "b", "b", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, sess := fileTestAgent(t)
			path := filepath.Join(sess.RepoRoot, "file")
			mustWrite(t, path, tc.content)
			requireFileOK(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "file"}))
			result := fileCall(t, a, sess, "edit", map[string]any{"file_path": "file", "old_string": tc.old, "new_string": tc.replacement, "replace_all": tc.all})
			if tc.code == "" {
				requireFileOK(t, result)
			} else {
				requireFileCode(t, result, tc.code)
			}
			assertContent(t, path, tc.want)
		})
	}
}

func TestFileToolsReadWindowsCapsAndEmptyFiles(t *testing.T) {
	a, sess := fileTestAgent(t)
	path := filepath.Join(sess.RepoRoot, "file")
	for _, tc := range []struct {
		text          string
		offset, limit int
		want          string
		total         int
		truncated     bool
	}{
		{"a\nb\nc\n", 2, 1, "2: b\n", 3, true},
		{"a\nb\nc", 3, 1, "3: c\n", 3, false},
		{"", 1, 1, "", 0, false},
		{"\n", 1, 1, "1: \n", 1, false},
		{"a\r\nb\r\n", 1, 2, "1: a\n2: b\n", 2, false},
		{"a", 100, 1, "", 1, false},
	} {
		mustWrite(t, path, tc.text)
		result := fileCall(t, a, sess, "read", map[string]any{"file_path": "file", "offset": tc.offset, "limit": tc.limit})
		requireFileOK(t, result)
		if result["content"] != tc.want || result["total_lines"] != float64(tc.total) || result["truncated"] != tc.truncated {
			t.Fatal(result)
		}
	}
	mustWrite(t, path, strings.Repeat("猫", maxReadOutputBytes))
	result := fileCall(t, a, sess, "read", map[string]any{"file_path": "file"})
	requireFileOK(t, result)
	if len(result["content"].(string)) > maxReadOutputBytes || result["truncated"] != true || result["next_offset"] != float64(1) {
		t.Fatal("unbounded or invalid long-line result", result["truncated"])
	}
	// A bounded window still observes the complete file version.
	requireFileOK(t, fileCall(t, a, sess, "edit", map[string]any{"file_path": "file", "old_string": "猫", "new_string": "犬", "replace_all": true}))
}

func TestFileToolsRejectMalformedArgumentsWithoutEffects(t *testing.T) {
	a, sess := fileTestAgent(t)
	for _, tc := range []struct{ tool, args string }{
		{"write", `{"file_path":"file"}`}, {"write", `{"file_path":"file","content":null}`},
		{"write", `{"file_path":"file","content":"x","extra":true}`}, {"write", `{"file_path":"file","content":"x"} {}`},
		{"write", `null`}, {"write", `[]`}, {"write", `{"file_path":" ","content":"x"}`},
		{"edit", `{"file_path":"file","old_string":"","new_string":"x"}`},
		{"edit", `{"file_path":"file","old_string":"x","new_string":null}`},
		{"edit", `{"file_path":"file","old_string":"x","new_string":"x"}`},
		{"edit", `{"file_path":"file","old_string":"x","new_string":"y","replace_all":"true"}`},
		{"read", `{"file_path":"file","offset":0}`}, {"read", `{"file_path":"file","limit":2001}`},
		{"read", `{"file_path":"file","offset":null}`},
		{"edit", `{"file_path":"file","old_string":"x","new_string":"y","replace_all":null}`},
	} {
		output := a.executeTool(context.Background(), sess, functionCall{Name: tc.tool, Arguments: tc.args})
		if !strings.Contains(string(output), `\"ok\":false`) {
			t.Fatalf("accepted %s %s: %s", tc.tool, tc.args, output)
		}
	}
	if _, err := os.Stat(filepath.Join(sess.RepoRoot, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid arguments caused effects: %v", err)
	}
}

func TestFileToolsBoundariesAndPermissions(t *testing.T) {
	a, sess := fileTestAgent(t)
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "keep"), "keep")
	if err := os.Symlink(outside, filepath.Join(sess.RepoRoot, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "keep"), filepath.Join(sess.RepoRoot, "link")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(sess.RepoRoot, "inside"), "inside")
	if err := os.Symlink("inside", filepath.Join(sess.RepoRoot, "internal-link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(sess.RepoRoot, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../keep", filepath.Join(outside, "keep"), "escape/keep", "escape/new/deep/file", "link", "internal-link", "fifo", ".", "a\x00b"} {
		for _, tool := range []string{"read", "write", "edit"} {
			args := map[string]any{"file_path": path}
			if tool == "write" {
				args["content"] = "bad"
			}
			if tool == "edit" {
				args["old_string"], args["new_string"] = "keep", "bad"
			}
			if result := fileCall(t, a, sess, tool, args); result["ok"] != false {
				t.Fatalf("accepted %s %s: %v", tool, path, result)
			}
		}
	}
	assertContent(t, filepath.Join(outside, "keep"), "keep")
	assertContent(t, filepath.Join(sess.RepoRoot, "inside"), "inside")
	if _, err := os.Stat(filepath.Join(outside, "new")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("escaped creation: %v", err)
	}
	path := filepath.Join(sess.RepoRoot, "executable")
	mustWrite(t, path, "old")
	if err := os.Chmod(path, 0o751); err != nil {
		t.Fatal(err)
	}
	requireFileOK(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "executable"}))
	requireFileOK(t, fileCall(t, a, sess, "edit", map[string]any{"file_path": "executable", "old_string": "old", "new_string": "new"}))
	requireFileOK(t, fileCall(t, a, sess, "write", map[string]any{"file_path": "executable", "content": "whole"}))
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o751 {
		t.Fatalf("lost permissions: %v %v", info, err)
	}
}

func TestFileToolsAliasesShareObservationsAndLocks(t *testing.T) {
	a, sess := fileTestAgent(t)
	mustWrite(t, filepath.Join(sess.RepoRoot, "real/file"), "old")
	if err := os.Symlink("real", filepath.Join(sess.RepoRoot, "alias")); err != nil {
		t.Fatal(err)
	}
	requireFileOK(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "alias/file"}))
	requireFileOK(t, fileCall(t, a, sess, "edit", map[string]any{"file_path": "real/./file", "old_string": "old", "new_string": "new"}))
	assertContent(t, filepath.Join(sess.RepoRoot, "real/file"), "new")
	// Two independent sessions read the same version through different aliases.
	other := *sess
	other.ID = "other"
	requireFileOK(t, fileCall(t, a, &other, "read", map[string]any{"file_path": "real/file"}))
	var wg sync.WaitGroup
	results := make(chan map[string]any, 2)
	for i, owner := range []*session{sess, &other} {
		wg.Add(1)
		go func(i int, owner *session) {
			defer wg.Done()
			path := "real/file"
			if i == 1 {
				path = "alias/file"
			}
			results <- fileCall(t, a, owner, "edit", map[string]any{"file_path": path, "old_string": "new", "new_string": "winner"})
		}(i, owner)
	}
	wg.Wait()
	close(results)
	ok, stale := 0, 0
	for result := range results {
		if result["ok"] == true {
			ok++
		} else if result["code"] == "FS_STALE_VERSION" {
			stale++
		} else {
			t.Fatal(result)
		}
	}
	if ok != 1 || stale != 1 {
		t.Fatalf("concurrent mutation: successes=%d stale=%d", ok, stale)
	}
	assertContent(t, filepath.Join(sess.RepoRoot, "real/file"), "winner")
}

func TestFileToolsConcurrentCreateDoesNotOverwrite(t *testing.T) {
	a, sess := fileTestAgent(t)
	other := *sess
	var wg sync.WaitGroup
	results := make(chan map[string]any, 2)
	for i, owner := range []*session{sess, &other} {
		wg.Add(1)
		go func(i int, owner *session) {
			defer wg.Done()
			results <- fileCall(t, a, owner, "write", map[string]any{"file_path": "new", "content": strings.Repeat(string(rune('a'+i)), 1024)})
		}(i, owner)
	}
	wg.Wait()
	close(results)
	ok, refused := 0, 0
	for result := range results {
		if result["ok"] == true {
			ok++
		} else if result["code"] == "FS_NOT_OBSERVED" {
			refused++
		} else {
			t.Fatal(result)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("create: %d success, %d refusal", ok, refused)
	}
	data, err := os.ReadFile(filepath.Join(sess.RepoRoot, "new"))
	if err != nil || (string(data) != strings.Repeat("a", 1024) && string(data) != strings.Repeat("b", 1024)) {
		t.Fatal("partial or unexpected publication", err)
	}
}

func TestFileToolsResumeDoesNotReuseObservations(t *testing.T) {
	a, sess := fileTestAgent(t)
	requireFileOK(t, fileCall(t, a, sess, "write", map[string]any{"file_path": "file", "content": "old"}))
	var resumed session
	if err := json.Unmarshal(mustJSON(t, sess), &resumed); err != nil {
		t.Fatal(err)
	}
	fresh := newAgent(io.Discard, io.Discard, "", time.Second, false)
	requireFileCode(t, fileCall(t, fresh, &resumed, "edit", map[string]any{"file_path": "file", "old_string": "old", "new_string": "new"}), "FS_NOT_OBSERVED")
	requireFileOK(t, fileCall(t, fresh, &resumed, "read", map[string]any{"file_path": "file"}))
	requireFileOK(t, fileCall(t, fresh, &resumed, "edit", map[string]any{"file_path": "file", "old_string": "old", "new_string": "new"}))
}

func TestSidekickSharesFileLocksButRequiresOwnRead(t *testing.T) {
	a, sess := fileTestAgent(t)
	a.backend = &responsesClient{}
	requireFileOK(t, fileCall(t, a, sess, "write", map[string]any{"file_path": "file", "content": "old"}))
	worker, err := a.newSidekick(sess)
	if err != nil {
		t.Fatal(err)
	}
	if a.fileTools() != worker.agent.fileTools() {
		t.Fatal("sidekick has independent mutation locks")
	}
	args := map[string]any{"file_path": "file", "old_string": "old", "new_string": "worker"}
	requireFileCode(t, fileCall(t, worker.agent, worker.session, "edit", args), "FS_NOT_OBSERVED")
	requireFileOK(t, fileCall(t, worker.agent, worker.session, "read", map[string]any{"file_path": "file"}))
	requireFileOK(t, fileCall(t, worker.agent, worker.session, "edit", args))
	requireFileCode(t, fileCall(t, a, sess, "write", map[string]any{"file_path": "file", "content": "director"}), "FS_STALE_VERSION")
	assertContent(t, filepath.Join(sess.RepoRoot, "file"), "worker")
}

func TestFileToolsReplacementIdentityAndPermissionChangesAreStale(t *testing.T) {
	for _, change := range []string{"replace", "chmod"} {
		t.Run(change, func(t *testing.T) {
			a, sess := fileTestAgent(t)
			path := filepath.Join(sess.RepoRoot, "file")
			mustWrite(t, path, "old")
			requireFileOK(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "file"}))
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if change == "replace" {
				other := filepath.Join(sess.RepoRoot, "replacement")
				mustWrite(t, other, "old")
				if err := os.Chtimes(other, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(other, path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Chmod(path, 0o600); err != nil {
				t.Fatal(err)
			}
			requireFileCode(t, fileCall(t, a, sess, "edit", map[string]any{"file_path": "file", "old_string": "old", "new_string": "new"}), "FS_STALE_VERSION")
			assertContent(t, path, "old")
		})
	}
}

func TestFileToolsRejectBinaryAndOversizedContent(t *testing.T) {
	a, sess := fileTestAgent(t)
	path := filepath.Join(sess.RepoRoot, "file")
	for _, content := range []string{"a\x00b", "\xff"} {
		mustWrite(t, path, content)
		requireFileCode(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "file"}), "FS_NOT_TEXT")
		assertContent(t, path, content)
	}
	text := strings.Repeat("a", maxTextFileBytes)
	mustWrite(t, path, text)
	requireFileOK(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "file", "limit": 1}))
	requireFileCode(t, fileCall(t, a, sess, "edit", map[string]any{"file_path": "file", "old_string": "a", "new_string": "aa", "replace_all": true}), "FS_TOO_LARGE")
	requireFileCode(t, fileCall(t, a, sess, "write", map[string]any{"file_path": "file", "content": text + "a"}), "FS_TOO_LARGE")
	assertContent(t, path, text)
	mustWrite(t, path, text+"a")
	requireFileCode(t, fileCall(t, a, sess, "read", map[string]any{"file_path": "file"}), "FS_TOO_LARGE")
}

func TestFileToolsCancelledMutationsDoNotPublish(t *testing.T) {
	a, sess := fileTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requireFileCode(t, fileCallContext(t, ctx, a, sess, "write", map[string]any{"file_path": "file", "content": "bad"}), "FS_CANCELLED")
	if _, err := os.Stat(filepath.Join(sess.RepoRoot, "file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled write published: %v", err)
	}
}

func TestRootPublicationRefusesRacedCreationAndCleansStaging(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = publishRootFile(root, "file", []byte("ours"), 0o644, true, func() error { mustWrite(t, filepath.Join(dir, "file"), "external"); return nil })
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("raced creation clobbered target: %v", err)
	}
	assertContent(t, filepath.Join(dir, "file"), "external")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("staging residue: %v %v", entries, err)
	}
	refusal := errors.New("stale")
	err = publishRootFile(root, "file", []byte("ours"), 0o644, false, func() error { return refusal })
	if !errors.Is(err, refusal) {
		t.Fatal(err)
	}
	assertContent(t, filepath.Join(dir, "file"), "external")
}

func TestRootPublicationCannotFollowReplacedParentOutside(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	mustWrite(t, filepath.Join(dir, "sub/file"), "original")
	mustWrite(t, filepath.Join(outside, "file"), "keep")
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = publishRootFile(root, "sub/file", []byte("bad"), 0o644, false, func() error {
		if err := os.Rename(filepath.Join(dir, "sub"), filepath.Join(dir, "old")); err != nil {
			t.Fatal(err)
		}
		return os.Symlink(outside, filepath.Join(dir, "sub"))
	})
	if err == nil {
		t.Fatal("publication followed escaped parent")
	}
	assertContent(t, filepath.Join(outside, "file"), "keep")
	assertContent(t, filepath.Join(dir, "old/file"), "original")
}

func TestFileMutationRecoveryPreservesUnknownOutcomes(t *testing.T) {
	for _, name := range []string{"write", "edit", "apply_patch"} {
		sess := &session{History: []json.RawMessage{mustJSON(t, functionCall{Type: "function_call", CallID: "pending", Name: name, Arguments: `{}`})}}
		if err := repairInterruptedToolCalls(sess); err != nil {
			t.Fatal(err)
		}
		var item struct {
			Output string `json:"output"`
		}
		if err := json.Unmarshal(sess.History[1], &item); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(item.Output), &result); err != nil {
			t.Fatal(err)
		}
		if result["outcome"] != "unknown" || result["ok"] != nil || !strings.Contains(result["instruction"].(string), "reconcile") {
			t.Fatalf("unsafe %s recovery: %v", name, result)
		}
		if err := repairInterruptedToolCalls(sess); err != nil || len(sess.History) != 2 {
			t.Fatalf("recovery replayed %s: %v", name, err)
		}
	}
}

func TestFileToolsResponsesLoopRecoversStaleEdit(t *testing.T) {
	a, sess := fileTestAgent(t)
	a.skipSkills = true
	mustWrite(t, filepath.Join(sess.RepoRoot, "file"), "original\n")
	step := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		found := map[string]bool{}
		for _, tool := range body.Tools {
			found[tool.Name] = true
		}
		if !found["read"] || !found["write"] || !found["edit"] || found["apply_patch"] {
			t.Error("incorrect tool catalog", found)
		}
		step++
		name := "read"
		args := map[string]any{"file_path": "file"}
		switch step {
		case 1:
		case 2:
			mustWrite(t, filepath.Join(sess.RepoRoot, "file"), "external\n")
			name = "edit"
			args["old_string"], args["new_string"] = "original", "ours"
		case 3:
			if !strings.Contains(string(body.Input), "FS_STALE_VERSION") {
				t.Error("model did not receive stale refusal")
			}
		case 4:
			if !strings.Contains(string(body.Input), "1: external") {
				t.Error("model did not receive fresh read")
			}
			name = "edit"
			args["old_string"], args["new_string"] = "external", "ours"
		case 5:
			name = "write"
			args["file_path"], args["content"] = "result", "verified\n"
		case 6:
			deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"DONE"}]}]`)
			return
		default:
			t.Error("unexpected request", step)
			w.WriteHeader(500)
			return
		}
		call := functionCall{Type: "function_call", CallID: name + string(rune('0'+step)), Name: name, Arguments: string(mustJSON(t, args))}
		deepseekTestResponse(w, string(mustJSON(t, []functionCall{call})))
	}))
	defer server.Close()
	a.backend = &responsesClient{profile: profileDeepSeek, models: defaultProviderConfig().Models, httpClient: server.Client(), endpoint: server.URL, stdout: io.Discard}
	if err := a.run(context.Background(), sess, "Read, edit and verify the file."); err != nil {
		t.Fatal(err)
	}
	if step != 6 {
		t.Fatalf("requests = %d", step)
	}
	assertContent(t, filepath.Join(sess.RepoRoot, "file"), "ours\n")
	assertContent(t, filepath.Join(sess.RepoRoot, "result"), "verified\n")
}
