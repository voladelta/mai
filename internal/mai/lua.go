package mai

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	lua "github.com/Shopify/go-lua"
)

const (
	luaCellTimeout = defaultBashTimeout
	// luaHookInterval is how many VM instructions pass between checks for
	// cancellation and heap growth.
	luaHookInterval = 10_000
	luaMaxDepth     = 24
)

// luaMemoryLimit caps heap growth during one cell; go-lua has no allocator limit.
var luaMemoryLimit uint64 = 1 << 30

type luaResult struct {
	OK         bool   `json:"ok"`
	Result     string `json:"result,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Error      string `json:"error,omitempty"`
	Fresh      bool   `json:"fresh"`
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// luaCell is the state of the cell currently executing.
type luaCell struct {
	ctx    context.Context
	sess   *session
	callID string // excluded from history search so a cell cannot match itself
	stdout *cappedBuffer
	heap0  uint64
}

// luaKernel keeps one Lua state across cells, so globals, functions and
// tables persist. Libraries that reach the OS are not opened; files and
// processes go through the guarded mai table.
type luaKernel struct {
	mu       sync.Mutex
	agent    *agent
	state    *lua.State
	cell     *luaCell
	cells    int
	reserved map[string]bool
	// csvColumns is the header of the last csv.decode(text, true), used to keep
	// column order when csv.encode gets rows built from it.
	csvColumns []string
}

func newLuaKernel(a *agent) *luaKernel {
	k := &luaKernel{agent: a, state: lua.NewState()}
	l := k.state
	for _, lib := range []lua.RegistryFunction{
		{Name: "_G", Function: lua.BaseOpen},
		{Name: "table", Function: lua.TableOpen},
		{Name: "string", Function: lua.StringOpen},
		{Name: "math", Function: lua.MathOpen},
		{Name: "bit32", Function: lua.Bit32Open},
		{Name: "os", Function: lua.OSOpen},
	} {
		lua.Require(l, lib.Name, lib.Function, true)
		l.Pop(1)
	}
	luaStringMatching(l)
	l.Global("os")
	l.PushGoFunction(luaOSDate)
	l.SetField(-2, "date")
	l.Pop(1)
	l.Global("table")
	l.Field(-1, "unpack")
	l.SetGlobal("unpack")
	l.Pop(1)
	l.Register("print", k.print)
	k.stubUnavailable()
	ioFuncs := map[string]lua.Function{"write": k.ioWrite}
	for name, advice := range map[string]string{
		"read": luaFileAdvice, "input": luaFileAdvice, "close": luaFileAdvice, "output": "use print(...) or io.write(...)",
	} {
		ioFuncs[name] = luaUnavailable("io."+name, advice)
	}
	k.library("io", ioFuncs)
	k.library("json", map[string]lua.Function{"decode": k.jsonDecode, "encode": k.jsonEncode})
	k.library("csv", map[string]lua.Function{"decode": k.csvDecode, "encode": k.csvEncode})
	k.library("mai", map[string]lua.Function{
		"bash": k.maiBash, "read": k.maiFile("read"), "write": k.maiFile("write"), "edit": k.maiFile("edit"),
		"read_text": k.maiReadText, "history": k.maiHistory,
	})
	if err := lua.DoString(l, luaPrelude); err != nil {
		panic("lua prelude: " + err.Error())
	}
	lua.SetDebugHook(l, k.checkLimits, lua.MaskCount, luaHookInterval)

	// Names present now must not be turned into globals by the local rewrite.
	k.reserved = map[string]bool{}
	l.PushGlobalTable()
	l.PushNil()
	for l.Next(-2) {
		if name, ok := l.ToString(-2); ok {
			k.reserved[name] = true
		}
		l.Pop(1)
	}
	l.Pop(1)
	return k
}

// luaPrelude adds the parts of the standard library scripts reach for that can
// be built safely on mai: read-only io.open/io.lines over repository files,
// io.popen over mai.bash, and a tonumber that ignores an invalid base. The
// latter matters because tonumber(s:gsub(...)) passes gsub's replacement count
// as the base, which is a common slip.
const luaPrelude = `
do
  local read_text, bash = mai.read_text, mai.bash
  local function stream(text)
    local pos, f = 1, {}
    function f:read(...)
      local formats, out = {...}, {}
      if #formats == 0 then formats[1] = "*l" end
      for i, fmt in ipairs(formats) do
        local value
        if type(fmt) == "number" then
          if pos <= #text or fmt == 0 then value = text:sub(pos, pos + fmt - 1); pos = pos + fmt end
          if value == "" and fmt > 0 then value = nil end
        else
          fmt = tostring(fmt):gsub("^%*", "")
          if fmt == "a" then
            value = text:sub(pos); pos = #text + 1
          elseif fmt == "l" or fmt == "L" then
            if pos <= #text then
              local e = text:find("\n", pos, true)
              if e then value = text:sub(pos, fmt == "L" and e or e - 1); pos = e + 1
              else value = text:sub(pos); pos = #text + 1 end
            end
          elseif fmt == "n" then
            local num = text:match("^%s*([+-]?%d+%.?%d*[eE]?[+-]?%d*)", pos)
            if num then value = tonumber(num); pos = text:find(num, pos, true) + #num end
          else
            error("bad argument to 'read' (invalid format)", 2)
          end
        end
        out[i] = value
        if value == nil then break end
      end
      return table.unpack(out, 1, #formats)
    end
    function f:lines() return function() return self:read("*l") end end
    function f:close() return true end
    function f:seek() return pos - 1 end
    return f
  end
  io.open = function(path, mode)
    mode = mode or "r"
    if mode:sub(1, 1) ~= "r" or mode:find("+", 1, true) then
      error("io.open: only read mode is available; write files with mai.write or mai.edit", 2)
    end
    local ok, text = pcall(read_text, path)
    if not ok then return nil, tostring(text):gsub("^[^:]*:%d+: ", ""), 2 end
    return stream(text)
  end
  io.lines = function(path)
    return assert(io.open(path)):lines()
  end
  io.popen = function(command, mode)
    if mode and mode ~= "r" then error("io.popen: only read mode is available", 2) end
    return stream(bash(command).stdout or "")
  end
  local plain_tonumber = tonumber
  tonumber = function(value, base)
    if base == nil or type(base) ~= "number" or base < 2 or base > 36 then return plain_tonumber(value) end
    return plain_tonumber(value, base)
  end
end
`

const (
	luaFileAdvice  = "read files with mai.read_text(path), and write with mai.write or mai.edit"
	luaShellAdvice = "run commands with mai.bash(command)"
)

// luaUnavailable builds a function for a withheld standard function that fails
// with the alternative, since "attempt to call a nil value" teaches nothing.
func luaUnavailable(name, advice string) lua.Function {
	return func(l *lua.State) int {
		lua.Errorf(l, "%s is not available; %s", name, advice)
		return 0
	}
}

// stubUnavailable installs the stubs for os and for the base functions.
func (k *luaKernel) stubUnavailable() {
	l := k.state
	for _, f := range []struct{ name, advice string }{
		{"require", "the standard libraries, json and mai are already loaded"},
		{"dofile", luaFileAdvice}, {"loadfile", luaFileAdvice}, {"loadstring", "use load(code)"},
	} {
		l.Register(f.name, luaUnavailable(f.name, f.advice))
	}
	l.Global("os")
	for _, f := range []struct{ name, advice string }{
		{"execute", luaShellAdvice}, {"remove", `delete files with mai.bash("rm ...")`}, {"rename", `move files with mai.bash("mv ...")`},
		{"exit", "end the cell with return"}, {"getenv", `read the environment with mai.bash("printenv NAME")`},
		{"tmpname", "write temporary files inside the repository with mai.write"}, {"setlocale", "it has no effect here"},
	} {
		l.PushGoFunction(luaUnavailable("os."+f.name, f.advice))
		l.SetField(-2, f.name)
	}
	l.Pop(1)
}

func (k *luaKernel) library(name string, funcs map[string]lua.Function) {
	l := k.state
	l.NewTable()
	names := make([]string, 0, len(funcs))
	for fname := range funcs {
		names = append(names, fname)
	}
	sort.Strings(names)
	for _, fname := range names {
		l.PushGoFunction(funcs[fname])
		l.SetField(-2, fname)
	}
	l.SetGlobal(name)
}

func (a *agent) executeLua(ctx context.Context, sess *session, arguments, callID string) json.RawMessage {
	var args struct {
		Code  string `json:"code"`
		Reset *bool  `json:"reset"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return textToolOutput(toolError("invalid lua arguments", err))
	}
	hasCode, hasReset := strings.TrimSpace(args.Code) != "", args.Reset != nil
	if hasCode == hasReset || (hasReset && !*args.Reset) {
		return textToolOutput(toolError("invalid lua arguments", errors.New("provide exactly one of code or reset:true")))
	}
	if hasReset {
		fmt.Fprintln(a.stderr, "→ lua: reset")
		a.lua = nil
		return textToolOutput(marshalToolResult(map[string]any{"ok": true, "reset": true}))
	}
	fmt.Fprintf(a.stderr, "→ lua: %s\n", oneLine(args.Code, 180))
	if a.lua == nil {
		a.lua = newLuaKernel(a)
	}
	return textToolOutput(marshalToolResult(a.lua.run(ctx, sess, callID, args.Code)))
}

