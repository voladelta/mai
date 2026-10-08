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

func runLua(t *testing.T, ctx context.Context, a *agent, sess *session, code string) luaResult {
	t.Helper()
	arguments, _ := json.Marshal(map[string]string{"code": code})
	var text string
	if err := json.Unmarshal(a.executeLua(ctx, sess, string(arguments), "call"), &text); err != nil {
		t.Fatal(err)
	}
	var result luaResult
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("not a lua result: %s", text)
	}
	return result
}

func TestLuaStatePersistsAndReturnsValues(t *testing.T) {
	a, sess := fileTestAgent(t)
	first := runLua(t, context.Background(), a, sess, `total = 0
for _, n in ipairs{1, 2, 3} do total = total + n end
function add(x) return x + total end
print("sum", total)`)
	if !first.OK || !first.Fresh || first.Stdout != "sum\t6\n" || first.Result != "" {
		t.Fatalf("first cell: %#v", first)
	}
	if second := runLua(t, context.Background(), a, sess, `add(10)`); !second.OK || second.Fresh || second.Result != "16" {
		t.Fatalf("expression result or persistence: %#v", second)
	}
	if third := runLua(t, context.Background(), a, sess, "x = 2\nreturn x * 2, 'a'"); !third.OK || third.Result != `[4,"a"]` {
		t.Fatalf("multiple returns: %#v", third)
	}
}

func TestLuaTopLevelLocalsPersistButNestedOnesDoNot(t *testing.T) {
	a, sess := fileTestAgent(t)
	runLua(t, context.Background(), a, sess, `local rows = {1, 2, 3}
local a, b = 10, 20
local pending
local function double(n) return n * 2 end
local string = "shadow"
do
  local hidden = 1
end
for i = 1, 2 do
  local inner = i
end`)
	got := runLua(t, context.Background(), a, sess, `return #rows, a + b, pending, double(4), hidden, inner, type(string), ("x"):upper()`)
	if !got.OK || got.Result != `[3,30,null,8,null,null,"table","X"]` {
		t.Fatalf("locals: %#v", got)
	}
}

func TestLuaLongStringsAreNotRewritten(t *testing.T) {
	a, sess := fileTestAgent(t)
	got := runLua(t, context.Background(), a, sess, "text = [[\nlocal x = 1\n]]\nreturn text")
	if !got.OK || got.Result != `"local x = 1\n"` {
		t.Fatalf("long string was rewritten: %#v", got)
	}
}

func TestLuaErrorsKeepStateAndLaterCellsWork(t *testing.T) {
	a, sess := fileTestAgent(t)
	failed := runLua(t, context.Background(), a, sess, "kept = 5\nerror('boom')\nlater = 1")
	if failed.OK || !strings.Contains(failed.Error, "boom") || !strings.Contains(failed.Error, "cell1:2") {
		t.Fatalf("error: %#v", failed)
	}
	if got := runLua(t, context.Background(), a, sess, `kept, later`); !got.OK || got.Result != `[5,null]` {
		t.Fatalf("partial state: %#v", got)
	}
	if got := runLua(t, context.Background(), a, sess, `x = = 1`); got.OK || got.Error == "" {
		t.Fatalf("syntax error accepted: %#v", got)
	}
	if got := runLua(t, context.Background(), a, sess, `1 + 1`); !got.OK || got.Result != "2" {
		t.Fatalf("kernel unusable after syntax error: %#v", got)
	}
}

func TestLuaResetDiscardsState(t *testing.T) {
	a, sess := fileTestAgent(t)
	runLua(t, context.Background(), a, sess, `kept = 1`)
	a.executeLua(context.Background(), sess, `{"reset":true}`, "")
	if got := runLua(t, context.Background(), a, sess, `kept`); !got.OK || !got.Fresh || got.Result != "" {
		t.Fatalf("reset kept state: %#v", got)
	}
}

func TestLuaWithheldFunctionsExplainTheAlternative(t *testing.T) {
	a, sess := fileTestAgent(t)
	for code, want := range map[string]string{
		`io.open("x", "w")`:    "mai.write",
		`io.read()`:            "mai.read_text",
		`os.execute("ls")`:     "mai.bash",
		`os.getenv("HOME")`:    "printenv",
		`require("json")`:      "already loaded",
		`dofile("x.lua")`:      "mai.read_text",
		`package.loaded`:       "attempt to index",
		`return type(os.time)`: ``,
	} {
		got := runLua(t, context.Background(), a, sess, code)
		if want == "" {
			if !got.OK || got.Result != `"function"` {
				t.Errorf("%s: %#v", code, got)
			}
			continue
		}
		if got.OK || !strings.Contains(got.Error, want) {
			t.Errorf("%s: want error containing %q, got %#v", code, want, got)
		}
	}
}

