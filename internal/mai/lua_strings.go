package mai

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	lua "github.com/Shopify/go-lua"
)

// go-lua ships without string.match, gmatch and gsub, and its find has no
// pattern support, so this file ports the Lua 5.2 pattern matcher (lstrlib.c).

const (
	luaMaxCaptures   = 32
	luaMaxMatchDepth = 200
	luaCapUnfinished = -1
	luaCapPosition   = -2
	luaPatternSpecs  = "^$*+?.([%-"
)

type luaCapture struct{ start, length int }

type luaMatcher struct {
	l        *lua.State
	src, pat string
	level    int
	depth    int
	capture  [luaMaxCaptures]luaCapture
}

func (m *luaMatcher) fail(format string, args ...any) {
	lua.Errorf(m.l, format, args...)
}

func (m *luaMatcher) reset() {
	m.level = 0
	m.depth = luaMaxMatchDepth
}

func (m *luaMatcher) classEnd(p int) int {
	if p >= len(m.pat) {
		m.fail("malformed pattern (ends with '%%')")
	}
	c := m.pat[p]
	p++
	switch c {
	case '%':
		if p >= len(m.pat) {
			m.fail("malformed pattern (ends with '%%')")
		}
		return p + 1
	case '[':
		if p < len(m.pat) && m.pat[p] == '^' {
			p++
		}
		for {
			if p >= len(m.pat) {
				m.fail("malformed pattern (missing ']')")
			}
			c := m.pat[p]
			p++
			if c == '%' && p < len(m.pat) {
				p++
			}
			if p < len(m.pat) && m.pat[p] == ']' {
				return p + 1
			}
		}
	}
	return p
}

func luaMatchClass(c, class byte) bool {
	var res bool
	switch class | 0x20 {
	case 'a':
		res = c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	case 'c':
		res = c < 32 || c == 127
	case 'd':
		res = c >= '0' && c <= '9'
	case 'g':
		res = c > 32 && c < 127
	case 'l':
		res = c >= 'a' && c <= 'z'
	case 'p':
		res = c > 32 && c < 127 && !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z')
	case 's':
		res = c == ' ' || c >= '\t' && c <= '\r'
	case 'u':
		res = c >= 'A' && c <= 'Z'
	case 'w':
		res = c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	case 'x':
		res = c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
	default:
		return class == c
	}
	if class >= 'A' && class <= 'Z' {
		return !res
	}
	return res
}

// matchBracketClass reports whether c is in the set pat[p:ec+1], where
// pat[p] is '[' and pat[ec] is the closing ']'.
func (m *luaMatcher) matchBracketClass(c byte, p, ec int) bool {
	sig := true
	if m.pat[p+1] == '^' {
		sig = false
		p++
	}
	for p++; p < ec; p++ {
		switch {
		case m.pat[p] == '%':
			p++
			if luaMatchClass(c, m.pat[p]) {
				return sig
			}
		case p+2 < ec && m.pat[p+1] == '-':
			p += 2
			if m.pat[p-2] <= c && c <= m.pat[p] {
				return sig
			}
		case m.pat[p] == c:
			return sig
		}
	}
	return !sig
}

func (m *luaMatcher) singleMatch(s, p, ep int) bool {
	if s >= len(m.src) {
		return false
	}
	c := m.src[s]
	switch m.pat[p] {
	case '.':
		return true
	case '%':
		return luaMatchClass(c, m.pat[p+1])
	case '[':
		return m.matchBracketClass(c, p, ep-1)
	}
	return m.pat[p] == c
}

func (m *luaMatcher) matchBalance(s, p int) int {
	if p+1 >= len(m.pat) {
		m.fail("missing arguments to '%%b'")
	}
	if s >= len(m.src) || m.src[s] != m.pat[p] {
		return -1
	}
	open, closing := m.pat[p], m.pat[p+1]
	depth := 1
	for s++; s < len(m.src); s++ {
		switch m.src[s] {
		case closing:
			if depth--; depth == 0 {
				return s + 1
			}
		case open:
			depth++
		}
	}
	return -1
}