func (k *luaKernel) run(parent context.Context, sess *session, callID, code string) luaResult {
	k.mu.Lock()
	defer k.mu.Unlock()
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, luaCellTimeout)
	defer cancel()

	cell := &luaCell{ctx: ctx, sess: sess, callID: callID, stdout: &cappedBuffer{max: maxToolStreamBytes}, heap0: heapBytes()}
	result := luaResult{Fresh: k.cells == 0}
	k.cells++
	k.cell = cell
	value, err := k.execute(code)
	k.cell = nil

	result.OK = err == nil
	if err != nil {
		result.Error = err.Error() + k.errorHint(err.Error(), code)
	} else {
		result.Result = value
	}
	result.Stdout = cell.stdout.String()
	result.Truncated = cell.stdout.Truncated()
	result.DurationMS = time.Since(started).Milliseconds()
	return result
}

// execute runs one cell and returns its rendered return values. Like a REPL,
// it first tries the cell as an expression so `x + 1` shows its value.
func (k *luaKernel) execute(code string) (value string, err error) {
	l := k.state
	top := l.Top()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("lua panic: %v", r)
		}
		l.SetTop(top)
	}()
	if err := k.cell.ctx.Err(); err != nil {
		return "", err
	}
	name := fmt.Sprintf("=cell%d", k.cells)
	source := luaPersistLocals(code, k.reserved)
	if lua.LoadBuffer(l, "return "+source, name, "") != nil {
		l.SetTop(top)
		if err := lua.LoadBuffer(l, source, name, ""); err != nil {
			if message, ok := l.ToString(-1); ok {
				return "", errors.New(message)
			}
			return "", err
		}
	}
	if err := l.ProtectedCall(0, lua.MultipleReturns, 0); err != nil {
		return "", err
	}
	var values []any
	for i := top + 1; i <= l.Top(); i++ {
		values = append(values, luaToGo(l, i, 0))
	}
	switch len(values) {
	case 0:
		return "", nil
	case 1:
		if values[0] == nil {
			return "", nil
		}
		return renderLuaValue(values[0]), nil
	}
	return renderLuaValue(values), nil
}

