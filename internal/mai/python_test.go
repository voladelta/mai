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
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func pythonTestAgent(t *testing.T) (*agent, *session) {
	t.Helper()
	python := os.Getenv("MAI_PYTHON")
	if python == "" {
		python = "python3"
	}
	if _, err := exec.LookPath(python); err != nil {
		t.Skipf("Python runtime unavailable: %v", err)
	}
	a := &agent{stderr: io.Discard, timeout: 5 * time.Second}
	t.Cleanup(a.python.close)
	return a, &session{CWD: t.TempDir()}
}

func pythonCell(t *testing.T, a *agent, sess *session, code string) pythonResult {
	t.Helper()
	args, _ := json.Marshal(map[string]string{"code": code})
	raw := a.executeTool(context.Background(), sess, functionCall{Name: "python", Arguments: string(args)})
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		t.Fatal(err)
	}
	var result pythonResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPythonPersistsAnalysisAndPartialFailures(t *testing.T) {
	a, sess := pythonTestAgent(t)
	result := pythonCell(t, a, sess, `import sqlite3, csv, io
rows = list(csv.DictReader(io.StringIO('name,total\nA,3\nB,7\n')))
db = sqlite3.connect(':memory:')
db.execute('create table sales (name, total)')
db.executemany('insert into sales values (?, ?)', [(r['name'], int(r['total'])) for r in rows])
len(rows)`)
	if !result.OK || !result.Fresh || result.Generation != 1 || result.Stdout != "2\n" {
		t.Fatalf("initial analysis: %#v", result)
	}

	result = pythonCell(t, a, sess, "db.execute('select sum(total) from sales').fetchone()[0]")
	if !result.OK || result.Fresh || result.Stdout != "10\n" {
		t.Fatalf("connection was not retained: %#v", result)
	}

	result = pythonCell(t, a, sess, `answer = 42
raise ValueError('bad cell')`)
	if result.OK || result.StateLost || !strings.Contains(result.Stderr, "ValueError: bad cell") {
		t.Fatalf("exception handling: %#v", result)
	}

	result = pythonCell(t, a, sess, "answer")
	if !result.OK || result.Stdout != "42\n" {
		t.Fatalf("partial state was lost: %#v", result)
	}

	result = pythonCell(t, a, sess, "None")
	if !result.OK || result.Stdout != "" {
		t.Fatalf("None was displayed: %#v", result)
	}
}

func TestPythonBoundsNativeOutputAndKeepsProtocolSeparate(t *testing.T) {
	a, sess := pythonTestAgent(t)
	result := pythonCell(t, a, sess, `import os
_ = os.write(1, b'HEAD' + b'x' * 200000 + b'TAIL')
_ = os.write(2, b'ERR' + b'y' * 200000 + b'END')`)
	if !result.OK || !result.Truncated || result.StdoutBytes != 200008 || result.StderrBytes != 200006 || result.OmittedBytes != 400014-2*maxToolStreamBytes {
		t.Fatalf("output metrics: %#v", result)
	}
	if !strings.HasPrefix(result.Stdout, "HEAD") || !strings.HasSuffix(result.Stdout, "TAIL") || !strings.HasSuffix(result.Stderr, "END") {
		t.Fatal("stream boundaries were lost")
	}

	result = pythonCell(t, a, sess, `print('{"ok":false}')
input()`)
	if result.OK || result.StateLost || !strings.Contains(result.Stderr, "EOFError") {
		t.Fatalf("input consumed protocol: %#v", result)
	}

	result = pythonCell(t, a, sess, "21 * 2")
	if !result.OK || result.Stdout != "42\n" || result.Stderr != "" {
		t.Fatalf("output leaked between cells: %#v", result)
	}
}

func TestPythonClassesBelongToPersistentMainModule(t *testing.T) {
	a, sess := pythonTestAgent(t)
	result := pythonCell(t, a, sess, `from __future__ import annotations
from dataclasses import dataclass
import pickle

@dataclass
class Row:
    total: int

row = Row(12)
blob = pickle.dumps(row)`)
	if !result.OK {
		t.Fatalf("class creation failed: %#v", result)
	}

	result = pythonCell(t, a, sess, "pickle.loads(blob).total")
	if !result.OK || result.Stdout != "12\n" {
		t.Fatalf("class module did not persist: %#v", result)
	}
}

