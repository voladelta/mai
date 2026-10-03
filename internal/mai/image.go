package mai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	maxImageBytes     = 8 << 20
	maxImageDimension = 8192
)

func (a *agent) describeImageOutput(ctx context.Context, sess *session, metadata, imageURL string) json.RawMessage {
	client, ok := a.backend.(*responsesClient)
	if !ok {
		return textToolOutput(toolError("image description failed", errors.New("Flash backend is unavailable")))
	}
	fmt.Fprintln(a.stderr, "→ Flash image description")
	started := time.Now()
	description, usage, err := client.describeImage(ctx, sess, imageURL)
	if err != nil {
		return textToolOutput(toolError("Flash image description failed", err))
	}

	var result map[string]any
	_ = json.Unmarshal([]byte(metadata), &result)
	result["description"] = description
	result["description_model"] = "flash"
	result["description_note"] = "Flash-generated image description; a lossy interpretation, not verified facts. Uncertain details require verification."
	result["description_duration_ms"] = time.Since(started).Milliseconds()
	if usage != nil {
		result["description_usage"] = usage
	}
	return textToolOutput(marshalToolResult(result))
}

type imageFileResult struct {
	OK         bool   `json:"ok"`
	Path       string `json:"path"`
	MediaType  string `json:"media_type"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SolidColor string `json:"solid_color,omitempty"`
	imageURL   string
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
	file, err := os.OpenFile(resolved, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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
	solidColor := ""
	// Exact pixel evidence for small uniform images. Do not infer a color from
	// an average or a thumbnail, and bound decoding work for larger images.
	if config.Width*config.Height <= 1_000_000 {
		decoded, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return imageFileResult{}, fmt.Errorf("decode image pixels: %w", err)
		}
		solidColor = uniformImageColor(decoded)
	}
	return imageFileResult{
		OK: true, Path: resolved, MediaType: mediaType,
		Width: config.Width, Height: config.Height,
		SolidColor: solidColor,
		imageURL:   "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data),
	}, nil
}

func uniformImageColor(img image.Image) string {
	bounds := img.Bounds()
	r, g, b, a := img.At(bounds.Min.X, bounds.Min.Y).RGBA()
	if a != 0xffff {
		return ""
	}
	if r%0x101 != 0 || g%0x101 != 0 || b%0x101 != 0 {
		return "" // A six-digit hex value cannot represent this 16-bit color exactly.
	}
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r2, g2, b2, a2 := img.At(x, y).RGBA()
			if r2 != r || g2 != g || b2 != b || a2 != a {
				return ""
			}
		}
	}
	return fmt.Sprintf("#%02x%02x%02x", r>>8, g>>8, b>>8)
}