func renderLuaValue(v any) string {
	encoded, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	if len(encoded) > maxToolStreamBytes {
		return string(encoded[:maxToolStreamBytes]) + fmt.Sprintf("... %d bytes omitted", len(encoded)-maxToolStreamBytes)
	}
	return string(encoded)
}

// checkLimits runs every luaHookInterval instructions: it aborts the cell on
// cancellation, timeout, or runaway heap growth.
func (k *luaKernel) checkLimits(l *lua.State, _ lua.Debug) {
	cell := k.cell
	if cell == nil {
		return
	}
	// Raise errors directly: lua.Errorf adds a source position, which a hook has none of.
	stop := func(message string) {
		l.PushString("cell stopped: " + message)
		l.Error()
	}
	if err := cell.ctx.Err(); err != nil {
		stop(err.Error())
	}
	if heap := heapBytes(); heap > cell.heap0 && heap-cell.heap0 > luaMemoryLimit {
		stop(fmt.Sprintf("memory limit exceeded (%d MiB)", luaMemoryLimit>>20))
	}
}

func heapBytes() uint64 {
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	return sample[0].Value.Uint64()
}

var (
	luaLocalFunction = regexp.MustCompile(`^local\s+function\s+([A-Za-z_]\w*)`)
	luaLocalNames    = regexp.MustCompile(`^local\s+([A-Za-z_]\w*(?:\s*,\s*[A-Za-z_]\w*)*)\s*(=.*)?$`)
	luaLongOpen      = regexp.MustCompile(`\[(=*)\[`)
)

