package mai

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPythonHostHandlersPreservePolicyAndResults(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.RepoRoot = sess.CWD
	result := pythonCell(t, a, sess, `result = await mai.bash('printf error >&2; exit 7')
assert not result['ok'] and result['exit_code'] == 7
assert result['stderr'] == 'error'`)
	if !result.OK || len(result.Activities) != 1 || result.Activities[0].Status != "failed" {
		t.Fatalf("host error result: %#v", result)
	}

	patch := "*** Begin Patch\n*** Add File: created\n+host patch\n*** End Patch"
	result = pythonCell(t, a, sess, fmt.Sprintf("await mai.apply_patch(%q)", patch))
	if !result.OK {
		t.Fatalf("host patch: %#v", result)
	}
	content, err := os.ReadFile(filepath.Join(sess.CWD, "created"))
	if err != nil || string(content) != "host patch\n" {
		t.Fatalf("patch effect: %q, %v", content, err)
	}

	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	result = pythonCell(t, a, sess, fmt.Sprintf("result = await mai.bash(%q)\nassert 'approval' in result['error']", "rm "+outside))
	if !result.OK {
		t.Fatalf("approval boundary: %#v", result)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("rejected operation changed file: %v", err)
	}

	a.customAgent = &customAgent{Name: "child"}
	result = pythonCell(t, a, sess, "result = await mai.spawn_subagent('other', 'work')\nassert 'cannot spawn' in result['error']")
	if !result.OK || result.Activities[0].Status != "failed" {
		t.Fatalf("child restriction: %#v", result)
	}

	raw, err := a.pythonHost(sess, "outer")(context.Background(), 1, 99, 1, "python", json.RawMessage(`{"code":"1"}`))
	if err != nil || !strings.Contains(string(raw), "not available through the bridge") || sess.PythonActivities[len(sess.PythonActivities)-1].Status != "rejected" {
		t.Fatalf("recursive Python was accepted: %s, %v", raw, err)
	}
}

func TestPythonBoundsConcurrentHostRequests(t *testing.T) {
	a, sess := pythonTestAgent(t)
	result := pythonCell(t, a, sess, `import asyncio
await asyncio.gather(*(mai.bash('sleep 0.01') for _ in range(9)))`)
	if result.OK || result.StateLost || !strings.Contains(result.Stderr, "host call limit") || len(result.Activities) > maxPythonPending {
		t.Fatalf("pending host bound: %#v", result)
	}

	if result := pythonCell(t, a, sess, "await mai.bash('printf recovered')"); !result.OK || !strings.Contains(result.Stdout, "recovered") {
		t.Fatalf("kernel failed after bounded rejection: %#v", result)
	}
}

func TestPythonBoundsHostCallsPerCell(t *testing.T) {
	a, sess := pythonTestAgent(t)
	result := pythonCell(t, a, sess, "for i in range(65):\n    await mai.bash('')")
	if result.OK || result.StateLost || !strings.Contains(result.Stderr, "host call limit") || len(result.Activities) != maxPythonCalls {
		t.Fatalf("per-cell host bound: %#v", result)
	}

	result = pythonCell(t, a, sess, "await mai.bash('')")
	if !result.OK || len(result.Activities) != 1 || result.Activities[0].Call != 1 {
		t.Fatalf("host call budget did not reset: %#v", result)
	}
}

func TestPythonStaleCallbackCannotUseLaterCellAuthority(t *testing.T) {
	a, sess := pythonTestAgent(t)
	result := pythonCell(t, a, sess, `import asyncio
stale = []
async def old_call():
    try:
        await mai.bash('printf unexpected')
    except RuntimeError:
        stale.append('rejected')
asyncio.get_running_loop().call_later(0.05, lambda: asyncio.create_task(old_call()))
await asyncio.sleep(0)`)
	if !result.OK {
		t.Fatal(result)
	}

	result = pythonCell(t, a, sess, "await asyncio.sleep(0.1)\nassert stale == ['rejected']")
	if !result.OK || len(result.Activities) != 0 {
		t.Fatalf("stale callback gained host authority: %#v", result)
	}
}

func TestPythonRejectsInvalidControlFrames(t *testing.T) {
	for name, frame := range map[string]string{
		"malformed": `{"type":"done","generation":1,"cell":1,"ok":"yes"}`,
		"stale":     `{"type":"done","generation":1,"cell":999,"ok":true}`,
		"extra":     `{"type":"done","generation":1,"cell":1,"ok":true,"extra":true}`,
		"oversized": strings.Repeat("x", maxPythonFrame+1),
	} {
		t.Run(name, func(t *testing.T) {
			a, sess := pythonTestAgent(t)
			code := fmt.Sprintf("import os\nos.write(4, %q.encode())", frame+"\n")
			if name == "oversized" {
				code = fmt.Sprintf("import os\nos.write(4, b'x' * %d)", maxPythonFrame+1)
			}
			result := pythonCell(t, a, sess, code)
			if result.OK || !result.StateLost {
				t.Fatalf("invalid frame accepted: %#v", result)
			}

			result = pythonCell(t, a, sess, "1")
			if !result.OK || !result.Fresh {
				t.Fatalf("new kernel after protocol failure: %#v", result)
			}
		})
	}
}

