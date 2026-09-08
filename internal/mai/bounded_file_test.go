package mai

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadBoundedFileSizeBoundary(t *testing.T) {
	for _, content := range []string{"", "1234", "12345"} {
		t.Run(content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data")
			mustWrite(t, path, content)
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()

			data, err := readBoundedFile(file, 4)
			if len(content) > 4 {
				if !errors.Is(err, errFileTooLarge) || data != nil {
					t.Fatalf("oversize read = %q, %v", data, err)
				}
				return
			}

			if err != nil || string(data) != content {
				t.Fatalf("read = %q, %v; want %q", data, err, content)
			}
		})
	}
}

func TestReadBoundedFileRejectsDirectory(t *testing.T) {
	file, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	if _, err := readBoundedFile(file, 4); !errors.Is(err, errNotRegularFile) {
		t.Fatalf("directory read error = %v", err)
	}
}

func TestReadSkillFileSizeBoundaryAndInternalSymlink(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	path := filepath.Join(root, "demo", "guide.txt")
	content := strings.Repeat("a", maxSkillFileBytes)
	mustWrite(t, path, content)
	if err := os.Symlink(path, filepath.Join(root, "demo", "link.txt")); err != nil {
		t.Fatal(err)
	}

	result, err := readSkillFile(root, "demo", "link.txt")
	if err != nil || result.Content != content {
		t.Fatalf("exact-limit symlink read: bytes=%d, error=%v", len(result.Content), err)
	}

	mustWrite(t, path, content+"a")
	if _, err := readSkillFile(root, "demo", "link.txt"); err == nil || !strings.Contains(err.Error(), "skill file limit") {
		t.Fatalf("oversize skill error = %v", err)
	}
}

func TestLoadCustomAgentFileSizeBoundaryAndSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "demo.toml")
	content := "name = \"demo\"\ndescription = \"Demo\"\ndeveloper_instructions = \"Do the work\"\nmodel_reasoning_effort = \"high\"\n#"
	content += strings.Repeat("a", maxAgentFileBytes-len(content))
	mustWrite(t, path, content)

	if _, err := loadCustomAgentFile(path); err != nil {
		t.Fatalf("exact-limit configuration: %v", err)
	}

	link := path + ".link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCustomAgentFile(link); err == nil {
		t.Fatal("accepted configuration symlink")
	}

	mustWrite(t, path, content+"a")
	if _, err := loadCustomAgentFile(path); err == nil || !strings.Contains(err.Error(), "agent configuration exceeds") {
		t.Fatalf("oversize configuration error = %v", err)
	}
}

func TestChildJournalRejectsOversizeAndSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session")
	journal := path + ".children.json"
	target := filepath.Join(t.TempDir(), "journal")
	mustWrite(t, target, "[]")
	if err := os.Symlink(target, journal); err != nil {
		t.Fatal(err)
	}

	if _, err := openChildRegistry(path); err == nil || !strings.Contains(err.Error(), "read child journal") {
		t.Fatalf("symlink journal error = %v", err)
	}

	if err := os.Remove(journal); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, journal, "[]")
	if err := os.Truncate(journal, maxChildJournalBytes+1); err != nil {
		t.Fatal(err)
	}

	if _, err := openChildRegistry(path); err == nil || !strings.Contains(err.Error(), "bounded regular file") {
		t.Fatalf("oversize journal error = %v", err)
	}
}