// luaPersistLocals turns column-0 `local` declarations into globals so that
// state survives across cells, as it does in a REPL session. Declarations of
// standard names stay local, and nested (indented) locals are untouched.
func luaPersistLocals(code string, reserved map[string]bool) string {
	lines := strings.Split(code, "\n")
	longClose := ""
	for i, line := range lines {
		if longClose != "" {
			if strings.Contains(line, longClose) {
				longClose = ""
			}
			continue
		}
		if m := luaLongOpen.FindStringSubmatch(line); m != nil {
			if closer := "]" + m[1] + "]"; !strings.Contains(line[strings.Index(line, m[0])+len(m[0]):], closer) {
				longClose = closer
			}
		}
		if m := luaLocalFunction.FindStringSubmatch(line); m != nil {
			if !reserved[m[1]] {
				lines[i] = strings.TrimPrefix(line, "local ")
			}
			continue
		}
		m := luaLocalNames.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		names := strings.Split(m[1], ",")
		skip := false
		for _, name := range names {
			skip = skip || reserved[strings.TrimSpace(name)]
		}
		if skip {
			continue
		}
		if m[2] == "" {
			lines[i] = m[1] + " = nil"
		} else {
			lines[i] = strings.TrimPrefix(line, "local ")
		}
	}
	return strings.Join(lines, "\n")
}

// luaToGo converts the Lua value at index to JSON-compatible Go values.
func luaToGo(l *lua.State, index, depth int) any {
	switch l.TypeOf(index) {
	case lua.TypeNil, lua.TypeNone:
		return nil
	case lua.TypeBoolean:
		return l.ToBoolean(index)
	case lua.TypeNumber:
		n, _ := l.ToNumber(index)
		return n
	case lua.TypeString:
		s, _ := l.ToString(index)
		return s
	case lua.TypeTable:
		if depth >= luaMaxDepth {
			return "<table: nesting too deep>"
		}
		return luaTableToGo(l, l.AbsIndex(index), depth)
	}
	s, _ := lua.ToStringMeta(l, index)
	l.Pop(1)
	return s
}

func luaTableToGo(l *lua.State, index, depth int) any {
	array := map[int]any{}
	object := map[string]any{}
	isArray := true
	l.PushNil()
	for l.Next(index) {
		value := luaToGo(l, l.AbsIndex(-1), depth+1)
		switch l.TypeOf(-2) {
		case lua.TypeNumber:
			n, _ := l.ToNumber(-2)
			if n == float64(int(n)) && n >= 1 {
				array[int(n)] = value
			} else {
				isArray = false
			}
			object[fmt.Sprint(n)] = value
		case lua.TypeString:
			isArray = false
			key, _ := l.ToString(-2)
			object[key] = value
		default:
			isArray = false
			l.PushValue(-2)
			key, _ := lua.ToStringMeta(l, -1)
			l.Pop(1)
			object[key] = value
		}
		l.Pop(1)
	}
	if isArray {
		out := make([]any, len(array))
		for i := range out {
			value, ok := array[i+1]
			if !ok {
				return object
			}
			out[i] = value
		}
		return out
	}
	return object
}

// luaPush pushes a JSON-compatible Go value as a Lua value.
func luaPush(l *lua.State, v any) {
	switch v := v.(type) {
	case nil:
		l.PushNil()
	case bool:
		l.PushBoolean(v)
	case string:
		l.PushString(v)
	case float64:
		l.PushNumber(v)
	case int:
		l.PushInteger(v)
	case json.Number:
		n, _ := v.Float64()
		l.PushNumber(n)
	case []any:
		l.CreateTable(len(v), 0)
		for i, item := range v {
			luaPush(l, item)
			l.RawSetInt(-2, i+1)
		}
	case map[string]any:
		l.CreateTable(0, len(v))
		for key, item := range v {
			luaPush(l, item)
			l.SetField(-2, key)
		}
	default:
		l.PushString(fmt.Sprint(v))
	}
}

