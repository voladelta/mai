package mai

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchCreateUpdateMoveDelete(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "old.txt"), "alpha\nbeta\n")
	mustWrite(t, filepath.Join(root, "delete.txt"), "gone\n")

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
