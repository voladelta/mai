package mai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchAliasesSharePendingContent(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "real/file"), "a\nb\n")
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}

	_, err := applyPatch(root, "*** Begin Patch\n*** Update File: alias/file\n@@\n-a\n+A\n*** Update File: real/file\n@@\n-b\n+B\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}

	assertContent(t, filepath.Join(root, "real/file"), "A\nB\n")

	_, err = applyPatch(root, "*** Begin Patch\n*** Update File: alias/file\n@@\n-A\n+intermediate\n*** Update File: real/file\n@@\n-intermediate\n+final\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}

	assertContent(t, filepath.Join(root, "real/file"), "final\nB\n")
}

func TestPatchAliasesRejectDuplicateAddsBeforeWrites(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}

	_, err := applyPatch(root, "*** Begin Patch\n*** Add File: alias/nested/file\n+first\n*** Add File: real/nested/file\n+second\n*** End Patch")
	if err == nil {
		t.Fatal("accepted duplicate target through alias")
	}

	if _, err := os.Stat(filepath.Join(root, "real/nested/file")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("duplicate add changed disk: %v", err)
	}
}

func TestPatchEOFMatchesSuffix(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "file"), "a\nb\na\n")

	_, err := applyPatch(root, "*** Begin Patch\n*** Update File: file\n@@\n-a\n+last\n*** End of File\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}

	assertContent(t, filepath.Join(root, "file"), "a\nb\nlast\n")
	if _, err := applyChunks("a\nb\na\n", []patchChunk{
		{anchor: "b", oldLines: []string{"a"}},
		{oldLines: []string{"a"}, endOfFile: true},
	}); err == nil {
		t.Fatal("EOF hunk searched behind the cursor")
	}
}

func TestPatchRejectsAmbiguousContextBeforeWriting(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.go")
	original := "func retry() int {\n\treturn 30\n}\n\nfunc startup() int {\n\treturn 30\n}\n"
	mustWrite(t, file, original)

	ambiguous := "*** Begin Patch\n*** Update File: file.go\n@@\n-\treturn 30\n+\treturn 45\n*** End Patch"
	_, err := applyPatch(root, ambiguous)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous patch error = %v", err)
	}
	assertContent(t, file, original)

	anchored := "*** Begin Patch\n*** Update File: file.go\n@@\n func startup() int {\n-\treturn 30\n+\treturn 45\n*** End Patch"
	if _, err := applyPatch(root, anchored); err != nil {
		t.Fatal(err)
	}
	assertContent(t, file, "func retry() int {\n\treturn 30\n}\n\nfunc startup() int {\n\treturn 45\n}\n")
}

func TestApplyChunksPreservesForwardMatchingAndNewlines(t *testing.T) {
	for _, trailing := range []string{"", "\n"} {
		content := "head\na\nb\nanchor\nc\ntail" + trailing
		chunks := []patchChunk{
			{oldLines: []string{"a", "b"}, newLines: []string{"replacement"}},
			{anchor: "anchor", oldLines: []string{"c"}, newLines: []string{"c1", "c2"}},
			{newLines: []string{"insert1"}},
			{newLines: []string{"insert2"}},
			{oldLines: []string{"tail"}, newLines: []string{"end"}, endOfFile: true},
		}

		got, err := applyChunks(content, chunks)
		want := "head\nreplacement\nanchor\nc1\nc2\ninsert1\ninsert2\nend" + trailing
		if err != nil || got != want {
			t.Fatalf("got %q, err=%v; want %q", got, err, want)
		}
	}

	if _, err := applyChunks("a\nb\n", []patchChunk{{oldLines: []string{"a"}, endOfFile: true}}); err == nil {
		t.Fatal("accepted context that does not reach EOF")
	}

	if _, err := applyChunks("a\nb", []patchChunk{
		{oldLines: []string{"a"}, newLines: []string{"inserted"}},
		{anchor: "inserted", newLines: []string{"x"}},
	}); err == nil {
		t.Fatal("searched backwards into inserted content")
	}
}

