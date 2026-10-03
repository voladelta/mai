package mai

import (
	"path/filepath"
	"testing"
)

func TestSecureFilePathAcceptsAbsolutePathsInsideRoot(t *testing.T) {
	root, err := canonicalPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := secureFilePath(root, filepath.Join(root, "pkg", "a.go"))
	if err != nil || got != filepath.Join("pkg", "a.go") {
		t.Fatalf("inside root = %q, %v", got, err)
	}
	for _, bad := range []string{"/etc/passwd", filepath.Dir(root), root + "-sibling/a.go"} {
		if _, err := secureFilePath(root, bad); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
	if _, err := secureFilePath(root, root); err == nil {
		t.Fatal("root itself accepted")
	}
}