func TestPythonHostJournalRecoveryAndSummaryBounds(t *testing.T) {
	activities := []pythonActivity{
		{OuterCallID: "outer", Generation: 1, Cell: 2, Call: 1, Name: "bash", Status: "completed", Arguments: json.RawMessage(`{"command":"first"}`), Result: json.RawMessage(`{"ok":true}`)},
		{OuterCallID: "outer", Generation: 1, Cell: 2, Call: 2, Name: "bash", Status: "pending", Arguments: json.RawMessage(`{"command":"second"}`)},
	}
	sess := &session{History: []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"outer","name":"python","arguments":"{}"}`)}, PythonActivities: activities}
	if err := repairInterruptedToolCalls(sess); err != nil {
		t.Fatal(err)
	}
	if len(sess.History) != 2 || len(sess.PythonActivities) != 0 || !strings.Contains(string(sess.History[1]), "unknown") || !strings.Contains(string(sess.History[1]), "activities") {
		t.Fatalf("nested recovery: %#v", sess)
	}

	if err := repairInterruptedToolCalls(sess); err != nil || len(sess.History) != 2 {
		t.Fatalf("recovery replayed an entry: %v, %#v", err, sess.History)
	}

	for i := range 64 {
		activities = append(activities, pythonActivity{Call: i + 3, Name: "bash", Status: "completed", Arguments: json.RawMessage(strings.Repeat("x", 1<<20)), Result: json.RawMessage(strings.Repeat("y", 1<<20))})
	}
	summaries, _ := summarizePythonActivities(activities)
	data, err := json.Marshal(summaries)
	if err != nil || len(data) > (32<<10)+2 || !strings.Contains(string(data), "unknown") {
		t.Fatalf("activity summaries exceed bound or hide unknown: %d, %v", len(data), err)
	}
}

func TestPythonHostJournalSavesBeforeEffects(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.RepoRoot = sess.CWD
	blocker := filepath.Join(sess.CWD, "blocked")
	if err := os.WriteFile(blocker, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	a.sessionPath = filepath.Join(blocker, "session.json")
	result := pythonCell(t, a, sess, "await mai.bash('touch unexpected')")
	if result.OK || !result.StateLost || !strings.Contains(result.Error, "before dispatch") {
		t.Fatalf("journal failure result: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(sess.CWD, "unexpected")); !os.IsNotExist(err) {
		t.Fatalf("host effect ran without journal: %v", err)
	}
}

func TestPythonHostOutputTransfersJournalIntoOuterSummary(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.RepoRoot = sess.CWD
	a.sessionPath = filepath.Join(sess.CWD, "session.json")
	sess.History = []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"outer","name":"python","arguments":"{}"}`)}
	err := a.executeCalls(context.Background(), sess, []functionCall{{Name: "python", CallID: "outer", Arguments: `{"code":"result = await mai.bash('printf host')\n42"}`}})
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(a.sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	var saved session
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if len(saved.PythonActivities) != 0 || len(saved.History) != 2 || historyItemType(saved.History[1]) != "function_call_output" {
		t.Fatalf("journal was retained or nested model calls were fabricated: %#v", saved)
	}
	var envelope struct {
		Output string `json:"output"`
	}
	if err := json.Unmarshal(saved.History[1], &envelope); err != nil {
		t.Fatal(err)
	}
	var result pythonResult
	if err := json.Unmarshal([]byte(envelope.Output), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.Stdout != "42\n" || len(result.Activities) != 1 || result.Activities[0].Status != "completed" {
		t.Fatalf("outer Python summary: %#v", result)
	}
}

func TestPythonHostJournalCompletionFailureStaysUnknown(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.RepoRoot = sess.CWD
	a.sessionPath = filepath.Join(sess.CWD, "journal", "session.json")
	result := pythonCell(t, a, sess, "await mai.bash('mv journal saved-journal; printf blocked > journal')")
	if result.OK || !result.StateLost || len(result.Activities) != 1 || result.Activities[0].Status != "unknown" {
		t.Fatalf("completion save was acknowledged: %#v", result)
	}
	data, err := os.ReadFile(filepath.Join(sess.CWD, "saved-journal", "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved session
	if err := json.Unmarshal(data, &saved); err != nil || len(saved.PythonActivities) != 1 || saved.PythonActivities[0].Status != "pending" {
		t.Fatalf("durable pending entry was lost: %#v, %v", saved, err)
	}
}

func TestPythonCancellationDuringHostApproval(t *testing.T) {
	a, sess := pythonTestAgent(t)
	sess.RepoRoot = sess.CWD
	outside := filepath.Join(t.TempDir(), "keep")
	if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	a.approve = func(ctx context.Context, command, reason string) (bool, error) {
		close(started)
		<-ctx.Done()
		return true, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan pythonResult, 1)
	go func() {
		code := fmt.Sprintf("await mai.bash(%q)", "rm "+outside)
		done <- a.python.execute(ctx, sess.CWD, code, false, 5*time.Second, a.pythonHost(sess, "outer"))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("approval did not start")
	}
	cancel()
	select {
	case result := <-done:
		if result.OK || !result.StateLost || sess.PythonActivities[0].Status != "unknown" {
			t.Fatalf("cancelled host result: %#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled host operation did not terminate")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("cancelled approval allowed a late effect: %v", err)
	}
}
