package mai

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestViewImageReturnsTypedImageWithinRepository(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "screen.png")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(file, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	sess := &session{CWD: root, RepoRoot: root}
	a := &agent{stderr: &bytes.Buffer{}}
	output := a.executeViewImage(sess, `{"path":"screen.png"}`)
	var parts []struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(output, &parts); err != nil {
		t.Fatal(err)
	}
	if len(parts) != 2 || parts[1].Type != "input_image" || !strings.HasPrefix(parts[1].ImageURL, "data:image/png;base64,") {
		t.Fatalf("unexpected image output: %s", output)
	}
}

func TestViewImageRejectsOutsideAndOversizedFiles(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.png")
	if err := os.WriteFile(outside, []byte("not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := viewImage(root, root, outside); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside file accepted: %v", err)
	}
	path := filepath.Join(root, "large.png")
	if err := os.WriteFile(path, make([]byte, maxImageBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := viewImage(root, root, path); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized file accepted: %v", err)
	}
}
