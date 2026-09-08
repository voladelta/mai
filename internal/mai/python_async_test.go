package mai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPythonAsyncRuntimeCompatibility(t *testing.T) {
	a, sess := pythonTestAgent(t)
	for _, code := range []string{
		`import asyncio, sqlite3, threading
db = sqlite3.connect(':memory:')
db.execute('create table values_(n integer)')
db.execute('insert into values_ values (6)')
owner_thread = threading.get_ident()`,
		`from contextlib import asynccontextmanager
@asynccontextmanager
async def connection():
    await asyncio.sleep(0)
    yield db
async def numbers():
    for n in range(3):
        yield n
async with connection() as shared_db:
    assert threading.get_ident() == owner_thread
    total = shared_db.execute('select n from values_').fetchone()[0]
async for n in numbers():
    total += n
assert total == 9`,
		`async def answer():
    return 42
assert asyncio.run(answer()) == 42
assert threading.get_ident() == owner_thread`,
		`await asyncio.sleep(0)
assert db.execute('select n from values_').fetchone()[0] == 6`,
	} {
		if got := pythonCell(t, a, sess, code); !got.OK {
			t.Fatalf("runtime compatibility: %#v", got)
		}
	}
}

func TestPythonOwnerChainHelper(t *testing.T) {
	mode := os.Getenv("MAI_ASYNC_TEST_MODE")
	if mode != "parent" && mode != "child" {
		return
	}
	root := os.Getenv("MAI_ASYNC_TEST_ROOT")
	if err := os.WriteFile(filepath.Join(root, mode+"-go"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	a := &agent{stderr: io.Discard, timeout: time.Minute}
	sess := &session{CWD: root, RepoRoot: root}
	identity := pythonCell(t, a, sess, "import os; os.getpid()")
	if !identity.OK {
		t.Fatalf("kernel identity failed: %#v", identity)
	}
	if err := os.WriteFile(filepath.Join(root, mode+"-python"), []byte(identity.Stdout), 0600); err != nil {
		t.Fatal(err)
	}
	var code string
	if mode == "parent" {
		a.executable = filepath.Join(root, "child-wrapper")
		a.customAgents = map[string]customAgent{"review": {Name: "review"}}
		code = `await mai.spawn_subagent('review', 'wait')`
		if os.Getenv("MAI_BACKGROUND_OWNER_TEST") == "1" {
			if got := pythonCell(t, a, sess, `child = await mai.spawn('review', 'wait')`); !got.OK {
				t.Fatalf("background admission failed: %#v", got)
			}
			code = `import time; time.sleep(60)`
		}
	} else {
		a.customAgent = &customAgent{Name: "review"}
		code = `await mai.bash('printf %s "$$" > bash; sleep 60 & printf %s "$!" > sleep; wait')`
	}
	args, _ := json.Marshal(map[string]string{"code": code})
	out := a.executeTool(context.Background(), sess, functionCall{Name: "python", CallID: "chain", Arguments: string(args)})
	t.Fatalf("owner chain unexpectedly returned: %s", out)
}

func TestPythonForcedOwnerDeathStopsNestedHostProcesses(t *testing.T) {
	pythonTestAgent(t)
	root := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nexport MAI_ASYNC_TEST_MODE=child\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestPythonOwnerChainHelper$\n"
	if err := os.WriteFile(filepath.Join(root, "child-wrapper"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestPythonOwnerChainHelper$")
	cmd.Env = append(os.Environ(), "MAI_ASYNC_TEST_MODE=parent", "MAI_ASYNC_TEST_ROOT="+root)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var captured cappedBuffer
	captured.max = 8192
	cmd.Stdout, cmd.Stderr = &captured, &captured
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var pids []int
	for _, name := range []string{"parent-go", "parent-python", "child-go", "child-python", "bash", "sleep"} {
		deadline := time.Now().Add(5 * time.Second)
		for {
			data, _ := os.ReadFile(filepath.Join(root, name))
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 {
				pids = append(pids, pid)
				t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("chain did not reach %s: %s", name, captured.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Go owner did not terminate")
	}
	for _, pid := range pids {
		deadline := time.Now().Add(3 * time.Second)
		for {
			out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
			state := strings.TrimSpace(string(out))
			if (err != nil && state == "") || strings.HasPrefix(state, "Z") {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(fmt.Sprintf("process %d remains alive after ancestor Go owner death: %q", pid, state))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestBackgroundChildForcedOwnerDeathStopsDescendants(t *testing.T) {
	t.Setenv("MAI_BACKGROUND_OWNER_TEST", "1")
	TestPythonForcedOwnerDeathStopsNestedHostProcesses(t)
}

func TestPythonReturnedAwaitablesAreValues(t *testing.T) {
	a, sess := pythonTestAgent(t)
	got := pythonCell(t, a, sess, `class Value:
    def __await__(self):
        raise AssertionError('the last value must not be awaited')
    def __repr__(self):
        return 'an awaitable value'
Value()`)
	if !got.OK || got.Stdout != "an awaitable value\n" {
		t.Fatalf("unexpected awaiting: %#v", got)
	}
}

func TestPythonCellTaskCleanup(t *testing.T) {
	a, sess := pythonTestAgent(t)
	got := pythonCell(t, a, sess, `import asyncio
cleanup = []
async def pending():
    try:
        await asyncio.sleep(60)
    finally:
        cleanup.append('cancelled')
task = asyncio.create_task(pending())
await asyncio.sleep(0)`)
	if !got.OK {
		t.Fatalf("cell with pending task: %#v", got)
	}
	got = pythonCell(t, a, sess, `assert task.done()
assert cleanup == ['cancelled']`)
	if !got.OK {
		t.Fatalf("cell retained live async work: %#v", got)
	}
}

func TestPythonTracebackRetainsDefiningCell(t *testing.T) {
	a, sess := pythonTestAgent(t)
	got := pythonCell(t, a, sess, `def earlier():
    raise ValueError("remembered-source")`)
	if !got.OK {
		t.Fatalf("define earlier function: %#v", got)
	}
	got = pythonCell(t, a, sess, "earlier()")
	if got.OK || !strings.Contains(got.Stderr, "<mai:g1:c1>") || !strings.Contains(got.Stderr, `raise ValueError("remembered-source")`) {
		t.Fatalf("traceback lost prior source: %#v", got)
	}
}

func TestPythonHostWaiterCancellationDoesNotLeakEffects(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.RepoRoot = sess.CWD
	got := pythonCell(t, a, sess, `import asyncio, pathlib
task = asyncio.create_task(mai.bash('sleep 0.15; printf A >> events'))
await asyncio.sleep(0.03)
task.cancel()
try:
    await task
except asyncio.CancelledError:
    pass
result = await mai.bash('printf B >> events; cat events')
assert result['ok'], result
assert result['stdout'] in ('AB', 'B'), result
observed = pathlib.Path('events').read_text()`)
	if !got.OK {
		t.Fatalf("cancelled waiter affected serialization: %#v", got)
	}
	got = pythonCell(t, a, sess, `await asyncio.sleep(0.3)
assert pathlib.Path('events').read_text() == observed`)
	if !got.OK {
		t.Fatalf("host operation escaped its cell: %#v", got)
	}
}

func TestPythonAbandonedHostTaskCannotOutliveCell(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.RepoRoot = sess.CWD
	got := pythonCell(t, a, sess, `import asyncio, pathlib
task = asyncio.create_task(mai.bash('sleep 0.2; printf A >> events'))
await asyncio.sleep(0.03)`)
	if !got.OK && !got.StateLost {
		t.Fatalf("abandoned host task must finish, cancel, or lose state: %#v", got)
	}
	got = pythonCell(t, a, sess, `import asyncio, pathlib
path = pathlib.Path('events')
before = path.read_text() if path.exists() else ''
await asyncio.sleep(0.4)
after = path.read_text() if path.exists() else ''
assert before == after, (before, after)`)
	if !got.OK {
		t.Fatalf("host effect arrived after cell completion: %#v", got)
	}
}