func (m *luaMatcher) maxExpand(s, p, ep int) int {
	i := 0
	for m.singleMatch(s+i, p, ep) {
		i++
	}
	for ; i >= 0; i-- {
		if res := m.match(s+i, ep+1); res != -1 {
			return res
		}
	}
	return -1
}

func (m *luaMatcher) minExpand(s, p, ep int) int {
	for {
		if res := m.match(s, ep+1); res != -1 {
			return res
		}
		if !m.singleMatch(s, p, ep) {
			return -1
		}
		s++
	}
}

func (m *luaMatcher) startCapture(s, p, what int) int {
	if m.level >= luaMaxCaptures {
		m.fail("too many captures")
	}
	m.capture[m.level] = luaCapture{s, what}
	m.level++
	res := m.match(s, p)
	if res == -1 {
		m.level--
	}
	return res
}

func (m *luaMatcher) endCapture(s, p int) int {
	l := -1
	for i := m.level - 1; i >= 0; i-- {
		if m.capture[i].length == luaCapUnfinished {
			l = i
			break
		}
	}
	if l < 0 {
		m.fail("invalid pattern capture")
	}
	m.capture[l].length = s - m.capture[l].start
	res := m.match(s, p)
	if res == -1 {
		m.capture[l].length = luaCapUnfinished
	}
	return res
}

func (m *luaMatcher) matchCapture(s int, digit byte) int {
	l := int(digit) - '1'
	if l < 0 || l >= m.level || m.capture[l].length == luaCapUnfinished {
		m.fail("invalid capture index %%%d", l+1)
	}
	c := m.capture[l]
	if len(m.src)-s >= c.length && m.src[c.start:c.start+c.length] == m.src[s:s+c.length] {
		return s + c.length
	}
	return -1
}

// match returns the end of the match of pat[p:] at src[s:], or -1.
func (m *luaMatcher) match(s, p int) int {
	if m.depth--; m.depth == 0 {
		m.fail("pattern too complex")
	}
	defer func() { m.depth++ }()
	for {
		if p == len(m.pat) {
			return s
		}
		switch m.pat[p] {
		case '(':
			if p+1 < len(m.pat) && m.pat[p+1] == ')' {
				return m.startCapture(s, p+2, luaCapPosition)
			}
			return m.startCapture(s, p+1, luaCapUnfinished)
		case ')':
			return m.endCapture(s, p+1)
		case '$':
			if p+1 == len(m.pat) {
				if s == len(m.src) {
					return s
				}
				return -1
			}
		case '%':
			if p+1 < len(m.pat) {
				switch d := m.pat[p+1]; {
				case d == 'b':
					if s = m.matchBalance(s, p+2); s != -1 {
						p += 4
						continue
					}
					return -1
				case d == 'f':
					p += 2
					if p >= len(m.pat) || m.pat[p] != '[' {
						m.fail("missing '[' after '%%f' in pattern")
					}
					ep := m.classEnd(p)
					var prev, cur byte
					if s > 0 {
						prev = m.src[s-1]
					}
					if s < len(m.src) {
						cur = m.src[s]
					}
					if !m.matchBracketClass(prev, p, ep-1) && m.matchBracketClass(cur, p, ep-1) {
						p = ep
						continue
					}
					return -1
				case d >= '0' && d <= '9':
					if s = m.matchCapture(s, d); s != -1 {
						p += 2
						continue
					}
					return -1
				}
			}
		}
		ep := m.classEnd(p)
		var epc byte
		if ep < len(m.pat) {
			epc = m.pat[ep]
		}
		if !m.singleMatch(s, p, ep) {
			if epc == '*' || epc == '?' || epc == '-' {
				p = ep + 1
				continue
			}
			return -1
		}
		switch epc {
		case '?':
			if res := m.match(s+1, ep+1); res != -1 {
				return res
			}
			p = ep + 1
		case '+':
			return m.maxExpand(s+1, p, ep)
		case '*':
			return m.maxExpand(s, p, ep)
		case '-':
			return m.minExpand(s, p, ep)
		default:
			s++
			p = ep
		}
	}
}

