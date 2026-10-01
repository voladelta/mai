package mai

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRegularFileReadersRejectFIFOWithoutWriter(t *testing.T) {
	readers := []struct {
		name string
		read func(string) error
	}{
		{name: "image", read: func(path string) error {
			_, err := viewImage(filepath.Dir(path), filepath.Dir(path), path)
			return err
		}},
		{name: "saved_state", read: func(path string) error {
			_, err := readRegularFile(path)
			return err
		}},
		{name: "transcript", read: func(path string) error {
			file, err := openTranscript(path, os.O_RDONLY)
			if file != nil {
				file.Close()
			}
			return err
		}},
	}

	for _, reader := range readers {
		t.Run(reader.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "input")
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- reader.read(path) }()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), "regular file") {
					t.Fatalf("FIFO rejection = %v", err)
				}
			case <-time.After(time.Second):
				// Release a blocking reader so the regression cannot leak a goroutine.
				writer, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer writer.Close()
				<-done
				t.Fatal("reader blocked waiting for a FIFO writer")
			}
		})
	}
}

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

func TestReadSkillSizeBoundaryAndInternalSymlink(t *testing.T) {
	root := testSkillRoot(t)
	writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
	path := filepath.Join(root, "demo", "guide.txt")
	content := strings.Repeat("a", maxSkillFileBytes)
	mustWrite(t, path, content)
	if err := os.Symlink(path, filepath.Join(root, "demo", "link.txt")); err != nil {
		t.Fatal(err)
	}

	result, err := readSkill([]string{root}, "demo", "link.txt")
	if err != nil || result.Content != content {
		t.Fatalf("exact-limit symlink read: bytes=%d, error=%v", len(result.Content), err)
	}

	mustWrite(t, path, content+"a")
	if _, err := readSkill([]string{root}, "demo", "link.txt"); err == nil || !strings.Contains(err.Error(), "skill file limit") {
		t.Fatalf("oversize skill error = %v", err)
	}
}