func TestLuaTimeoutAbortsRunawayLoop(t *testing.T) {
	a, sess := fileTestAgent(t)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	got := runLua(t, ctx, a, sess, `while true do end`)
	if got.OK || time.Since(started) > 5*time.Second || !strings.Contains(got.Error, "cell stopped") {
		t.Fatalf("loop not aborted: %#v after %s", got, time.Since(started))
	}
	if got := runLua(t, context.Background(), a, sess, `1 + 1`); !got.OK || got.Result != "2" {
		t.Fatalf("kernel unusable after abort: %#v", got)
	}
}

func TestLuaMemoryGrowthIsCapped(t *testing.T) {
	old := luaMemoryLimit
	luaMemoryLimit = 64 << 20
	defer func() { luaMemoryLimit = old }()
	a, sess := fileTestAgent(t)
	got := runLua(t, context.Background(), a, sess, `t = {} local i = 0 while true do i = i + 1 t[i] = {i, tostring(i), i * 2} end`)
	if got.OK || !strings.Contains(got.Error, "memory limit") {
		t.Fatalf("growth not capped: %#v", got)
	}
	runLua(t, context.Background(), a, sess, `t = nil`)
}

func TestLuaOutputIsBounded(t *testing.T) {
	a, sess := fileTestAgent(t)
	got := runLua(t, context.Background(), a, sess, `for i = 1, 20000 do print("0123456789abcdef") end`)
	if !got.OK || !got.Truncated || len(got.Stdout) > maxToolStreamBytes+100 {
		t.Fatalf("output not bounded: ok=%v truncated=%v len=%d", got.OK, got.Truncated, len(got.Stdout))
	}
}

func TestLuaJSONRoundTrip(t *testing.T) {
	a, sess := fileTestAgent(t)
	got := runLua(t, context.Background(), a, sess, `local cfg = json.decode('{"a":{"b":false,"n":[1,2,3]},"s":"x"}')
return cfg.a.b, #cfg.a.n, cfg.s, json.encode({1, 2, {k = "v"}}), json.encode({}), json.encode(cfg.a.n)`)
	if !got.OK || got.Result != `[false,3,"x","[1,2,{\"k\":\"v\"}]","[]","[1,2,3]"]` {
		t.Fatalf("json: %#v", got)
	}
	if got := runLua(t, context.Background(), a, sess, `json.decode("{bad")`); got.OK || !strings.Contains(got.Error, "json.decode") {
		t.Fatalf("bad json accepted: %#v", got)
	}
}

func TestLuaHostCalls(t *testing.T) {
	a, sess := fileTestAgent(t)
	got := runLua(t, context.Background(), a, sess, `local r = mai.bash("printf hi; exit 3")
print(r.stdout, r.exit_code, r.ok)
local w = mai.write("note.txt", "alpha\nbeta\n")
print(w.ok)
return mai.read("note.txt", 2).content`)
	if !got.OK || got.Stdout != "hi\t3\tfalse\ntrue\n" || !strings.Contains(got.Result, "beta") {
		t.Fatalf("host calls: %#v", got)
	}
	got = runLua(t, context.Background(), a, sess, `return mai.edit("note.txt", "beta", "gamma").ok, mai.edit("note.txt", "missing", "x").ok`)
	if !got.OK || got.Result != "[true,false]" {
		t.Fatalf("edit: %#v", got)
	}
	data, err := os.ReadFile(filepath.Join(sess.RepoRoot, "note.txt"))
	if err != nil || string(data) != "alpha\ngamma\n" {
		t.Fatalf("note.txt = %q, %v", data, err)
	}
	if got := runLua(t, context.Background(), a, sess, `return mai.write("../escape.txt", "x").ok`); !got.OK || got.Result != "false" {
		t.Fatalf("write outside repository: %#v", got)
	}
}