// capture returns capture i of the match src[s:e] as a string or position.
func (m *luaMatcher) captureValue(i, s, e int) (text string, position int, isPosition bool) {
	if i >= m.level {
		if i != 0 {
			m.fail("invalid capture index %%%d", i+1)
		}
		return m.src[s:e], 0, false
	}
	c := m.capture[i]
	switch c.length {
	case luaCapUnfinished:
		m.fail("unfinished capture")
	case luaCapPosition:
		return "", c.start + 1, true
	}
	return m.src[c.start : c.start+c.length], 0, false
}

func (m *luaMatcher) pushCapture(i, s, e int) {
	text, pos, isPos := m.captureValue(i, s, e)
	if isPos {
		m.l.PushInteger(pos)
	} else {
		m.l.PushString(text)
	}
}

// pushCaptures pushes every capture (or the whole match when there are none
// and wholeIfNone is set) and returns how many it pushed.
func (m *luaMatcher) pushCaptures(s, e int, wholeIfNone bool) int {
	n := m.level
	if n == 0 && wholeIfNone {
		n = 1
	}
	for i := 0; i < n; i++ {
		m.pushCapture(i, s, e)
	}
	return n
}

func luaPosRelative(pos, length int) int {
	switch {
	case pos >= 0:
		return pos
	case -pos > length:
		return 0
	}
	return length + pos + 1
}

func luaFind(find bool) lua.Function {
	return func(l *lua.State) int {
		s, p := lua.CheckString(l, 1), lua.CheckString(l, 2)
		init := luaPosRelative(lua.OptInteger(l, 3, 1), len(s))
		if init == 0 || init-1 > len(s) {
			l.PushNil()
			return 1
		}
		init--
		if find && (l.ToBoolean(4) || !strings.ContainsAny(p, luaPatternSpecs)) {
			if at := strings.Index(s[init:], p); at >= 0 {
				l.PushInteger(init + at + 1)
				l.PushInteger(init + at + len(p))
				return 2
			}
			l.PushNil()
			return 1
		}
		m := &luaMatcher{l: l, src: s, pat: p}
		anchor := strings.HasPrefix(p, "^")
		start := 0
		if anchor {
			start = 1
		}
		for s1 := init; s1 <= len(s); s1++ {
			m.reset()
			if e := m.match(s1, start); e != -1 {
				if find {
					l.PushInteger(s1 + 1)
					l.PushInteger(e)
					return m.pushCaptures(s1, e, false) + 2
				}
				return m.pushCaptures(s1, e, true)
			}
			if anchor {
				break
			}
		}
		l.PushNil()
		return 1
	}
}

func luaGmatch(l *lua.State) int {
	s, p := lua.CheckString(l, 1), lua.CheckString(l, 2)
	src, lastMatch := 0, -1
	l.PushGoFunction(func(l *lua.State) int {
		m := &luaMatcher{l: l, src: s, pat: p}
		for ; src <= len(s); src++ {
			m.reset()
			if e := m.match(src, 0); e != -1 && e != lastMatch {
				start := src
				src, lastMatch = e, e
				return m.pushCaptures(start, e, true)
			}
		}
		return 0
	})
	return 1
}

func luaGsub(l *lua.State) int {
	s, p := lua.CheckString(l, 1), lua.CheckString(l, 2)
	kind := l.TypeOf(3)
	if kind != lua.TypeNumber && kind != lua.TypeString && kind != lua.TypeTable && kind != lua.TypeFunction {
		lua.ArgumentError(l, 3, "string/function/table expected")
	}
	maxN := lua.OptInteger(l, 4, len(s)+1)
	anchor := strings.HasPrefix(p, "^")
	start := 0
	if anchor {
		start = 1
	}
	m := &luaMatcher{l: l, src: s, pat: p}
	var out strings.Builder
	src, n := 0, 0
	for n < maxN {
		m.reset()
		e := m.match(src, start)
		if e != -1 {
			n++
			out.WriteString(m.replacement(src, e))
		}
		if e != -1 && e > src {
			src = e
		} else if src < len(s) {
			out.WriteByte(s[src])
			src++
		} else {
			break
		}
		if anchor {
			break
		}
	}
	out.WriteString(s[src:])
	l.PushString(out.String())
	l.PushInteger(n)
	return 2
}

