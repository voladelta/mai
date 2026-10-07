package mai

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNestingDepthRefusalIsCheap(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	writeTestDeepSeekConfig(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)
	t.Setenv("MAI_DEPTH", "3")

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"work", "--persist"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stderr.String(), "nesting depth 3 exceeds the limit of 2 (MAI_MAX_DEPTH)") {
		t.Fatalf("stderr = %s", stderr.String())
	}
	if requests != 0 {
		t.Fatal("refused run made a model request")
	}
	if _, err := os.Stat(filepath.Join(root, ".mai")); !os.IsNotExist(err) {
		t.Fatalf("refused run created state: %v", err)
	}
}

func TestNestingDepthAtLimitRuns(t *testing.T) {
	t.Chdir(t.TempDir())
	writeTestDeepSeekConfig(t)
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		deepseekTestResponse(w, `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]`)
	}))
	defer server.Close()
	t.Setenv("MAI_DEEPSEEK_URL", server.URL)
	t.Setenv("MAI_DEPTH", "2")

	var stdout, stderr bytes.Buffer
	if code := Main([]string{"work"}, &stdout, &stderr); code != 0 || requests != 1 {
		t.Fatalf("exit code = %d, requests = %d, stderr = %s", code, requests, stderr.String())
	}
}

func TestMalformedDepthEnvFailsClosed(t *testing.T) {
	for _, test := range []struct {
		env   string
		value string
	}{
		{"MAI_DEPTH", "abc"},
		{"MAI_DEPTH", "-1"},
		{"MAI_MAX_DEPTH", "2.5"},
		{"MAI_MAX_DEPTH", "-2"},
	} {
		t.Setenv(test.env, test.value)
		if _, err := nestingDepth(); err == nil || !strings.Contains(err.Error(), test.env) {
			t.Fatalf("%s=%q: error = %v", test.env, test.value, err)
		}
		t.Setenv(test.env, "")
	}
}

func TestChildProcessesObserveIncrementedDepth(t *testing.T) {
	sess := deepseekTestSession(t)
	a := newAgent(io.Discard, io.Discard, "", time.Second*10, false)
	a.depth = 1
	// An inherited MAI_DEPTH records this run's own depth, not the child's;
	// the override must replace it rather than let it through unchanged.
	t.Setenv("MAI_DEPTH", "1")

	raw := a.executeTool(context.Background(), sess, functionCall{
		Name:      "bash",
		Arguments: `{"command":"printf %s \"${MAI_DEPTH:-unset}\""}`,
	})
	var output string
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		OK     bool   `json:"ok"`
		Stdout string `json:"stdout"`
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Stdout != "2" {
		t.Fatalf("child MAI_DEPTH = %q, want %q: %s", result.Stdout, "2", output)
	}

	// The child env is built from cmd.Environ(), so PWD is the session CWD,
	// not this process's working directory.
	raw = a.executeTool(context.Background(), sess, functionCall{
		Name:      "bash",
		Arguments: `{"command":"printf %s \"$PWD\""}`,
	})
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Stdout != sess.CWD {
		t.Fatalf("child PWD = %q, want %q: %s", result.Stdout, sess.CWD, output)
	}
}