// luaPushStructured converts a tool result (maps with Go ints and structs)
// through JSON, then pushes it.
func luaPushStructured(l *lua.State, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	luaPush(l, decoded)
	return nil
}

func (k *luaKernel) print(l *lua.State) int {
	parts := make([]string, l.Top())
	for i := range parts {
		parts[i], _ = lua.ToStringMeta(l, i+1)
		l.Pop(1)
	}
	_, _ = k.cell.stdout.Write([]byte(strings.Join(parts, "\t") + "\n"))
	return 0
}

func (k *luaKernel) ioWrite(l *lua.State) int {
	for i := 1; i <= l.Top(); i++ {
		if !l.IsString(i) {
			lua.CheckString(l, i) // raises the standard argument error
		}
		s, _ := l.ToString(i)
		_, _ = k.cell.stdout.Write([]byte(s))
	}
	return 0
}

func (k *luaKernel) jsonDecode(l *lua.State) int {
	decoder := json.NewDecoder(strings.NewReader(lua.CheckString(l, 1)))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		lua.Errorf(l, "json.decode: %s", err.Error())
	}
	luaPush(l, value)
	return 1
}

func (k *luaKernel) jsonEncode(l *lua.State) int {
	value := luaToGo(l, 1, 0)
	var encoded []byte
	var err error
	if l.ToBoolean(2) {
		encoded, err = json.MarshalIndent(value, "", "  ")
	} else {
		encoded, err = json.Marshal(value)
	}
	if err != nil {
		lua.Errorf(l, "json.encode: %s", err.Error())
	}
	l.PushString(string(encoded))
	return 1
}

// maiBash is mai.bash(command, timeout_ms); it returns the Bash result table.
func (k *luaKernel) maiBash(l *lua.State) int {
	a := k.agent
	command := lua.CheckString(l, 1)
	output := runBash(k.cell.ctx, bashRequest{
		Command: command, TimeoutMS: lua.OptInteger(l, 2, 0), CWD: k.cell.sess.CWD,
		RepoRoot: k.cell.sess.RepoRoot, Approve: a.approve,
		Depth: a.depth, CaptureRoot: a.captureRoot(),
	})
	var decoded map[string]any
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		lua.Errorf(l, "mai.bash: %s", err.Error())
	}
	luaPush(l, decoded)
	return 1
}

// maiFile exposes the read, write and edit tools. They return structured
// tables with ok; failures are tables with ok = false, not Lua errors.
func (k *luaKernel) maiFile(name string) lua.Function {
	return func(l *lua.State) int {
		request := map[string]any{"file_path": lua.CheckString(l, 1)}
		switch name {
		case "read":
			if !l.IsNoneOrNil(2) {
				request["offset"] = lua.CheckInteger(l, 2)
			}
			if !l.IsNoneOrNil(3) {
				request["limit"] = lua.CheckInteger(l, 3)
			}
		case "write":
			request["content"] = lua.CheckString(l, 2)
		case "edit":
			request["old_string"] = lua.CheckString(l, 2)
			request["new_string"] = lua.CheckString(l, 3)
			request["replace_all"] = l.ToBoolean(4)
		}
		encoded, _ := json.Marshal(request)
		if err := luaPushStructured(l, k.agent.runFileTool(k.cell.ctx, k.cell.sess, name, string(encoded))); err != nil {
			lua.Errorf(l, "mai.%s: %s", name, err.Error())
		}
		return 1
	}
}

// maiReadText is mai.read_text(path): the file's raw text, or a Lua error.
func (k *luaKernel) maiReadText(l *lua.State) int {
	result := k.agent.readRawFile(k.cell.ctx, k.cell.sess, lua.CheckString(l, 1))
	if result["ok"] != true {
		lua.Errorf(l, "%s", fmt.Sprint(result["error"]))
	}
	l.PushString(result["text"].(string))
	return 1
}