func TestPythonIdleDeathReportsLossBeforeNextCell(t *testing.T) {
	a, sess := pythonTestAgent(t)
	if result := pythonCell(t, a, sess, "1"); !result.OK {
		t.Fatal(result)
	}
	if err := a.python.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-a.python.wait
	result := pythonCell(t, a, sess, "open('unexpected', 'w').close()")
	if result.OK || !result.StateLost || !strings.Contains(result.Error, "not executed") {
		t.Fatalf("idle loss not reported: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(sess.CWD, "unexpected")); !os.IsNotExist(err) {
		t.Fatalf("cell ran after idle death: %v", err)
	}
}

func TestPythonResetAndNewAgentStartFresh(t *testing.T) {
	a, sess := pythonTestAgent(t)
	if result := pythonCell(t, a, sess, "secret = 7"); !result.OK {
		t.Fatal(result)
	}
	pid := a.python.cmd.Process.Pid
	a.executeTool(context.Background(), sess, functionCall{Name: "python", Arguments: `{"reset":true}`})
	if a.python.cmd != nil || syscall.Kill(pid, 0) == nil {
		t.Fatal("reset did not reap the kernel")
	}
	result := pythonCell(t, a, sess, "'secret' in globals()")
	if !result.OK || !result.Fresh || result.Generation != 2 || result.Stdout != "False\n" {
		t.Fatalf("reset retained state: %#v", result)
	}

	b, _ := pythonTestAgent(t)
	result = pythonCell(t, b, sess, "'secret' in globals()")
	if !result.OK || !result.Fresh || result.Generation != 1 || result.Stdout != "False\n" {
		t.Fatalf("new agent retained state: %#v", result)
	}
}

func TestPythonTimeoutAndCancellationKillProcessGroup(t *testing.T) {
	for _, cancelCell := range []bool{false, true} {
		t.Run(strconv.FormatBool(cancelCell), func(t *testing.T) {
			a, sess := pythonTestAgent(t)
			result := pythonCell(t, a, sess, `import subprocess, sys
child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'])
child.pid`)
			if !result.OK {
				t.Fatal(result)
			}
			child, err := strconv.Atoi(strings.TrimSpace(result.Stdout))
			if err != nil {
				t.Fatal(err)
			}
			pid := a.python.cmd.Process.Pid
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 100 * time.Millisecond
			if cancelCell {
				timeout = 5 * time.Second
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			result = a.python.execute(ctx, sess.CWD, "import time; time.sleep(30)", false, timeout)
			if result.OK || !result.StateLost || result.TimedOut == cancelCell || a.python.cmd != nil || syscall.Kill(pid, 0) == nil {
				t.Fatalf("failed cleanup: %#v", result)
			}
			// A killed descendant can briefly be a zombie before init reaps it.
			state, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(child)).Output()
			if value := strings.TrimSpace(string(state)); value != "" && !strings.HasPrefix(value, "Z") {
				t.Fatalf("child remains running: %s", value)
			}
			if next := pythonCell(t, a, sess, "1"); !next.OK || !next.Fresh || next.Generation != 2 {
				t.Fatalf("kernel did not recover: %#v", next)
			}
		})
	}
}

func TestPythonCrashDoesNotReplayCell(t *testing.T) {
	a, sess := pythonTestAgent(t)
	result := pythonCell(t, a, sess, `import os
with open('effect', 'a') as f:
    f.write('once')
os._exit(2)`)
	if result.OK || !result.StateLost || !strings.Contains(result.Error, "Do not replay") {
		t.Fatalf("crash result: %#v", result)
	}
	content, err := os.ReadFile(filepath.Join(sess.CWD, "effect"))
	if err != nil || string(content) != "once" {
		t.Fatalf("cell was replayed: %q, %v", content, err)
	}
	if next := pythonCell(t, a, sess, "1"); !next.OK || !next.Fresh {
		t.Fatalf("new kernel failed: %#v", next)
	}
}

func TestPythonRejectsInvalidArgumentsWithoutStarting(t *testing.T) {
	a := &agent{stderr: io.Discard}
	for _, args := range []string{`{}`, `null`, `{"code":null}`, `{"code":" "}`, `{"reset":false}`, `{"reset":null}`, `{"code":"1","reset":true}`, `{"other":true}`, `{"code":"1","extra":2}`} {
		result := a.executeTool(context.Background(), &session{}, functionCall{Name: "python", Arguments: args})
		if !strings.Contains(string(result), "invalid python arguments") || a.python.cmd != nil {
			t.Fatalf("accepted %s: %s", args, result)
		}
	}
}

func TestPythonMissingRuntimeIsActionable(t *testing.T) {
	t.Setenv("MAI_PYTHON", filepath.Join(t.TempDir(), "missing-python"))
	a := &agent{stderr: io.Discard}
	result := pythonCell(t, a, &session{}, "1")
	if result.OK || !strings.Contains(result.Error, "MAI_PYTHON") || result.Fresh {
		t.Fatalf("missing runtime: %#v", result)
	}
}

func TestPythonHistoryCompactionAndRunCleanup(t *testing.T) {
	a, sess := pythonTestAgent(t)
	writeTestCodexAuth(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if hasCompactionTrigger(body["input"]) {
			writeSSEItem(t, w, `{"type":"compaction","encrypted_content":"summary"}`, 100)
			return
		}
		writeSSEItem(t, w, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}`, 100)
	}))
	defer server.Close()
	a.stdout = io.Discard
	a.client = newCodexClient(io.Discard, time.Second)
	a.client.endpoint = server.URL
	a.sessionPath = filepath.Join(t.TempDir(), "session.json")
	sess.Version, sess.ID = stateVersion, "01234567-89ab-cdef-0123-456789abcdef"
	sess.Model, sess.Effort, sess.RepoRoot = "luna", "m", sess.CWD
	err := a.executeCalls(context.Background(), sess, []functionCall{{Name: "python", CallID: "cell", Arguments: `{"code":"value = 42"}`}})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := loadSession(a.sessionPath)
	if err != nil || len(saved.History) != 1 || historyItemType(saved.History[0]) != "function_call_output" {
		t.Fatalf("Python result not persisted: %#v, %v", saved, err)
	}

	sess.ContextTokens = modelContextWindow
	if err := a.compactIfNeeded(context.Background(), sess, "instructions"); err != nil {
		t.Fatal(err)
	}
	if result := pythonCell(t, a, sess, "value"); !result.OK || result.Fresh || result.Stdout != "42\n" {
		t.Fatalf("compaction lost Python state: %#v", result)
	}

	pid := a.python.cmd.Process.Pid
	if err := a.run(context.Background(), sess, "finish"); err != nil {
		t.Fatal(err)
	}
	if a.python.cmd != nil || syscall.Kill(pid, 0) == nil {
		t.Fatal("agent run did not reap its Python process")
	}
}