func BenchmarkApplyChunksManyHunks(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			lines := make([]string, count)
			chunks := make([]patchChunk, count)
			for i := range lines {
				lines[i] = fmt.Sprintf("line %d", i)
				chunks[i] = patchChunk{oldLines: []string{lines[i]}, newLines: []string{"updated"}}
			}
			content := strings.Join(lines, "\n")
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				if _, err := applyChunks(content, chunks); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestApplyChunksIndexedMatchingPreservesContextRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
		chunks  []patchChunk
		want    string
		wantErr string
	}{
		{
			name:    "overlapping matches are ambiguous",
			content: "a\na\na\n",
			chunks:  []patchChunk{{oldLines: []string{"a", "a"}, newLines: []string{"new"}}},
			wantErr: "ambiguous",
		},
		{
			name:    "repeated first line needs full context",
			content: "a\nb\na\nc\na\nd\n",
			chunks:  []patchChunk{{oldLines: []string{"a", "c"}, newLines: []string{"new"}}},
			want:    "a\nb\nnew\na\nd\n",
		},
		{
			name:    "repeated anchors advance from cursor",
			content: "anchor\nfirst\nanchor\nsecond\n",
			chunks: []patchChunk{
				{anchor: "anchor", oldLines: []string{"first"}, newLines: []string{"one"}},
				{anchor: "anchor", oldLines: []string{"second"}, newLines: []string{"two"}},
			},
			want: "anchor\none\nanchor\ntwo\n",
		},
		{
			name:    "first line found but full context missing",
			content: "a\nb\na\n",
			chunks:  []patchChunk{{oldLines: []string{"a", "c"}}},
			wantErr: "context not found",
		},
		{
			name:   "empty file insertion",
			chunks: []patchChunk{{newLines: []string{"new"}}},
			want:   "new",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := applyChunks(test.content, test.chunks)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || got != "" {
					t.Fatalf("got %q, %v; want %q rejection", got, err, test.wantErr)
				}
				return
			}

			if err != nil || got != test.want {
				t.Fatalf("got %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestPatchSourceReadSizeBoundary(t *testing.T) {
	for _, size := range []int64{maxPatchFileBytes, maxPatchFileBytes + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			rootPath, err := canonicalPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(rootPath, "source.txt")
			mustWrite(t, path, "")
			if err := os.Truncate(path, size); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			plan := patchPlan{root: root, rootPath: rootPath, files: make(map[string]*pendingFile)}

			file, err := plan.loadFile("source.txt")
			if size > maxPatchFileBytes {
				if !errors.Is(err, errFileTooLarge) || file != nil || len(plan.files) != 0 {
					t.Fatalf("oversized source admitted: file=%v, err=%v", file != nil, err)
				}
				return
			}

			if err != nil || int64(len(file.content)) != size {
				t.Fatalf("exact-limit source rejected: %v", err)
			}
		})
	}
}