// maiHistory is mai.history(query, limit, start).
func (k *luaKernel) maiHistory(l *lua.State) int {
	request := map[string]any{"query": lua.CheckString(l, 1), "limit": lua.OptInteger(l, 2, 20), "start": lua.OptInteger(l, 3, 0)}
	encoded, _ := json.Marshal(request)
	var decoded any
	if err := json.Unmarshal(searchTranscript(k.cell.sess, encoded, k.cell.callID), &decoded); err != nil {
		lua.Errorf(l, "mai.history: %s", err.Error())
	}
	luaPush(l, decoded)
	return 1
}

var (
	luaErrorLine  = regexp.MustCompile(`cell\d+:(\d+):`)
	luaDottedCall = regexp.MustCompile(`[A-Za-z_]\w*(?:\.[A-Za-z_]\w*)+\s*\(`)
	luaMethodCall = regexp.MustCompile(`:([A-Za-z_]\w*)\s*\(`)
)

// errorHint adds one line of guidance to Lua errors that scripts hit often and
// whose standard messages mislead (go-lua reports for-loop internals such as
// '(for state)' as the nil variable).
func (k *luaKernel) errorHint(message, code string) string {
	switch {
	case strings.Contains(message, "attempt to perform arithmetic"):
		return "\nhint: an operand is nil or text. tonumber() returns nil for non-numeric text such as a CSV header row, and a pattern that did not match returns nil; skip the header and check tonumber results. Names shown like '(for state)' are loop internals, not your variable."
	case strings.Contains(message, "attempt to compare") && strings.Contains(message, "'string'") && strings.Contains(message, "'number'"):
		return "\nhint: comparisons do not convert text to numbers (arithmetic does); use tonumber(x) first."
	case strings.Contains(message, "attempt to concatenate"), strings.Contains(message, "attempt to compare"), strings.Contains(message, "attempt to index"), strings.Contains(message, "attempt to get length"):
		return "\nhint: a value is nil; check that the table key exists and that string.match/find matched (they return nil when nothing matches). Names shown like '(for state)' are loop internals."
	case strings.Contains(message, "(for generator)"):
		return "\nhint: iterate a table with pairs(t) or ipairs(t): for k, v in pairs(t) do ... end."
	case strings.Contains(message, "attempt to call"):
		return k.missingFunctionHint(message, code)
	}
	return ""
}

// missingFunctionHint names the function a failing line called that does not
// exist, and lists what the library does have.
func (k *luaKernel) missingFunctionHint(message, code string) string {
	match := luaErrorLine.FindStringSubmatch(message)
	if match == nil {
		return ""
	}
	line, _ := strconv.Atoi(match[1])
	lines := strings.Split(code, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	source := lines[line-1]
	l := k.state
	var hints []string
	for _, call := range luaDottedCall.FindAllString(source, -1) {
		path := strings.Split(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(call), "(")), ".")
		if hint := k.missingMember(l, path); hint != "" {
			hints = append(hints, hint)
		}
	}
	for _, call := range luaMethodCall.FindAllStringSubmatch(source, -1) {
		if hint := k.missingMember(l, []string{"string", call[1]}); hint != "" {
			hints = append(hints, "if the receiver is a string: "+hint)
		}
	}
	if len(hints) == 0 {
		return ""
	}
	return "\nhint: " + strings.Join(hints, "; ")
}

