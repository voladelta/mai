package mai

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

type shellToken struct {
	text    string
	op      bool
	dynamic bool
}

func requiresRMApproval(command, cwd, repoRoot string) (bool, string) {
	tokens, uncertain := lexShell(command)
	if uncertain && strings.Contains(command, "rm") {
		return true, "the shell command could not be analyzed safely"
	}
	if strings.Contains(command, "$(") || strings.Contains(command, "`") {
		if strings.Contains(command, "rm") {
			return true, "rm appears with command substitution"
		}
	}

	commandStart := true
	wrapped := false
	directoryMayHaveChanged := false
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.op {
			commandStart = true
			wrapped = false
			continue
		}
		if tok.text == "then" || tok.text == "do" || tok.text == "else" {
			commandStart = true
			continue
		}
		if !commandStart {
			continue
		}
		if isAssignment(tok.text) {
			continue
		}
		base := filepath.Base(tok.text)
		if base == "sudo" || base == "command" || base == "builtin" || base == "nohup" {
			wrapped = true
			continue
		}
		if base == "env" {
			wrapped = true
			continue
		}
		if wrapped && strings.HasPrefix(tok.text, "-") {
			continue
		}
		if base != "rm" {
			if base == "cd" || base == "pushd" || base == "popd" {
				directoryMayHaveChanged = true
			}
			commandStart = false
			continue
		}

		if tok.dynamic {
			return true, "the rm executable is dynamic"
		}
		if directoryMayHaveChanged {
			return true, "the command changes directory before invoking rm"
		}
		for j := i + 1; j < len(tokens) && !tokens[j].op; j++ {
			target := tokens[j]
			if target.text == "--" {
				continue
			}
			if strings.HasPrefix(target.text, "-") {
				continue
			}
			if target.dynamic || hasShellExpansion(target.text) {
				return true, fmt.Sprintf("rm target %q is dynamic", target.text)
			}
			path := target.text
			if !filepath.IsAbs(path) {
				path = filepath.Join(cwd, path)
			}
			path = filepath.Clean(path)
			if !pathWithin(repoRoot, path) {
				return true, fmt.Sprintf("rm target %s is outside %s", path, repoRoot)
			}
			if resolved, err := filepath.EvalSymlinks(path); err == nil && !pathWithin(repoRoot, resolved) {
				return true, fmt.Sprintf("rm target %s resolves outside %s", path, repoRoot)
			}
		}
		commandStart = false
	}
	return false, ""
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func hasShellExpansion(value string) bool {
	return strings.ContainsAny(value, "$`*?[{~")
}

func isAssignment(value string) bool {
	name, _, ok := strings.Cut(value, "=")
	if !ok || name == "" {
		return false
	}
	for i, r := range name {
		if !(r == '_' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r))) {
			return false
		}
	}
	return true
}

func lexShell(input string) ([]shellToken, bool) {
	var out []shellToken
	var b strings.Builder
	dynamic := false
	quoted := false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, shellToken{text: b.String(), dynamic: dynamic})
			b.Reset()
			dynamic = false
		}
	}
	for i := 0; i < len(input); i++ {
		ch := input[i]
		switch ch {
		case '\\':
			if i+1 >= len(input) {
				return out, true
			}
			i++
			b.WriteByte(input[i])
		case '\'':
			quoted = true
			i++
			for i < len(input) && input[i] != '\'' {
				b.WriteByte(input[i])
				i++
			}
			if i >= len(input) {
				return out, true
			}
		case '"':
			quoted = true
			i++
			for i < len(input) && input[i] != '"' {
				if input[i] == '$' || input[i] == '`' {
					dynamic = true
				}
				if input[i] == '\\' && i+1 < len(input) {
					i++
				}
				b.WriteByte(input[i])
				i++
			}
			if i >= len(input) {
				return out, true
			}
		case ';', '|', '&', '\n', '(', ')':
			flush()
			out = append(out, shellToken{text: string(ch), op: true})
		case ' ', '\t', '\r':
			flush()
		default:
			if ch == '$' || ch == '`' || ch == '*' || ch == '?' || ch == '[' || ch == '{' || ch == '~' {
				dynamic = true
			}
			b.WriteByte(ch)
		}
	}
	flush()
	_ = quoted
	return out, false
}