func TestPatchCommitRejectsOversizedSourceBeforeWrites(t *testing.T) {
	rootPath, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rootPath, "source.txt")
	mustWrite(t, path, "old\n")
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	plan := patchPlan{root: root, rootPath: rootPath, files: make(map[string]*pendingFile)}
	if err := plan.addOperation(patchOperation{kind: "update", path: "source.txt", chunks: []patchChunk{{oldLines: []string{"old"}, newLines: []string{"new"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := plan.addOperation(patchOperation{kind: "add", path: "added.txt", contents: "added\n"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, maxPatchFileBytes+1); err != nil {
		t.Fatal(err)
	}

	err = plan.commitWithIO(atomicWriteRootFile, (*os.Root).Remove)
	if !errors.Is(err, errFileTooLarge) {
		t.Fatalf("oversized source not rejected by read limit: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil || info.Size() != maxPatchFileBytes+1 {
		t.Fatalf("concurrent source overwritten: %v, %v", info, err)
	}
	if _, err := os.Lstat(filepath.Join(rootPath, "added.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("another target written before rejection: %v", err)
	}
}

func TestApplyPatchCreateUpdateMoveDelete(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "old.txt"), "alpha\nbeta\n")
	mustWrite(t, filepath.Join(root, "delete.txt"), "gone\n")
	if err := os.Chmod(filepath.Join(root, "old.txt"), 0o751); err != nil {
		t.Fatal(err)
	}

	patch := `*** Begin Patch
*** Add File: nested/new.txt
+new
*** Update File: old.txt
*** Move to: moved.txt
@@
 alpha
-beta
+bravo
*** Delete File: delete.txt
*** End Patch`
	result, err := applyPatch(root, patch)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, `"ok":true`) {
		t.Fatalf("unexpected result: %s", result)
	}
	assertContent(t, filepath.Join(root, "nested/new.txt"), "new\n")
	assertContent(t, filepath.Join(root, "moved.txt"), "alpha\nbravo\n")
	info, err := os.Stat(filepath.Join(root, "moved.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o751 {
		t.Fatalf("moved.txt mode = %o, want 751", info.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(root, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old.txt still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "delete.txt")); !os.IsNotExist(err) {
		t.Fatalf("delete.txt still exists: %v", err)
	}
}

func TestExecutePatchUsesRepositoryRootWhenWorkingInSubdirectory(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "subdir")
	mustWrite(t, filepath.Join(root, "file.txt"), "root\n")
	mustWrite(t, filepath.Join(subdir, "file.txt"), "subdir\n")
	sess := &session{CWD: subdir, RepoRoot: root}

	args, err := json.Marshal(map[string]string{
		"patch": "*** Begin Patch\n*** Update File: file.txt\n@@\n-root\n+updated\n*** End Patch",
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &agent{stderr: io.Discard}
	a.executePatch(sess, string(args))

	assertContent(t, filepath.Join(root, "file.txt"), "updated\n")
	assertContent(t, filepath.Join(subdir, "file.txt"), "subdir\n")
}

func TestApplyPatchRejectsEscape(t *testing.T) {
	root := t.TempDir()
	_, err := applyPatch(root, "*** Begin Patch\n*** Add File: ../escape.txt\n+x\n*** End Patch")
	if err == nil {
		t.Fatal("expected path escape error")
	}
}

func TestApplyPatchValidatesWholePlanBeforeWriting(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "existing.txt"), "one\n")
	patch := `*** Begin Patch
*** Add File: new.txt
+new
*** Update File: existing.txt
@@
-missing
+changed
*** End Patch`
	if _, err := applyPatch(root, patch); err == nil {
		t.Fatal("expected invalid update error")
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("new.txt was written before the full plan was valid: %v", err)
	}
	assertContent(t, filepath.Join(root, "existing.txt"), "one\n")
}

func TestApplyPatchRejectsWriteNamespaceConflictsBeforeWriting(t *testing.T) {
	patches := []string{
		"*** Begin Patch\n*** Add File: a\n+parent\n*** Add File: a/b\n+child\n*** End Patch",
		"*** Begin Patch\n*** Add File: a/b\n+child\n*** Add File: a\n+parent\n*** End Patch",
	}
	for _, patch := range patches {
		root := t.TempDir()
		_, err := applyPatch(root, patch)
		if err == nil {
			t.Fatal("expected write namespace conflict")
		}
		if !strings.Contains(err.Error(), "writes both file a and its descendant a/b") {
			t.Fatalf("unexpected conflict error: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, "a")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("patch changed the repository before rejecting the conflict: %v", err)
		}
	}
}

func TestApplyPatchRejectsMoveDestinationNamespaceConflictBeforeWriting(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "one.txt"), "one\n")
	mustWrite(t, filepath.Join(root, "two.txt"), "two\n")
	patch := `*** Begin Patch
*** Update File: one.txt
*** Move to: a
*** Update File: two.txt
*** Move to: a/b
*** End Patch`
	_, err := applyPatch(root, patch)
	if err == nil {
		t.Fatal("expected move destination namespace conflict")
	}
	if !strings.Contains(err.Error(), "writes both file a and its descendant a/b") {
		t.Fatalf("unexpected conflict error: %v", err)
	}
	assertContent(t, filepath.Join(root, "one.txt"), "one\n")
	assertContent(t, filepath.Join(root, "two.txt"), "two\n")
	if _, err := os.Lstat(filepath.Join(root, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("patch changed the repository before rejecting the conflict: %v", err)
	}
}

func TestPatchCommitReportsPartialResult(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "old.txt"), "old\n")
	patch := `*** Begin Patch
*** Add File: a.txt
+a
*** Add File: b.txt
+b
*** Delete File: old.txt
*** End Patch`
	writes := 0
	result, err := applyPatchWithIO(root, patch, func(root *os.Root, path string, content []byte, mode os.FileMode) error {
		writes++
		if writes == 2 {
			return errors.New("injected write failure")
		}
		return atomicWriteRootFile(root, path, content, mode)
	}, (*os.Root).Remove)
	if err == nil {
		t.Fatal("expected commit error")
	}
	var commitErr *patchCommitError
	if !errors.As(err, &commitErr) {
		t.Fatalf("error type = %T, want *patchCommitError", err)
	}
	if strings.Join(commitErr.applied, ",") != "write a.txt" || commitErr.failed != "write b.txt" ||
		strings.Join(commitErr.pending, ",") != "delete old.txt" {
		t.Fatalf("unexpected partial result: %#v", commitErr)
	}
	assertContent(t, filepath.Join(root, "a.txt"), "a\n")
	if _, err := os.Lstat(filepath.Join(root, "b.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed write changed b.txt: %v", err)
	}
	assertContent(t, filepath.Join(root, "old.txt"), "old\n")

	var outputJSON string
	if err := json.Unmarshal(patchToolOutput(result, err), &outputJSON); err != nil {
		t.Fatal(err)
	}
	var output struct {
		OK                     bool     `json:"ok"`
		Outcome                string   `json:"outcome"`
		Applied                []string `json:"applied"`
		Failed                 string   `json:"failed"`
		Pending                []string `json:"pending"`
		ReconciliationRequired bool     `json:"reconciliation_required"`
		Instruction            string   `json:"instruction"`
	}
	if err := json.Unmarshal([]byte(outputJSON), &output); err != nil {
		t.Fatal(err)
	}
	if output.OK || output.Outcome != "partial" || strings.Join(output.Applied, ",") != "write a.txt" || output.Failed != "write b.txt" ||
		strings.Join(output.Pending, ",") != "delete old.txt" || !output.ReconciliationRequired ||
		!strings.Contains(output.Instruction, "before you retry apply_patch") {
		t.Fatalf("tool output lost the partial result: %#v", output)
	}
}

func TestPatchCommitReportsDeleteFailure(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "old.txt"), "old\n")
	patch := `*** Begin Patch
*** Add File: added.txt
+added
*** Delete File: old.txt
*** End Patch`
	_, err := applyPatchWithIO(root, patch, atomicWriteRootFile, func(*os.Root, string) error {
		return errors.New("injected delete failure")
	})
	var commitErr *patchCommitError
	if !errors.As(err, &commitErr) {
		t.Fatalf("error type = %T, want *patchCommitError", err)
	}
	if strings.Join(commitErr.applied, ",") != "write added.txt" || commitErr.failed != "delete old.txt" ||
		len(commitErr.pending) != 0 {
		t.Fatalf("unexpected partial result: %#v", commitErr)
	}
	assertContent(t, filepath.Join(root, "added.txt"), "added\n")
	assertContent(t, filepath.Join(root, "old.txt"), "old\n")
}

func TestPatchCommitRejectsChangedFilesBeforeWriting(t *testing.T) {
	t.Run("source", func(t *testing.T) {
		rootPath := t.TempDir()
		rootPath, err := canonicalPath(rootPath)
		if err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(rootPath, "source.txt"), "old\n")
		root, err := os.OpenRoot(rootPath)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		plan := patchPlan{root: root, rootPath: rootPath, files: make(map[string]*pendingFile)}
		if err := plan.addOperation(patchOperation{kind: "update", path: "source.txt", chunks: []patchChunk{{oldLines: []string{"old"}, newLines: []string{"new"}}}}); err != nil {
			t.Fatal(err)
		}
		if err := plan.addOperation(patchOperation{kind: "add", path: "added.txt", contents: "added\n"}); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(rootPath, "source.txt"), "concurrent\n")
		if err := plan.commitWithIO(atomicWriteRootFile, (*os.Root).Remove); err == nil {
			t.Fatal("expected changed source error")
		}
		assertContent(t, filepath.Join(rootPath, "source.txt"), "concurrent\n")
		if _, err := os.Lstat(filepath.Join(rootPath, "added.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("patch wrote another target before rejecting changed source: %v", err)
		}
	})

	t.Run("destination", func(t *testing.T) {
		rootPath := t.TempDir()
		rootPath, err := canonicalPath(rootPath)
		if err != nil {
			t.Fatal(err)
		}
		root, err := os.OpenRoot(rootPath)
		if err != nil {
			t.Fatal(err)
		}
		defer root.Close()
		plan := patchPlan{root: root, rootPath: rootPath, files: make(map[string]*pendingFile)}
		if err := plan.addOperation(patchOperation{kind: "add", path: "added.txt", contents: "planned\n"}); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, filepath.Join(rootPath, "added.txt"), "concurrent\n")
		if err := plan.commitWithIO(atomicWriteRootFile, (*os.Root).Remove); err == nil {
			t.Fatal("expected changed destination error")
		}
		assertContent(t, filepath.Join(rootPath, "added.txt"), "concurrent\n")
	})
}

func TestPatchCommitDoesNotFollowReplacedParentOutsideRepository(t *testing.T) {
	rootPath := t.TempDir()
	rootPath, err := canonicalPath(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	parent := filepath.Join(rootPath, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	plan := patchPlan{root: root, rootPath: rootPath, files: make(map[string]*pendingFile)}
	if err := plan.addOperation(patchOperation{kind: "add", path: "parent/escape.txt", contents: "blocked\n"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if err := plan.commitWithIO(atomicWriteRootFile, (*os.Root).Remove); err == nil {
		t.Fatal("expected commit to reject the replaced parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("patch wrote outside the repository: %v", err)
	}
}

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
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("%s = %q, want %q", path, b, want)
	}
}
