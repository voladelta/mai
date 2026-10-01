package mai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	maxPythonFrame   = 1 << 20
	maxPythonPending = 8
	maxPythonCalls   = 64
)

type pythonRuntime struct {
	Version    string `json:"version"`
	Executable string `json:"executable"`
	GILEnabled bool   `json:"gil_enabled"`
}

type pythonFrame struct {
	Type       string          `json:"type"`
	Generation int             `json:"generation"`
	Cell       int             `json:"cell,omitempty"`
	Call       int             `json:"call,omitempty"`
	Name       string          `json:"name,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	OK         *bool           `json:"ok,omitempty"`
	Version    string          `json:"version,omitempty"`
	Executable string          `json:"executable,omitempty"`
	GILEnabled *bool           `json:"gil_enabled,omitempty"`
}

type pythonMessage struct {
	frame pythonFrame
	err   error
}

type pythonHostHandler func(context.Context, int, int, int, string, json.RawMessage) (json.RawMessage, error)

type pythonOperation struct {
	budgeted bool
	handler  pythonHostHandler
}

func decodePythonFrame(data []byte) (pythonFrame, error) {
	var frame pythonFrame
	if len(data)+1 > maxPythonFrame {
		return frame, errors.New("Python control frame exceeds 1 MiB")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return frame, err
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		return frame, err
	}
	var required []string
	switch frame.Type {
	case "ready":
		required = []string{"type", "generation", "version", "executable", "gil_enabled"}
	case "host_call":
		required = []string{"type", "generation", "cell", "call", "name", "arguments"}
	case "done":
		required = []string{"type", "generation", "cell", "ok"}
	default:
		return frame, errors.New("unknown Python frame type")
	}
	if len(fields) != len(required) {
		return frame, errors.New("invalid Python frame fields")
	}
	for _, name := range required {
		if raw, ok := fields[name]; !ok || bytes.Equal(raw, []byte("null")) {
			return frame, fmt.Errorf("missing Python frame field %s", name)
		}
	}
	return frame, nil
}

func readPythonFrames(input io.Reader, messages chan<- pythonMessage, stopped <-chan struct{}) {
	defer close(messages)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), maxPythonFrame)
	scanner.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if index := bytes.IndexByte(data, '\n'); index >= 0 {
			return index + 1, data[:index], nil
		}
		if atEOF && len(data) > 0 {
			return 0, nil, io.ErrUnexpectedEOF
		}
		return 0, nil, nil
	})
	for scanner.Scan() {
		frame, err := decodePythonFrame(scanner.Bytes())
		select {
		case messages <- pythonMessage{frame: frame, err: err}:
		case <-stopped:
			return
		}
		if err != nil {
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case messages <- pythonMessage{err: err}:
	case <-stopped:
	}
}

func (k *pythonKernel) writeFrame(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data)+1 > maxPythonFrame {
		return errors.New("Python control frame exceeds 1 MiB")
	}
	_, err = k.request.Write(append(data, '\n'))
	return err
}

func (k *pythonKernel) awaitReady(ctx context.Context) error {
	select {
	case message, open := <-k.messages:
		frame := message.frame
		if !open || message.err != nil {
			return fmt.Errorf("Python startup protocol: %v", message.err)
		}
		if frame.Type != "ready" || frame.Generation != k.generation || frame.Version == "" || frame.Executable == "" || frame.GILEnabled == nil {
			return errors.New("invalid Python ready message")
		}
		k.runtime = &pythonRuntime{frame.Version, frame.Executable, *frame.GILEnabled}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func exemptCalls(operations map[string]pythonOperation) []string {
	exempt := []string{}
	for name, operation := range operations {
		if !operation.budgeted {
			exempt = append(exempt, name)
		}
	}
	sort.Strings(exempt)
	return exempt
}

type pythonHostResult struct {
	frame  pythonFrame
	result json.RawMessage
	err    error
}

type pythonHostCalls struct {
	operations  map[string]pythonOperation
	active      chan pythonHostResult
	queued      []pythonFrame
	lastCall    int
	effectCalls int
}

func (h *pythonHostCalls) enqueue(frame pythonFrame) error {
	count := len(h.queued)
	if h.active != nil {
		count++
	}
	operation, known := h.operations[frame.Name]
	if known && operation.budgeted {
		h.effectCalls++
	}
	if frame.Call != h.lastCall+1 || !known || h.effectCalls > maxPythonCalls || count >= maxPythonPending || len(frame.Arguments) == 0 || frame.Arguments[0] != '{' {
		return errors.New("invalid or excessive Python host call")
	}
	h.lastCall = frame.Call
	h.queued = append(h.queued, frame)
	return nil
}

func (h *pythonHostCalls) startNext(ctx context.Context) {
	if h.active != nil || len(h.queued) == 0 {
		return
	}
	frame := h.queued[0]
	h.queued = h.queued[1:]
	h.active = make(chan pythonHostResult, 1)
	resultChan := h.active
	go func() {
		operation := h.operations[frame.Name]
		result, err := operation.handler(ctx, frame.Generation, frame.Cell, frame.Call, frame.Name, frame.Arguments)
		resultChan <- pythonHostResult{frame, result, err}
	}()
}

func (k *pythonKernel) handleControl(message pythonMessage, open bool, calls *pythonHostCalls) (finished, ok bool, err error) {
	if !open {
		return false, false, io.EOF
	}
	if message.err != nil {
		return false, false, message.err
	}
	frame := message.frame
	if frame.Generation != k.generation || frame.Cell != k.cell {
		return false, false, errors.New("stale Python control message")
	}
	switch frame.Type {
	case "host_call":
		if err := calls.enqueue(frame); err != nil {
			return false, false, err
		}
	case "done":
		if frame.OK == nil || calls.active != nil || len(calls.queued) != 0 {
			return false, false, errors.New("Python cell ended with outstanding host operations")
		}
		return true, *frame.OK, nil
	default:
		return false, false, errors.New("unexpected Python control message")
	}
	return false, false, nil
}

func (k *pythonKernel) runProtocol(ctx context.Context, code, marker string, operations map[string]pythonOperation) (bool, error) {
	if k.runtime == nil {
		if err := k.awaitReady(ctx); err != nil {
			return false, err
		}
	}
	if err := k.writeFrame(map[string]any{"type": "execute", "generation": k.generation, "cell": k.cell, "code": code, "marker": marker, "exempt_calls": exemptCalls(operations)}); err != nil {
		return false, err
	}

	hostCtx, cancel := context.WithCancel(ctx)
	calls := &pythonHostCalls{operations: operations}
	defer func() {
		cancel()
		// No host operation can outlive this cell, including a cancelled
		// Python awaiter or an operation that is finishing an atomic write.
		if calls.active != nil {
			<-calls.active
		}
	}()
	for {
		calls.startNext(hostCtx)
		select {
		case reply := <-calls.active:
			calls.active = nil
			if reply.err != nil {
				return false, reply.err
			}
			if err := k.writeFrame(map[string]any{"type": "host_reply", "generation": k.generation, "cell": k.cell, "call": reply.frame.Call, "result": reply.result}); err != nil {
				return false, err
			}
		case message, open := <-k.messages:
			finished, ok, err := k.handleControl(message, open, calls)
			if err != nil {
				return false, err
			}
			if finished {
				return ok, nil
			}
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}