// replacement computes the text that replaces the match src[s:e] using the
// replacement argument (index 3) of gsub.
func (m *luaMatcher) replacement(s, e int) string {
	l := m.l
	whole := m.src[s:e]
	switch l.TypeOf(3) {
	case lua.TypeFunction:
		l.PushValue(3)
		n := m.pushCaptures(s, e, true)
		l.Call(n, 1)
	case lua.TypeTable:
		l.PushValue(3)
		m.pushCapture(0, s, e)
		l.Table(-2)
		l.Remove(-2)
	default:
		repl, _ := l.ToString(3)
		var b strings.Builder
		for i := 0; i < len(repl); i++ {
			if repl[i] != '%' {
				b.WriteByte(repl[i])
				continue
			}
			i++
			switch {
			case i >= len(repl) || !(repl[i] == '%' || repl[i] >= '0' && repl[i] <= '9'):
				m.fail("invalid use of '%%' in replacement string")
			case repl[i] == '%':
				b.WriteByte('%')
			case repl[i] == '0':
				b.WriteString(whole)
			default:
				text, pos, isPos := m.captureValue(int(repl[i]-'1'), s, e)
				if isPos {
					text = strconv.Itoa(pos)
				}
				b.WriteString(text)
			}
		}
		return b.String()
	}
	defer l.Pop(1)
	if !l.ToBoolean(-1) {
		return whole
	}
	if !l.IsString(-1) {
		lua.Errorf(l, "invalid replacement value (a %s)", lua.TypeNameOf(l, -1))
	}
	text, _ := l.ToString(-1)
	return text
}

// luaStringMatching installs the pattern functions into the string table.
func luaStringMatching(l *lua.State) {
	l.Global("string")
	for name, f := range map[string]lua.Function{
		"find": luaFind(true), "match": luaFind(false), "gmatch": luaGmatch, "gsub": luaGsub,
	} {
		l.PushGoFunction(f)
		l.SetField(-2, name)
	}
	l.Pop(1)
}

// luaOSDate implements os.date: "*t" and "!*t" tables, and strftime-style
// formats, in local time or UTC with a leading "!".
func luaOSDate(l *lua.State) int {
	format := lua.OptString(l, 1, "%c")
	moment := time.Now()
	if !l.IsNoneOrNil(2) {
		moment = time.Unix(int64(lua.CheckNumber(l, 2)), 0)
	}
	if strings.HasPrefix(format, "!") {
		moment, format = moment.UTC(), format[1:]
	}
	if strings.HasPrefix(format, "*t") {
		l.CreateTable(0, 9)
		for _, field := range []struct {
			name  string
			value int
		}{
			{"year", moment.Year()}, {"month", int(moment.Month())}, {"day", moment.Day()},
			{"hour", moment.Hour()}, {"min", moment.Minute()}, {"sec", moment.Second()},
			{"wday", int(moment.Weekday()) + 1}, {"yday", moment.YearDay()},
		} {
			l.PushInteger(field.value)
			l.SetField(-2, field.name)
		}
		l.PushBoolean(moment.IsDST())
		l.SetField(-2, "isdst")
		return 1
	}
	var out strings.Builder
	for i := 0; i < len(format); i++ {
		if format[i] != '%' {
			out.WriteByte(format[i])
			continue
		}
		i++
		if i >= len(format) {
			lua.Errorf(l, "invalid conversion specifier '%%'")
		}
		layout, ok := map[byte]string{
			'Y': "2006", 'y': "06", 'm': "01", 'd': "02", 'H': "15", 'I': "03", 'M': "04", 'S': "05",
			'b': "Jan", 'B': "January", 'a': "Mon", 'A': "Monday", 'p': "PM", 'Z': "MST", 'z': "-0700",
			'c': "Mon Jan  2 15:04:05 2006", 'x': "01/02/06", 'X': "15:04:05", 'D': "01/02/06", 'F': "2006-01-02", 'T': "15:04:05",
		}[format[i]]
		switch {
		case format[i] == '%':
			out.WriteByte('%')
		case format[i] == 'j':
			fmt.Fprintf(&out, "%03d", moment.YearDay())
		case ok:
			out.WriteString(moment.Format(layout))
		default:
			lua.Errorf(l, "invalid conversion specifier '%%%c'", format[i])
		}
	}
	l.PushString(out.String())
	return 1
}
