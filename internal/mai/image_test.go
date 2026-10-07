package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestViewImageExactSolidColorEvidence(t *testing.T) {
	for _, kind := range []string{"solid", "mixed", "transparent"} {
		t.Run(kind, func(t *testing.T) {
			img := image.NewRGBA(image.Rect(0, 0, 2, 2))
			for y := 0; y < 2; y++ {
				for x := 0; x < 2; x++ {
					img.SetRGBA(x, y, color.RGBA{B: 255, A: 255})
				}
			}
			if kind == "mixed" {
				img.SetRGBA(1, 1, color.RGBA{R: 255, A: 255})
			}
			if kind == "transparent" {
				img.SetRGBA(1, 1, color.RGBA{})
			}
			root := t.TempDir()
			path := filepath.Join(root, "color.png")
			var data bytes.Buffer
			if err := png.Encode(&data, img); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := viewImage(root, path)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if kind == "solid" {
				want = "#0000ff"
			}
			if got.SolidColor != want {
				t.Fatalf("solid_color=%q, want=%q", got.SolidColor, want)
			}
		})
	}
}

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

	// Like the file tools, relative paths start at the repository root even
	// when the task runs in a subdirectory.
	sess := &session{CWD: filepath.Join(root, "src"), RepoRoot: root}
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
	if _, err := viewImage(root, outside); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("outside file accepted: %v", err)
	}
	path := filepath.Join(root, "large.png")
	if err := os.WriteFile(path, make([]byte, maxImageBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := viewImage(root, path); err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized file accepted: %v", err)
	}
}

func TestImageToolsReturnImageContentAndHistoryRoundTrips(t *testing.T) {
	for _, tool := range []string{"view_image", "read_skill"} {
		t.Run(tool, func(t *testing.T) {
			sess := deepseekTestSession(t)
			sess.Model, sess.Effort = "deepseek-v4-pro", "max"
			if err := appendUserPrompt(sess, "Inspect the error label in the screenshot."); err != nil {
				t.Fatal(err)
			}
			root := testSkillRoot(t)
			writeTestSkill(t, root, "demo", "demo", "A demonstration skill.")
			var pngData bytes.Buffer
			if err := png.Encode(&pngData, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(sess.CWD, "screen.png"), pngData.String())
			mustWrite(t, filepath.Join(root, "demo", "screen.png"), pngData.String())
			arguments := `{"path":"screen.png"}`
			if tool == "read_skill" {
				arguments = `{"path":"demo","file":"screen.png"}`
			}
			call := functionCall{Type: "function_call", CallID: "image-call", Name: tool, Arguments: arguments}
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch requests {
				case 1:
					if string(body["model"]) != `"deepseek-v4-pro"` || !bytes.Contains(body["tools"], []byte("view_image")) {
						t.Error("request lost the model or image tools")
					}
					deepseekTestResponse(w, "["+string(mustJSON(t, call))+"]")
				case 2:
					// Image-bearing history must reach the model verbatim on
					// the follow-up request.
					if string(body["model"]) != `"deepseek-v4-pro"` || !bytes.Contains(body["reasoning"], []byte(`"max"`)) || !bytes.Contains(body["input"], []byte("input_image")) || !bytes.Contains(body["input"], []byte("data:image/png;base64,")) {
						t.Error("image-bearing history was not replayed")
					}
					deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Continued."}]}]`)
				default:
					t.Error("unexpected extra request")
					w.WriteHeader(http.StatusInternalServerError)
				}
			}))
			defer server.Close()
			var output bytes.Buffer
			a := &agent{
				stdout: &output, stderr: io.Discard, skillsRoots: []string{root},
				sessionPath: filepath.Join(sess.CWD, "session.json"),
				backend: &responsesClient{
					profile:        profileDeepSeek,
					httpClient:     server.Client(),
					endpoint:       server.URL,
					apiKey:         "test",
					stdout:         &output,
					requestTimeout: time.Second,
				},
			}
			a.loadSkillInstructions("Inspect the screenshot.")

			if terminalItems, err := a.runTurn(context.Background(), sess, "instructions"); err != nil || len(terminalItems) != 0 {
				t.Fatalf("image tool turn: terminal=%v err=%v", terminalItems, err)
			}
			saved, err := loadSession(a.sessionPath)
			if err != nil {
				t.Fatal(err)
			}
			sess = saved
			if err := validateResponsesHistory(sess.History, profileDeepSeek); err != nil {
				t.Fatal(err)
			}
			if terminalItems, err := a.runTurn(context.Background(), sess, "instructions"); err != nil || len(terminalItems) == 0 {
				t.Fatalf("continuation: terminal=%v err=%v", terminalItems, err)
			}

			if requests != 2 || output.String() != "Continued.\n" {
				t.Fatalf("unexpected requests or output: requests=%d stdout=%q", requests, output.String())
			}
		})
	}
}
