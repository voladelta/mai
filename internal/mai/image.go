package mai

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
)

const (
	maxImageBytes     = 8 << 20
	maxImageDimension = 8192
)

type imageFileResult struct {
	OK        bool   `json:"ok"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	imageURL  string
}

func viewImage(root, cwd, path string) (imageFileResult, error) {
	if path == "" {
		return imageFileResult{}, errors.New("image path is empty")
	}
	requested := path
	if !filepath.IsAbs(requested) {
		requested = filepath.Join(cwd, requested)
	}
	resolved, err := canonicalPath(requested)
	if err != nil {
		return imageFileResult{}, fmt.Errorf("resolve image path: %w", err)
	}
	root, err = canonicalPath(root)
	if err != nil {
		return imageFileResult{}, fmt.Errorf("resolve repository path: %w", err)
	}
	if !pathWithin(root, resolved) {
		return imageFileResult{}, errors.New("image path is outside the repository")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return imageFileResult{}, fmt.Errorf("open image: %w", err)
	}
	defer file.Close()
	data, err := readBoundedFile(file, maxImageBytes)
	if err != nil {
		return imageFileResult{}, fmt.Errorf("read image: %w", err)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return imageFileResult{}, fmt.Errorf("decode image: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || config.Width > maxImageDimension || config.Height > maxImageDimension {
		return imageFileResult{}, fmt.Errorf("image dimensions exceed %d pixels per side", maxImageDimension)
	}
	mediaType := "image/" + format
	if format == "jpeg" {
		mediaType = "image/jpeg"
	}
	return imageFileResult{
		OK: true, Path: resolved, MediaType: mediaType,
		Width: config.Width, Height: config.Height,
		imageURL: "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data),
	}, nil
}
