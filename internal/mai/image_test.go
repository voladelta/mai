package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
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
	output := a.executeViewImage(context.Background(), sess, `{"path":"screen.png"}`)
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

func TestProImageToolsUseFlashAndContinueWithText(t *testing.T) {
	for _, tool := range []string{"view_image", "read_skill"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failure=%t", tool, fail), func(t *testing.T) {
				sess := deepseekTestSession(t)
				sess.Model, sess.Effort = "pro", "max"
				sess.appendEstimatedHistory(json.RawMessage(`{"role":"assistant","content":[{"type":"output_text","text":"PAST-HISTORY-SECRET"}]}`))
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
							t.Error("parent did not use Pro with image tools")
						}
						deepseekTestResponse(w, "["+string(mustJSON(t, call))+"]")
					case 2:
						if string(body["model"]) != `"deepseek-flash"` || string(body["tool_choice"]) != `"none"` || !bytes.Contains(body["reasoning"], []byte(`"low"`)) {
							t.Error("description did not use Flash at low effort without tools")
						}
						if _, exists := body["tools"]; exists {
							t.Error("Flash description can execute tools")
						}
						if !bytes.Contains(body["input"], []byte("data:image/png;base64,")) || !bytes.Contains(body["input"], []byte("Inspect the error label")) || bytes.Contains(body["input"], []byte("PAST-HISTORY-SECRET")) {
							t.Error("Flash did not receive just the image and current task context")
						}
						if fail {
							w.WriteHeader(http.StatusServiceUnavailable)
							return
						}
						deepseekTestResponse(w, `[{"type":"reasoning","content":[{"type":"reasoning_text","text":"FLASH-PRIVATE-REASONING"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The screenshot shows a red error label; its text is illegible."}]}]`)
					case 3:
						if string(body["model"]) != `"deepseek-v4-pro"` || !bytes.Contains(body["reasoning"], []byte(`"max"`)) || bytes.Contains(body["input"], []byte("input_image")) || bytes.Contains(body["input"], []byte("base64")) || bytes.Contains(body["input"], []byte("FLASH-PRIVATE-REASONING")) {
							t.Error("parent model, effort, or text-only history changed")
						}
						if fail {
							if !bytes.Contains(body["input"], []byte("Flash image description failed")) {
								t.Error("parent did not receive the Flash error")
							}
						} else if !bytes.Contains(body["input"], []byte("Flash-generated image description")) || !bytes.Contains(body["input"], []byte("red error label")) || !bytes.Contains(body["input"], []byte("screen.png")) || !bytes.Contains(body["input"], []byte("description_usage")) {
							t.Error("parent did not receive labeled description, source, and usage")
						}
						deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Continued."}]}]`)
					default:
						t.Error("image description was retried")
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
						models:         defaultProviderConfig().Models,
						httpClient:     server.Client(),
						endpoint:       server.URL,
						apiKey:         "test",
						stdout:         &output,
						requestTimeout: time.Second,
					},
				}

				if terminalItems, err := a.runTurn(context.Background(), sess, "instructions"); err != nil || len(terminalItems) != 0 {
					t.Fatalf("image tool turn: terminal=%v err=%v", terminalItems, err)
				}
				saved, err := loadSession(a.sessionPath)
				if err != nil {
					t.Fatal(err)
				}
				sess = saved
				if err := validateResponsesHistory(sess.History, sess.Model, profileDeepSeek); err != nil {
					t.Fatal(err)
				}
				if terminalItems, err := a.runTurn(context.Background(), sess, "instructions"); err != nil || len(terminalItems) == 0 {
					t.Fatalf("continuation: terminal=%v err=%v", terminalItems, err)
				}

				if requests != 3 || output.String() != "Continued.\n" {
					t.Fatalf("unexpected requests or leaked description output: requests=%d stdout=%q", requests, output.String())
				}
				if err := appendUserPrompt(sess, "Continue after checkpoint."); err != nil {
					t.Fatal(err)
				}
				if _, _, err := portableHistory(context.Background(), sess, &checkpointStub{reply: "Image description retained."}); err != nil {
					t.Fatalf("Pro image history cannot compact: %v", err)
				}
			})
		}
	}
}

func TestFlashImageDescriptionRejectsInvalidOutput(t *testing.T) {
	for _, test := range []struct {
		name  string
		items string
	}{
		{name: "no description", items: `[{"type":"reasoning","content":[{"type":"reasoning_text","text":"reasoning only"}]}]`},
		{name: "blank description", items: `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"   "}]}]`},
		{name: "oversized description", items: fmt.Sprintf(`[{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}]`, strings.Repeat("x", (16<<10)+1))},
		{name: "unexpected tool", items: `[{"type":"function_call","call_id":"bad","name":"bash","arguments":"{}"}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				deepseekTestResponse(w, test.items)
			}))
			defer server.Close()
			client := &responsesClient{
				profile:    profileDeepSeek,
				models:     defaultProviderConfig().Models,
				httpClient: server.Client(),
				endpoint:   server.URL,
				apiKey:     "test",
			}

			if _, _, err := client.describeImage(context.Background(), deepseekTestSession(t), "data:image/png;base64,test"); err == nil {
				t.Fatal("invalid Flash description accepted")
			}
		})
	}
}
