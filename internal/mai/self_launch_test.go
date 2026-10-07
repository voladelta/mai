package mai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaiCLISelfLaunchThroughTools(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "mai binary")
	build := exec.Command("go", "build", "-o", binary, "./cmd/mai")
	build.Dir = "../.."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Mai: %v\n%s", err, output)
	}

	for _, tool := range []string{"bash", "python"} {
		t.Run(tool, func(t *testing.T) {
			if tool == "python" {
				pythonTestAgent(t)
			}

			writeTestDefaultProviderConfig(t)
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Authorization") != "Bearer test-key" {
					t.Error("nested run did not inherit credentials")
				}

				var body struct {
					Input []json.RawMessage `json:"input"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}

				if requests == 1 {
					if len(body.Input) != 1 || !strings.Contains(string(body.Input[0]), "Act as a reviewer") {
						t.Errorf("nested run did not receive a fresh task: %s", mustJSON(t, body.Input))
					}

					deepseekTestResponse(w, `[{"type":"function_call","call_id":"nested-call","name":"bash","arguments":"{\"command\":\"printf nested-tool-result\"}"}]`)
					return
				}

				if len(body.Input) != 3 || !strings.Contains(string(body.Input[2]), "nested-tool-result") {
					t.Errorf("nested CLI lost its tool result: %s", mustJSON(t, body.Input))
				}

				deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"review complete"}]}]`)
			}))
			defer server.Close()
			t.Setenv("MAI_BASE_URL", server.URL)

			root := t.TempDir()
			sess := &session{CWD: root, RepoRoot: root}
			a := newAgent(io.Discard, io.Discard, "", time.Second, false)
			t.Cleanup(a.python.close)
			args := []string{binary, "--no-input", "--skip-skills", "--max-turns=2", "--", "Act as a reviewer. Inspect the current diff; do not edit or delegate further."}
			var arguments string
			if tool == "bash" {
				quoted := make([]string, len(args))
				for i, arg := range args {
					quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
				}
				arguments = string(mustJSON(t, map[string]any{"command": strings.Join(quoted, " "), "timeout_ms": 10000}))
			} else {
				code := "import subprocess\nresult = subprocess.run(" + string(mustJSON(t, args)) + ", capture_output=True, text=True, timeout=10)\nassert result.returncode == 0, result.stderr\nprint(result.stdout, end='')"
				arguments = string(mustJSON(t, map[string]string{"code": code}))
			}

			raw := a.executeTool(context.Background(), sess, functionCall{Name: tool, Arguments: arguments})
			var output string
			if err := json.Unmarshal(raw, &output); err != nil {
				t.Fatal(err)
			}

			var result struct {
				OK     bool   `json:"ok"`
				Stdout string `json:"stdout"`
				Stderr string `json:"stderr"`
			}
			if err := json.Unmarshal([]byte(output), &result); err != nil {
				t.Fatal(err)
			}

			if !result.OK || result.Stdout != "review complete\n" || requests != 2 {
				t.Fatalf("nested CLI result = %#v, requests = %d", result, requests)
			}

			if _, err := os.Stat(filepath.Join(root, ".mai")); !os.IsNotExist(err) {
				t.Fatalf("stateless nested run created saved state: %v", err)
			}
		})
	}
}