// missingMember returns a hint when path (like {"string", "split"}) resolves
// to a library table that lacks its last element.
func (k *luaKernel) missingMember(l *lua.State, path []string) string {
	top := l.Top()
	defer l.SetTop(top)
	l.Global(path[0])
	for _, name := range path[1 : len(path)-1] {
		if !l.IsTable(-1) {
			return ""
		}
		l.Field(-1, name)
	}
	if !l.IsTable(-1) {
		return ""
	}
	last := path[len(path)-1]
	l.Field(-1, last)
	if !l.IsNil(-1) {
		return ""
	}
	l.Pop(1)
	var names []string
	l.PushNil()
	for l.Next(-2) {
		if key, ok := l.ToString(-2); ok && l.TypeOf(-1) == lua.TypeFunction {
			names = append(names, key)
		}
		l.Pop(1)
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return fmt.Sprintf("%s has no function %q; it has: %s", strings.Join(path[:len(path)-1], "."), last, strings.Join(names, ", "))
}

// csvValue converts a field to a number when it is a canonical numeral (what
// Lua would print back), so comparisons like row.qty >= 20 work while IDs such
// as "007", dates, and "1e5" stay text.
func csvValue(field string) any {
	if n, err := strconv.ParseFloat(field, 64); err == nil && strconv.FormatFloat(n, 'f', -1, 64) == field {
		return n
	}
	return field
}

// csvDecode is csv.decode(text, header): rows as arrays, or with a true header
// argument, tables keyed by the first row's column names. The header row is
// not returned; the column order is kept in the result's `columns` field.
// Canonical numerals become numbers; everything else stays a string.
func (k *luaKernel) csvDecode(l *lua.State) int {
	text := strings.TrimPrefix(lua.CheckString(l, 1), "\ufeff")
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	records, err := reader.ReadAll()
	if err != nil {
		lua.Errorf(l, "csv.decode: %s", err.Error())
	}
	header := l.ToBoolean(2) && len(records) > 0
	var columns []string
	if header {
		columns, records = records[0], records[1:]
	}
	rows := make([]any, 0, len(records))
	for _, record := range records {
		if !header {
			fields := make([]any, len(record))
			for i, field := range record {
				fields[i] = csvValue(field)
			}
			rows = append(rows, fields)
			continue
		}
		row := make(map[string]any, len(columns))
		for i, name := range columns {
			if i < len(record) {
				row[name] = csvValue(record[i])
			}
		}
		rows = append(rows, row)
	}
	luaPush(l, rows)
	if header {
		k.csvColumns = columns
		names := make([]any, len(columns))
		for i, name := range columns {
			names[i] = name
		}
		luaPush(l, names)
		l.SetField(-2, "columns")
	}
	return 1
}

// csvEncode is csv.encode(rows, columns): rows is an array of arrays, or an
// array of tables keyed by column name (as csv.decode(text, true) returns),
// which are written under a header line. The column order is columns if given,
// else rows.columns, else the last decoded header when it covers every key,
// else the sorted keys.
func (k *luaKernel) csvEncode(l *lua.State) int {
	if !l.IsTable(1) {
		lua.ArgumentError(l, 1, "array of rows expected")
	}
	var rows []any
	for i := 1; i <= l.RawLength(1); i++ {
		l.RawGetInt(1, i)
		rows = append(rows, luaToGo(l, l.Top(), 1))
		l.Pop(1)
	}
	var columns []string
	explicit := func(index int) bool {
		if names, ok := luaToGo(l, index, 1).([]any); ok {
			for _, name := range names {
				columns = append(columns, fmt.Sprint(name))
			}
		}
		return len(columns) > 0
	}
	if !(l.IsTable(2) && explicit(2)) {
		l.Field(1, "columns")
		if !(l.IsTable(-1) && explicit(l.Top())) {
			columns = nil
		}
		l.Pop(1)
	}
	keys := map[string]bool{}
	hasObjects := false
	for _, row := range rows {
		if object, ok := row.(map[string]any); ok {
			hasObjects = true
			for key := range object {
				keys[key] = true
			}
		}
	}
	if hasObjects && len(columns) == 0 {
		covered := len(k.csvColumns) > 0
		for key := range keys {
			covered = covered && slicesContains(k.csvColumns, key)
		}
		if covered {
			columns = k.csvColumns
		} else {
			for key := range keys {
				columns = append(columns, key)
			}
			sort.Strings(columns)
		}
	}
	text := func(field any) string {
		switch v := field.(type) {
		case nil:
			return ""
		case string:
			return v
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return fmt.Sprint(field)
	}
	var out strings.Builder
	writer := csv.NewWriter(&out)
	write := func(record []string) {
		if err := writer.Write(record); err != nil {
			lua.Errorf(l, "csv.encode: %s", err.Error())
		}
	}
	if hasObjects {
		write(columns)
	}
	for _, row := range rows {
		switch v := row.(type) {
		case []any:
			record := make([]string, len(v))
			for i, field := range v {
				record[i] = text(field)
			}
			write(record)
		case map[string]any:
			record := make([]string, len(columns))
			for i, name := range columns {
				record[i] = text(v[name])
			}
			write(record)
		default:
			lua.ArgumentError(l, 1, "each row must be an array or a table keyed by column")
		}
	}
	writer.Flush()
	l.PushString(out.String())
	return 1
}

func slicesContains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}
