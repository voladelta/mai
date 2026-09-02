package mai

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func TestApplyPatchRejectsEscape(t *testing.T) {
	root := t.TempDir()
	_, err := applyPatch(root, "*** Begin Patch\n*** Add File: ../escape.txt\n+x\n*** End Patch")
	if err == nil {
		t.Fatal("expected path escape error")
	}
}

func TestApplyPatchRequiresExactContext(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "a.txt"), "one\ntwo\n")
	_, err := applyPatch(root, "*** Begin Patch\n*** Update File: a.txt\n@@\n-three\n+four\n*** End Patch")
	if err == nil {
		t.Fatal("expected missing context error")
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
		if err := plan.commit(); err == nil {
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
		if err := plan.commit(); err == nil {
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
	if err := plan.commit(); err == nil {
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
