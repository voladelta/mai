package mai

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func secureFilePath(root, rel string) (string, error) {
	if rel == "" || strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("invalid file path %q", rel)
	}
	if filepath.IsAbs(rel) {
		// Models echo the absolute repository root from the prompt; accept paths
		// under it and reject everything else.
		if !pathWithin(root, rel) {
			return "", fmt.Errorf("file path escapes repository: %s", rel)
		}
		inside, err := filepath.Rel(root, rel)
		if err != nil {
			return "", fmt.Errorf("invalid file path %q", rel)
		}
		rel = inside
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file path escapes repository: %s", rel)
	}
	abs := filepath.Join(root, clean)
	if !pathWithin(root, abs) {
		return "", fmt.Errorf("file path escapes repository: %s", rel)
	}
	parent := filepath.Dir(abs)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			if !pathWithin(root, resolved) {
				return "", fmt.Errorf("file path resolves outside repository: %s", rel)
			}

			// Use one identity for paths reached through directory aliases, including
			// new descendants whose nearest existing parent is the alias target.
			suffix, err := filepath.Rel(parent, abs)
			if err != nil {
				return "", fmt.Errorf("resolve file path %s: %w", rel, err)
			}
			return filepath.Rel(root, filepath.Join(resolved, suffix))
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("resolve file path %s: %w", rel, err)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", fmt.Errorf("cannot resolve parent for %s", rel)
		}
		parent = next
	}
}

func atomicWriteRootFile(root *os.Root, path string, content []byte, mode os.FileMode) error {
	return publishRootFile(root, path, content, mode, false, nil)
}

// publishRootFile stages complete content privately. beforePublish rechecks the
// observed file immediately before publication; create uses a no-replace link.
func publishRootFile(root *os.Root, path string, content []byte, mode os.FileMode, create bool, beforePublish func() error) error {
	dir := filepath.Dir(path)
	if err := root.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	var suffix [12]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Errorf("create temporary name for %s: %w", path, err)
	}
	staging := filepath.Join(dir, ".mai-"+hex.EncodeToString(suffix[:]))
	if err := root.Mkdir(staging, 0o700); err != nil {
		return fmt.Errorf("create staging directory for %s: %w", path, err)
	}
	defer root.Remove(staging)
	tmpPath := filepath.Join(staging, "content")
	tmp, err := root.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	defer func() {
		_ = tmp.Close()
		_ = root.Remove(tmpPath)
	}()
	if _, err := tmp.Write(content); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set mode for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", path, err)
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return err
		}
	}
	if create {
		if err := root.Link(tmpPath, path); err != nil {
			return fmt.Errorf("create %s without overwriting: %w", path, err)
		}
		return nil
	}
	if err := root.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func atomicWriteFile(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open directory for %s: %w", path, err)
	}
	defer root.Close()
	return atomicWriteRootFile(root, filepath.Base(path), content, mode)
}