func TestLuaReadTextIsRawAndConfinedToRepository(t *testing.T) {
	a, sess := fileTestAgent(t)
	mustWrite(t, filepath.Join(sess.RepoRoot, "data.csv"), "a,b\r\n1,2\r\n")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, outside, "secret")
	if err := os.Symlink(outside, filepath.Join(sess.RepoRoot, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if got := runLua(t, context.Background(), a, sess, `return mai.read_text("data.csv")`); !got.OK || got.Result != `"a,b\r\n1,2\r\n"` {
		t.Fatalf("read_text: %#v", got)
	}
	for _, path := range []string{outside, "../x", "link.txt", "missing"} {
		got := runLua(t, context.Background(), a, sess, `return mai.read_text([[`+path+`]])`)
		if got.OK || got.Error == "" {
			t.Errorf("read_text(%q) did not fail: %#v", path, got)
		}
	}
	// A raw read observes the file, so the script can then modify it, but only
	// while it is unchanged.
	if got := runLua(t, context.Background(), a, sess, `return mai.write("other.txt", "x").ok`); !got.OK || got.Result != "true" {
		t.Fatalf("creating a new file failed: %#v", got)
	}
	got := runLua(t, context.Background(), a, sess, `local t = mai.read_text("data.csv")
return mai.write("data.csv", t:upper()).ok`)
	if !got.OK || got.Result != "true" {
		t.Fatalf("write after read_text refused: %#v", got)
	}
	mustWrite(t, filepath.Join(sess.RepoRoot, "data.csv"), "changed behind its back")
	if got := runLua(t, context.Background(), a, sess, `return mai.write("data.csv", "x").ok`); !got.OK || got.Result != "false" {
		t.Fatalf("stale write accepted: %#v", got)
	}
	mustWrite(t, filepath.Join(sess.RepoRoot, "unread.txt"), "never read")
	if got := runLua(t, context.Background(), a, sess, `return mai.write("unread.txt", "x").ok`); !got.OK || got.Result != "false" {
		t.Fatalf("write to an unread file accepted: %#v", got)
	}
}

func TestLuaHistoryIncludesVisibleItemsAndExcludesOpaqueItems(t *testing.T) {
	a, sess := fileTestAgent(t)
	sess.History = []json.RawMessage{
		json.RawMessage(`{"role":"user","content":[{"type":"input_text","text":"The passphrase is copper heron"}]}`),
		json.RawMessage(`{"type":"reasoning","encrypted_content":"private-reasoning-marker"}`),
		json.RawMessage(`{"type":"function_call","name":"bash","call_id":"call-1","arguments":"{\"command\":\"date\"}"}`),
		json.RawMessage(`{"type":"function_call_output","call_id":"call-1","output":"2026-09-27"}`),
	}
	got := runLua(t, context.Background(), a, sess, `local found = mai.history("copper heron")
found.matches[1].text = "changed"
return found.total, mai.history("copper heron").matches[1].text, mai.history("private-reasoning-marker").total, mai.history("2026-09-27").matches[1].kind`)
	if !got.OK || got.Result != `[1,"The passphrase is copper heron",0,"tool_result"]` {
		t.Fatalf("history: %#v", got)
	}
}

func TestLuaHistoryExcludesActiveCellCode(t *testing.T) {
	a, sess := fileTestAgent(t)
	sess.History = []json.RawMessage{
		json.RawMessage(`{"type":"function_call","name":"lua","call_id":"call","arguments":"{\"code\":\"mai.history('absent-needle')\"}"}`),
	}
	if got := runLua(t, context.Background(), a, sess, `return mai.history("absent-needle").total`); !got.OK || got.Result != "0" {
		t.Fatalf("cell matched its own query: %#v", got)
	}
}

func TestLuaHistoryPagesPastEarlierMatches(t *testing.T) {
	a, sess := fileTestAgent(t)
	for i := 0; i < 25; i++ {
		sess.History = append(sess.History, json.RawMessage(`{"role":"user","content":"needle earlier"}`))
	}
	sess.History = append(sess.History, json.RawMessage(`{"role":"user","content":"needle final fact is 42"}`))
	got := runLua(t, context.Background(), a, sess, `local page = mai.history("needle")
local seen = {}
for _, m in ipairs(page.matches) do seen[#seen + 1] = m end
while page.next do
  page = mai.history("needle", 20, page.next)
  for _, m in ipairs(page.matches) do seen[#seen + 1] = m end
end
return #seen, seen[#seen].index == mai.history("final fact").matches[1].index, seen[#seen].text`)
	if !got.OK || got.Result != `[26,true,"needle final fact is 42"]` {
		t.Fatalf("paging: %#v", got)
	}
}

func TestLuaArgumentValidation(t *testing.T) {
	a, sess := fileTestAgent(t)
	for _, arguments := range []string{`{}`, `{"code":"1","reset":true}`, `{"reset":false}`, `{"code":"  "}`} {
		var text string
		_ = json.Unmarshal(a.executeLua(context.Background(), sess, arguments, ""), &text)
		if !strings.Contains(text, `"ok":false`) {
			t.Errorf("%s accepted: %s", arguments, text)
		}
	}
}

func TestLuaStringPatterns(t *testing.T) {
	a, sess := fileTestAgent(t)
	for code, want := range map[string]string{
		`return ("hello world"):match("o w")`:                                                  `"o w"`,
		`return ("key=val"):match("(%w+)=(%w+)")`:                                              `["key","val"]`,
		`return ("2026-03-05T09:41:02 ERROR [db] x"):match("T(%d+):.* (%u+) %[(%w+)%]")`:       `["09","ERROR","db"]`,
		`return ("  trim  "):match("^%s*(.-)%s*$")`:                                            `"trim"`,
		`return ("abc"):match("()b()")`:                                                        `[2,3]`,
		`return ("abc"):match("x")`:                                                            ``,
		`return ("a,b,,c"):find(",", 1, true)`:                                                 `[2,2]`,
		`return ("a.b"):find("%.")`:                                                            `[2,2]`,
		`return ("hello"):find("l+")`:                                                          `[3,4]`,
		`return ("hello"):find("(l)(l)")`:                                                      `[3,4,"l","l"]`,
		`return ("abc"):find("b", -1)`:                                                         ``,
		`return ("THE (quick) fox"):find("%((%a+)%)")`:                                         `[5,11,"quick"]`,
		`return ("f(a(b)c)d"):match("%b()")`:                                                   `"(a(b)c)"`,
		`return ("THE (quick) fox"):gsub("%a+", "x")`:                                          `["x (x) x",3]`,
		`return ("hello world"):gsub("(%w+) (%w+)", "%2 %1")`:                                  `["world hello",1]`,
		`return ("abc"):gsub("", "-")`:                                                         `["-a-b-c-",4]`,
		`return ("hello"):gsub("l", {l = "L"})`:                                                `["heLLo",2]`,
		`return ("1 2 3"):gsub("%d", function(d) return d * 2 end)`:                            `["2 4 6",3]`,
		`return ("abc"):gsub("%w", "%0%0", 2)`:                                                 `["aabbc",2]`,
		`return ("x"):gsub("x", "%%")`:                                                         `["%",1]`,
		`return ("a b"):gsub("^a", "z")`:                                                       `["z b",1]`,
		`return ("THE END"):lower():gsub("%f[%a]%a", string.upper)`:                            `["The End",2]`,
		`return ("a1b22c333"):gsub("%d+", function() return nil end)`:                          `["a1b22c333",3]`,
		`local t = {} for w in ("one two  three"):gmatch("%S+") do t[#t+1] = w end return t`:   `["one","two","three"]`,
		`local t = {} for k, v in ("a=1, b=2"):gmatch("(%w+)=(%w+)") do t[k] = v end return t`: `{"a":"1","b":"2"}`,
		`local n = 0 for _ in ("abc"):gmatch("") do n = n + 1 end return n`:                    `4`,
		`return ("x"):rep(3, ","), ("%5.2f"):format(3.14159), ("abc"):sub(2), #"héllo"`:        `["x,x,x"," 3.14","bc",6]`,
		`return os.date("!%Y-%m-%d %H:%M:%S", 86400), os.date("!*t", 0).year, unpack({1, 2})`:  `["1970-01-02 00:00:00",1970,1,2]`,
	} {
		got := runLua(t, context.Background(), a, sess, code)
		if !got.OK || got.Result != want {
			t.Errorf("%s\n  want %s\n  got  ok=%v result=%s err=%s", code, want, got.OK, got.Result, got.Error)
		}
	}
	for code, want := range map[string]string{
		`return ("a"):match("[a")`:     "malformed pattern (missing ']')",
		`return ("a"):match("%")`:      "malformed pattern (ends with '%')",
		`return ("a"):match("(a")`:     "unfinished capture",
		`return ("a"):gsub("a", "%2")`: "invalid capture index",
		`return ("a"):gsub("a", true)`: "string/function/table expected",
		`x = = 1`:                      "cell",
	} {
		got := runLua(t, context.Background(), a, sess, code)
		if got.OK || !strings.Contains(got.Error, want) {
			t.Errorf("%s: want error containing %q, got ok=%v %q", code, want, got.OK, got.Error)
		}
	}
}

func TestLuaHintsForCommonMistakes(t *testing.T) {
	a, sess := fileTestAgent(t)
	for code, want := range map[string]string{
		"local row = {}\nreturn row.qty * 2":      "tonumber() returns nil",
		"local t = {}\nreturn 'a' .. t.missing":   "a value is nil",
		"return string.split('a,b', ',')":         `string has no function "split"`,
		"local s = 'a b'\nreturn s:trim()":        `string has no function "trim"`,
		"return 5 >= '3'":                         "tonumber(x)",
		"local t = {a = 1}\nfor k, v in t do end": "pairs(t)",
		"return table.contains({1}, 1)":           `table has no function "contains"`,
	} {
		got := runLua(t, context.Background(), a, sess, code)
		if got.OK || !strings.Contains(got.Error, "hint:") || !strings.Contains(got.Error, want) {
			t.Errorf("%q: want hint containing %q, got %q", code, want, got.Error)
		}
	}
}

func TestLuaIOIsReadOnlyOverRepositoryFiles(t *testing.T) {
	a, sess := fileTestAgent(t)
	mustWrite(t, filepath.Join(sess.RepoRoot, "data.txt"), "one\ntwo\n12 34\nlast")
	got := runLua(t, context.Background(), a, sess, `local f = io.open("data.txt")
local first = f:read("*l")
local second = f:read("l")
local n1, n2 = f:read("*n", "*n")
f:read("*l")
local rest = f:read("*a")
local nothing = f:read("*l")
f:close()
local lines = {}
for l in io.lines("data.txt") do lines[#lines + 1] = l end
local missing, err = io.open("nope.txt")
local all = io.open("data.txt"):read("*a")
local pipe = io.popen("printf 'a\\nb\\n'"):read("*a")
return first, second, n1, n2, rest, nothing, #lines, missing, #all, pipe`)
	if !got.OK || got.Result != `["one","two",12,34,"last",null,4,null,18,"a\nb\n"]` {
		t.Fatalf("io: %#v", got)
	}
	got = runLua(t, context.Background(), a, sess, `local f, err = io.open("../outside.txt")
return f == nil, type(err)`)
	if !got.OK || got.Result != `[true,"string"]` {
		t.Fatalf("outside read must fail softly: %#v", got)
	}
}

func TestLuaToNumberIgnoresInvalidBase(t *testing.T) {
	a, sess := fileTestAgent(t)
	got := runLua(t, context.Background(), a, sess, `local s = "1,234"
return tonumber(s:gsub(",", "")), tonumber("ff", 16), tonumber("101", 2), tonumber("7", 99), tonumber("x"), tonumber(nil)`)
	if !got.OK || got.Result != `[1234,255,5,7,null,null]` {
		t.Fatalf("tonumber: %#v", got)
	}
}

func TestLuaCSV(t *testing.T) {
	a, sess := fileTestAgent(t)
	got := runLua(t, context.Background(), a, sess, `local text = "date,region,qty,price,id\r\n2026-03-01,north,12,0.5,007\r\n2026-03-02,\"south, far\",3,0.25,8\r\n"
local rows = csv.decode(text, true)
local raw = csv.decode(text)
local total = 0
for _, r in ipairs(rows) do total = total + r.qty * r.price end
return #rows, rows[2].region, total, rows[1].id, type(rows[1].qty), rows[1].qty >= 10, #raw, raw[1][2], table.concat(rows.columns, "|"), type(raw[2][3])`)
	if !got.OK || got.Result != `[2,"south, far",6.75,"007","number",true,3,"region","date|region|qty|price|id","number"]` {
		t.Fatalf("csv decode: %#v", got)
	}
	got = runLua(t, context.Background(), a, sess, `local rows = csv.decode("b,a,c\n1,2,3\n4,5,6\n", true)
local keep = {}
for _, r in ipairs(rows) do if r.a > 2 then keep[#keep + 1] = r end end
return csv.encode(keep), csv.encode(rows, {"c", "a"}), csv.encode({{"x", "y,z", 1.5}, {"q", "", 2}}), csv.encode({{k = "v", a = 1}})`)
	if !got.OK || got.Result != `["b,a,c\n4,5,6\n","c,a\n3,2\n6,5\n","x,\"y,z\",1.5\nq,,2\n","a,k\n1,v\n"]` {
		t.Fatalf("csv encode: %#v", got)
	}
	if got := runLua(t, context.Background(), a, sess, `return #csv.decode(""), #csv.decode("a,b", true)`); !got.OK || got.Result != "[0,0]" {
		t.Fatalf("empty csv: %#v", got)
	}
	if got := runLua(t, context.Background(), a, sess, `return csv.encode("x")`); got.OK {
		t.Fatalf("encode accepted a string: %#v", got)
	}
}
