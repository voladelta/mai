package mai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveDeepSeekImageMessage(t *testing.T) {
	if os.Getenv("MAI_LIVE_DEEPSEEK_TOOLS") != "1" {
		t.Skip("set MAI_LIVE_DEEPSEEK_TOOLS=1")
	}
	requireDeepSeekLiveProvider(t)
	sess := deepseekTestSession(t)
	sess.History = nil
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, color.RGBA{B: 255, A: 255})
		}
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{
		"role": "user",
		"content": []map[string]string{
			{"type": "input_text", "text": "Identify the solid color of the supplied image. Do not guess if it is unavailable."},
			{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngBytes.Bytes()), "detail": "auto"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sess.History = append(sess.History, input)
	a := newAgent(io.Discard, io.Discard, "", time.Minute, false)
	defer a.close()
	a.skipSkills = true
	provider := liveToolProvider(t, sess)
	sess.Model = provider.Model
	if err := a.configureBackend(sess, provider); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	result, err := a.backend.stream(ctx, sess, "Answer the user's image question directly.")
	if err != nil {
		t.Fatal(err)
	}
	answer, err := assistantResponseText(result.items, "\n")
	if err != nil || !strings.Contains(strings.ToLower(answer), "blue") {
		t.Fatalf("direct user-image interpretation: answer=%q err=%v", answer, err)
	}
	t.Logf("DIRECT_IMAGE answer=%q", answer)
}

func requireDeepSeekLiveProvider(t *testing.T) {
	t.Helper()
	if os.Getenv("MAI_LIVE_PROVIDER") != "deepseek" {
		t.Skip("set MAI_LIVE_PROVIDER=deepseek and MAI_LIVE_CONFIG to run this DeepSeek probe")
	}
}

func liveToolProvider(t *testing.T, sess *session) providerConfig {
	t.Helper()
	name := os.Getenv("MAI_LIVE_PROVIDER")
	if name == "" {
		// Offline callers stand in for the built-in default provider so
		// MAI_BASE_URL redirection applies. Paid probes must set
		// MAI_LIVE_PROVIDER and MAI_LIVE_CONFIG explicitly.
		return defaultProviderConfig()
	}
	path := os.Getenv("MAI_LIVE_CONFIG")
	if path == "" {
		t.Fatal("MAI_LIVE_CONFIG must name a provider config when MAI_LIVE_PROVIDER is set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := decodeProviderConfig(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := cfg.Providers[name]
	if !ok {
		t.Fatalf("provider %q is not configured", name)
	}
	sess.Provider = name
	return provider
}

// Paid model-driven integration.
func TestLiveDeepSeekTools(t *testing.T) {
	if os.Getenv("MAI_LIVE_DEEPSEEK_TOOLS") != "1" {
		t.Skip("set MAI_LIVE_DEEPSEEK_TOOLS=1")
	}
	requireDeepSeekLiveProvider(t)
	sess := deepseekTestSession(t)
	sess.History = nil
	token := "TOKEN-" + sess.ID
	mustWrite(t, filepath.Join(sess.CWD, "verification.txt"), token+"\n")
	mustWrite(t, filepath.Join(sess.CWD, "numbers.txt"), "17\n25\n")
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.SetRGBA(x, y, color.RGBA{B: 255, A: 255})
		}
	}
	file, err := os.Create(filepath.Join(sess.CWD, "swatch.png"))
	if err != nil {
		t.Fatal(err)
	}
	encodeErr := png.Encode(file, img)
	closeErr := file.Close()
	if encodeErr != nil || closeErr != nil {
		t.Fatalf("image fixture: encode=%v close=%v", encodeErr, closeErr)
	}
	prompt := `Complete this isolated tool integration task. Do not access credentials, network, other projects, or delegate. Never write files using Python, Ruby, or Node.
1. Use bash to read verification.txt and retrieve the token. Skills are disabled.
2. Use view_image on swatch.png to identify its solid color. Do not infer the color from image bytes or another tool.
3. Use bash to read numbers.txt.
4. Use bash to print the sum of those numbers.
5. Use write directly to create result.txt containing exactly three lines: the token, the lowercase color name, and the total before incrementing. End every line with a newline. Then use edit directly to increment only the total line. Use read directly to verify the resulting file.
6. Use bash to read result.txt and verify it against your tool observations.
Your entire final response must be the single token TOOLS_OK if verified. Do not include a summary, explanation, bullets, or any other text.`
	if err := appendUserPrompt(sess, prompt); err != nil {
		t.Fatal(err)
	}
	a := newAgent(io.Discard, io.Discard, "", 2*time.Minute, false)
	a.skipSkills, a.maxTurns = true, 12
	provider := liveToolProvider(t, sess)
	sess.Model = provider.Model
	if err := a.configureBackend(sess, provider); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	started := time.Now()
	if err := a.run(ctx, sess, prompt); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	answered := false
	answerText := ""
	for _, raw := range sess.History {
		var item struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			CallID string          `json:"call_id"`
			Output json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		if item.Type == "function_call" {
			counts[item.Name]++
		}
		if item.Type == "function_call_output" {
			var output string
			if json.Unmarshal(item.Output, &output) != nil {
				// Typed image content is checked by its resulting
				// observed value in result.txt, rather than a JSON status.
				continue
			}
			var result struct {
				OK          *bool  `json:"ok"`
				Description string `json:"description"`
			}
			// Image results are typed content; other tools return JSON.
			if json.Unmarshal([]byte(output), &result) == nil && result.OK != nil {
				if result.Description != "" {
					t.Logf("IMAGE_DESCRIPTION %s", result.Description)
				}
				if !*result.OK {
					t.Errorf("tool %s failed: %s", item.CallID, output)
				}
			}
		}
		entry, visible, err := visibleTranscriptEntry(raw)
		if err != nil {
			t.Fatal(err)
		}
		answered = answered || (visible && entry.Kind == "assistant" && strings.TrimSpace(entry.Text) == "TOOLS_OK")
		if visible && entry.Kind == "assistant" {
			answerText = entry.Text
		}
	}
	for _, name := range []string{"bash", "read", "write", "edit", "view_image"} {
		if counts[name] == 0 {
			t.Errorf("missing real %s call", name)
		}
	}
	got, err := os.ReadFile(filepath.Join(sess.CWD, "result.txt"))
	want := token + "\nblue\n43\n"
	t.Logf("FLASH_TOOLS provider=%s model=%s counts=%s turns=%d duration_ms=%d", sess.Provider, provider.Model, fmt.Sprint(counts), a.modelTurns, time.Since(started).Milliseconds())
	if err != nil || string(got) != want || !answered {
		t.Fatalf("tool effects: file=%q err=%v answered=%v answer=%q", got, err, answered, answerText)
	}
}
