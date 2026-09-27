package mai

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExemptCallsAreSortedAndNeverNil(t *testing.T) {
	operations := map[string]pythonOperation{
		"zulu":   {},
		"alpha":  {},
		"effect": {budgeted: true},
	}
	got := exemptCalls(operations)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zulu" {
		t.Fatalf("exempt calls = %v", got)
	}
	if got := exemptCalls(map[string]pythonOperation{"effect": {budgeted: true}}); got == nil || len(got) != 0 {
		t.Fatalf("exempt calls should be an empty non-nil slice, got %v", got)
	}
}

func TestPythonHostCallsEnqueueRejectsInvalidCalls(t *testing.T) {
	operations := map[string]pythonOperation{
		"effect": {budgeted: true},
		"exempt": {},
	}
	args := json.RawMessage(`{}`)

	calls := &pythonHostCalls{operations: operations}
	if err := calls.enqueue(pythonFrame{Call: 1, Name: "effect", Arguments: args}); err != nil {
		t.Fatal(err)
	}
	if calls.lastCall != 1 || len(calls.queued) != 1 || calls.effectCalls != 1 {
		t.Fatalf("valid call was not queued: %#v", calls)
	}

	for name, frame := range map[string]pythonFrame{
		"out of order":    {Call: 3, Name: "effect", Arguments: args},
		"unknown name":    {Call: 2, Name: "missing", Arguments: args},
		"empty args":      {Call: 2, Name: "effect"},
		"non-object args": {Call: 2, Name: "effect", Arguments: json.RawMessage(`[1]`)},
	} {
		t.Run(name, func(t *testing.T) {
			fresh := &pythonHostCalls{operations: operations, lastCall: 1}
			if err := fresh.enqueue(frame); err == nil || !strings.Contains(err.Error(), "invalid or excessive Python host call") {
				t.Fatalf("enqueue accepted %s: %v", name, err)
			}
			if len(fresh.queued) != 0 {
				t.Fatalf("rejected %s was queued: %#v", name, fresh.queued)
			}
		})
	}
}

func TestPythonHostCallsEnqueueBoundsPending(t *testing.T) {
	args := json.RawMessage(`{}`)
	for _, withActive := range []bool{false, true} {
		calls := &pythonHostCalls{operations: map[string]pythonOperation{"effect": {budgeted: true}}}
		if withActive {
			calls.active = make(chan pythonHostResult, 1)
		}
		pending := maxPythonPending
		if withActive {
			pending--
		}
		for i := 1; i <= pending; i++ {
			if err := calls.enqueue(pythonFrame{Call: i, Name: "effect", Arguments: args}); err != nil {
				t.Fatalf("call %d rejected with active=%t: %v", i, withActive, err)
			}
		}
		if err := calls.enqueue(pythonFrame{Call: pending + 1, Name: "effect", Arguments: args}); err == nil {
			t.Fatalf("pending host calls were not bounded with active=%t", withActive)
		}
	}
}

func TestPythonHostCallsEnqueueBoundsEffectCalls(t *testing.T) {
	operations := map[string]pythonOperation{
		"effect": {budgeted: true},
		"exempt": {},
	}
	calls := &pythonHostCalls{operations: operations}
	args := json.RawMessage(`{}`)
	call := 0
	enqueue := func(name string) error {
		call++
		err := calls.enqueue(pythonFrame{Call: call, Name: name, Arguments: args})
		calls.queued = nil
		return err
	}
	for i := 0; i < maxPythonCalls; i++ {
		if err := enqueue("effect"); err != nil {
			t.Fatalf("budgeted call %d rejected: %v", i+1, err)
		}
	}
	for i := 0; i < maxPythonCalls; i++ {
		if err := enqueue("exempt"); err != nil {
			t.Fatalf("exempt call %d consumed the effect budget: %v", i+1, err)
		}
	}
	if err := enqueue("effect"); err == nil || !strings.Contains(err.Error(), "invalid or excessive Python host call") {
		t.Fatalf("effect calls were not bounded: %v", err)
	}
}
