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

type rmInvocation struct {
	executable              shellToken
	args                    []shellToken
	directoryMayHaveChanged bool
}

func requiresRMApproval(command, cwd, repoRoot string) (bool, string) {
	tokens, uncertain := lexShell(command)
	if uncertain && strings.Contains(command, "rm") {
		return true, "the shell command could not be analyzed safely"
	}
	if strings.Contains(command, "rm") &&
		(strings.Contains(command, "$(") || strings.Contains(command, "`")) {
		return true, "rm appears with command substitution"
	}

	invocations, unclassified := findRMInvocations(tokens)
	if unclassified {
		return true, "rm appears after a command that could not be analyzed safely"
	}
	for _, invocation := range invocations {
		if reason := rmApprovalReason(invocation, cwd, repoRoot); reason != "" {
			return true, reason
		}
	}
	return false, ""
}

func findRMInvocations(tokens []shellToken) ([]rmInvocation, bool) {
	var invocations []rmInvocation
	commandStart := true
	wrapped := false
	wrapper := ""
	wrapperArgument := false
	directoryMayHaveChanged := false
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok.op {
			commandStart = true
			wrapped = false
			wrapper = ""
			wrapperArgument = false
			continue
		}
		if tok.text == "then" || tok.text == "do" || tok.text == "else" {
			commandStart = true
			continue
		}
		if !commandStart {
			continue
		}
		if wrapperArgument {
			wrapperArgument = false
			continue
		}
		if isAssignment(tok.text) {
			continue
		}
		base := filepath.Base(tok.text)
		if isCommandWrapper(base) {
			wrapped = true
			wrapper = base
			continue
		}
		if wrapped && strings.HasPrefix(tok.text, "-") {
			wrapperArgument = wrapperOptionNeedsArgument(wrapper, tok.text)
			continue
		}
		if base != "rm" {
			if changesDirectory(base) {
				directoryMayHaveChanged = true
			}
			if base != "printf" && containsRMWord(shellCommandArgs(tokens[i+1:])) {
				return invocations, true
			}
			commandStart = false
			continue
		}
		invocations = append(invocations, rmInvocation{
			executable: tok, args: shellCommandArgs(tokens[i+1:]),
			directoryMayHaveChanged: directoryMayHaveChanged,
		})
		commandStart = false
	}
	return invocations, false
}

func containsRMWord(tokens []shellToken) bool {
	for _, token := range tokens {
		if token.op {
			continue
		}
		for _, word := range strings.Fields(token.text) {
			word = strings.Trim(word, ";|&()")
			if filepath.Base(word) == "rm" {
				return true
			}
		}
	}
	return false
}

func wrapperOptionNeedsArgument(wrapper, option string) bool {
	switch wrapper {
	case "exec":
		return option == "-a"
	case "env":
		return option == "-u" || option == "--unset" || option == "-C" || option == "--chdir" || option == "-S" || option == "--split-string"
	case "sudo":
		switch option {
		case "-C", "--close-from", "-D", "--chdir", "-g", "--group", "-h", "--host", "-p", "--prompt", "-R", "--chroot", "-r", "--role", "-T", "--command-timeout", "-t", "--type", "-U", "--other-user", "-u", "--user":
			return true
		}
	}
	return false
}

func isCommandWrapper(command string) bool {
	switch command {
	case "!", "sudo", "command", "builtin", "nohup", "env", "time", "exec":
		return true
	default:
		return false
	}
}

func changesDirectory(command string) bool {
	return command == "cd" || command == "pushd" || command == "popd"
}

func shellCommandArgs(tokens []shellToken) []shellToken {
	for i, token := range tokens {
		if token.op {
			return tokens[:i]
		}
	}
	return tokens
}

func rmApprovalReason(invocation rmInvocation, cwd, repoRoot string) string {
	if invocation.executable.dynamic {
		return "the rm executable is dynamic"
	}
	if invocation.directoryMayHaveChanged {
		return "the command changes directory before invoking rm"
	}
	for _, target := range invocation.args {
		if reason := rmTargetApprovalReason(target, cwd, repoRoot); reason != "" {
			return reason
		}
	}
	return ""
}

func rmTargetApprovalReason(target shellToken, cwd, repoRoot string) string {
	if target.text == "--" || strings.HasPrefix(target.text, "-") {
		return ""
	}
	if target.dynamic || hasShellExpansion(target.text) {
		return fmt.Sprintf("rm target %q is dynamic", target.text)
	}
	path := target.text
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	if !pathWithin(repoRoot, path) {
		return fmt.Sprintf("rm target %s is outside %s", path, repoRoot)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && !pathWithin(repoRoot, resolved) {
		return fmt.Sprintf("rm target %s resolves outside %s", path, repoRoot)
	}
	return ""
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

type shellLexer struct {
	input   string
	out     []shellToken
	word    strings.Builder
	dynamic bool
}

func lexShell(input string) ([]shellToken, bool) {
	lexer := shellLexer{input: input}
	return lexer.scan()
}

func (lexer *shellLexer) scan() ([]shellToken, bool) {
	for i := 0; i < len(lexer.input); i++ {
		switch lexer.input[i] {
		case '\\':
			if !lexer.readEscape(&i) {
				return lexer.out, true
			}
		case '\'':
			if !lexer.readSingleQuoted(&i) {
				return lexer.out, true
			}
		case '"':
			if !lexer.readDoubleQuoted(&i) {
				return lexer.out, true
			}
		case ';', '|', '&', '\n', '(', ')':
			lexer.flush()
			lexer.out = append(lexer.out, shellToken{text: string(lexer.input[i]), op: true})
		case ' ', '\t', '\r':
			lexer.flush()
		default:
			lexer.writeUnquoted(lexer.input[i])
		}
	}
	lexer.flush()
	return lexer.out, false
}

func (lexer *shellLexer) flush() {
	if lexer.word.Len() == 0 {
		return
	}
	lexer.out = append(lexer.out, shellToken{text: lexer.word.String(), dynamic: lexer.dynamic})
	lexer.word.Reset()
	lexer.dynamic = false
}

func (lexer *shellLexer) readEscape(index *int) bool {
	if *index+1 >= len(lexer.input) {
		return false
	}
	(*index)++
	lexer.word.WriteByte(lexer.input[*index])
	return true
}

func (lexer *shellLexer) readSingleQuoted(index *int) bool {
	(*index)++
	for *index < len(lexer.input) && lexer.input[*index] != '\'' {
		lexer.word.WriteByte(lexer.input[*index])
		(*index)++
	}
	return *index < len(lexer.input)
}

func (lexer *shellLexer) readDoubleQuoted(index *int) bool {
	(*index)++
	for *index < len(lexer.input) && lexer.input[*index] != '"' {
		if lexer.input[*index] == '$' || lexer.input[*index] == '`' {
			lexer.dynamic = true
		}
		if lexer.input[*index] == '\\' && *index+1 < len(lexer.input) {
			(*index)++
		}
		lexer.word.WriteByte(lexer.input[*index])
		(*index)++
	}
	return *index < len(lexer.input)
}

func (lexer *shellLexer) writeUnquoted(ch byte) {
	if strings.ContainsRune("$`*?[{~", rune(ch)) {
		lexer.dynamic = true
	}
	lexer.word.WriteByte(ch)
}
