package mai

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func childTestExecutable(t *testing.T, root, script string) string {
	t.Helper()
	path := filepath.Join(root, "child")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func awaitChild(t *testing.T, r *childRegistry, id string) childRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		record, err := r.inspect(1, id, false)
		if err != nil {
			t.Fatal(err)
		}
		if record.Status != "running" {
			return record
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child did not finish")
	return childRecord{}
}

func TestChildFailureKeepsTypedOutcomeInJournal(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.json")
	r, err := openChildRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.stop)
	executable := childTestExecutable(t, root, "printf partial; printf diagnostic >&2; exit 7")

	record, err := r.spawn(context.Background(), 1, executable, root, "review", "work", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	failed := awaitChild(t, r, record.ID)
	result := decodeSubagentResult(t, string(failed.Result))
	if failed.Status != "failed" || result.OK || result.ExitCode != 7 || result.Output != "partial" || result.Stderr != "diagnostic" {
		t.Fatalf("failure lost at registry boundary: %#v %#v", failed, result)
	}

	data, err := os.ReadFile(path + ".children.json")
	if err != nil {
		t.Fatal(err)
	}
	var saved []childRecord
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Status != "failed" || decodeSubagentResult(t, string(saved[0].Result)) != result {
		t.Fatalf("durable failure changed: %s", data)
	}
}

func TestChildAdmissionCompletionAndRecovery(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.json")
	r, err := openChildRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.stop)
	executable := childTestExecutable(t, root, "touch started\nwhile [ ! -f release ]; do sleep 0.01; done\nprintf done")
	record, err := r.spawn(context.Background(), 1, executable, root, "review", "work", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path + ".children.json")
	if err != nil || !strings.Contains(string(data), record.ID) || !strings.Contains(string(data), "running") {
		t.Fatalf("admission not durable: %s %v", data, err)
	}

	// The process remains blocked while independent session writes proceed.
	for i := 0; i < 20; i++ {
		if err := saveJSON(path, &session{History: []json.RawMessage{json.RawMessage(`{"type":"compaction"}`)}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	completed := awaitChild(t, r, record.ID)
	if completed.Status != "completed" || !strings.Contains(string(completed.Result), "done") {
		t.Fatalf("completion: %#v", completed)
	}
	repeated, err := r.inspect(1, record.ID, false)
	if err != nil || string(repeated.Result) != string(completed.Result) {
		t.Fatalf("changed result: %#v %v", repeated, err)
	}

	recovered, err := openChildRegistry(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recovered.inspect(1, record.ID, false); err == nil {
		t.Fatal("restored a live handle")
	}
	if !strings.Contains(recovered.recoveryInstructions(), record.ID) || !strings.Contains(recovered.recoveryInstructions(), "completed") {
		t.Fatal("missing recovery guidance")
	}

	record.Status = "running"
	if err := saveJSON(path+".children.json", []childRecord{record}); err != nil {
		t.Fatal(err)
	}
	recovered, err = openChildRegistry(path)
	if err != nil || recovered.recovered[0].Status != "unknown" {
		t.Fatalf("unfinished recovery: %#v %v", recovered, err)
	}
}

func TestChildLimitsCancellationAndStaleHandles(t *testing.T) {
	root := t.TempDir()
	r, _ := openChildRegistry("")
	t.Cleanup(r.stop)
	executable := childTestExecutable(t, root, "sleep 30")
	var ids []string
	for i := 0; i < maxActiveChildren; i++ {
		record, err := r.spawn(context.Background(), 1, executable, root, "review", "work", time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, record.ID)
	}
	if _, err := r.spawn(context.Background(), 1, executable, root, "review", "work", time.Minute); err == nil {
		t.Fatal("accepted fifth active child")
	}
	if _, err := r.inspect(2, ids[0], true); err == nil {
		t.Fatal("stale generation accepted")
	}
	if _, err := r.inspect(1, "unknown", true); err == nil {
		t.Fatal("unknown ID accepted")
	}

	for i := 0; i < 2; i++ {
		if _, err := r.inspect(1, ids[0], true); err != nil {
			t.Fatal(err)
		}
	}
	if got := awaitChild(t, r, ids[0]); got.Status != "cancelled" {
		t.Fatalf("cancel result: %#v", got)
	}
	r.stop()
	next := r.nextGeneration()
	if _, err := next.inspect(1, ids[0], false); err == nil {
		t.Fatal("reset restored handle")
	}
	for len(next.recovered) < maxChildHandles {
		next.recovered = append(next.recovered, childRecord{Status: "unknown"})
	}
	if _, err := next.spawn(context.Background(), 2, executable, root, "review", "work", time.Minute); err == nil {
		t.Fatal("retention cap bypassed")
	}
}

func TestChildSaveFailuresPreventEffectsAndHideUnsavedResult(t *testing.T) {
	root := t.TempDir()
	executable := childTestExecutable(t, root, "touch unexpected\nwhile [ ! -f release ]; do sleep 0.01; done\nprintf done")
	r, _ := openChildRegistry("")
	r.path = filepath.Join(root, "blocked", "journal")
	if err := os.WriteFile(filepath.Join(root, "blocked"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.spawn(context.Background(), 1, executable, root, "review", "work", time.Minute); err == nil {
		t.Fatal("admission save failure accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "unexpected")); !os.IsNotExist(err) {
		t.Fatal("process started before durable admission")
	}

	r, _ = openChildRegistry(filepath.Join(root, "session"))
	t.Cleanup(r.stop)
	record, err := r.spawn(context.Background(), 1, executable, root, "review", "work", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(r.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(r.path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	got := awaitChild(t, r, record.ID)
	if got.Status != "unknown" || len(got.Result) != 0 {
		t.Fatalf("unsaved result acknowledged: %#v", got)
	}
	if _, err := r.spawn(context.Background(), 1, executable, root, "review", "work", time.Minute); err == nil {
		t.Fatal("admitted after result save failure")
	}
}

func TestPythonChildrenCrossCellWaitAndCancelledWaiter(t *testing.T) {
	a, sess := pythonTestAgent(t)
	a.executable = childTestExecutable(t, sess.CWD, "touch started\nwhile [ ! -f release ]; do sleep 0.01; done\nprintf done")
	a.customAgents = map[string]customAgent{"review": {Name: "review"}}
	got := pythonCell(t, a, sess, "child = await mai.spawn('review', 'work')\nraise ValueError('retained')")
	if got.OK || got.StateLost {
		t.Fatalf("exception state: %#v", got)
	}
	got = pythonCell(t, a, sess, `import asyncio
try:
    await asyncio.wait_for(child.wait(), 0.05)
except asyncio.TimeoutError:
    pass
assert (await child.status())['status'] == 'running'
for _ in range(80):
    assert (await child.status())['status'] == 'running'
`)
	if !got.OK {
		t.Fatalf("waiter cancellation/status quota: %#v", got)
	}
	if err := os.WriteFile(filepath.Join(sess.CWD, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	got = pythonCell(t, a, sess, "result = await child.wait()\nassert result['output'] == 'done'\nassert await child.wait() == result")
	if !got.OK {
		t.Fatalf("cross-cell repeated wait: %#v", got)
	}
}

func TestChildTimeoutAndParentCancellation(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		name := "parent cancellation"
		if timeout {
			name = "child timeout"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			r, _ := openChildRegistry("")
			t.Cleanup(r.stop)
			executable := childTestExecutable(t, root, "sleep 30")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			limit := time.Minute
			if timeout {
				limit = 50 * time.Millisecond
			}
			record, err := r.spawn(ctx, 1, executable, root, "review", "work", limit)
			if err != nil {
				t.Fatal(err)
			}
			if !timeout {
				cancel()
			}

			got := awaitChild(t, r, record.ID)
			expected := "cancelled"
			if timeout {
				expected = "timed_out"
			}
			if got.Status != expected {
				t.Fatalf("lifetime result: %#v", got)
			}
		})
	}
}

func TestPythonIdleKernelDeathCancelsChildren(t *testing.T) {
	a, sess := pythonTestAgent(t)
	a.executable = childTestExecutable(t, sess.CWD, "sleep 30")
	a.customAgents = map[string]customAgent{"review": {Name: "review"}}
	if got := pythonCell(t, a, sess, "child = await mai.spawn('review', 'work')"); !got.OK {
		t.Fatalf("admission: %#v", got)
	}
	if err := a.python.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.python.wait:
	case <-time.After(5 * time.Second):
		t.Fatal("idle kernel death did not reap children")
	}

	records := a.children.nextGeneration().recovered
	if len(records) != 1 || records[0].Status != "cancelled" {
		t.Fatalf("child survived kernel loss: %#v", records)
	}
}
