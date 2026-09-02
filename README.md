# mai

`mai` is a small coding agent for macOS and Linux. It uses your existing Codex
ChatGPT login, so you do not need an OpenAI API key.

The agent has 4 tools:

- `bash` reads files, searches code and runs commands
- `apply_patch` creates, changes, moves and deletes files
- `read_skill` loads the complete instructions for one skill
- `read_skill_file` loads a required supporting file from that skill

Skills are read only from `~/.agents/skills`. Each request includes eligible
skill names and descriptions in an 8,000-character catalog, then loads a complete
`SKILL.md` only when needed.
Set `policy.allow_implicit_invocation` to `false` in `agents/openai.yaml` to hide
a skill from automatic selection; an explicit `$skill-name` still loads it.
Images use typed image output; other binary files are rejected.

`mai` uses server-sent events (SSE). The Go standard library provides everything
it needs, so the project has no third-party dependencies.

## Requirements

You need:

- Go 1.27 or later
- a ChatGPT account with access to Codex
- the Codex command-line interface

Log in before you run `mai`:

```bash
codex login
```

## Build mai

```bash
cd ~/Codehub/mai
go build -o mai ./cmd/mai
```

Move the `mai` binary to a directory in your `PATH` if you want to run it from
any directory.

## Start a task

Run `mai` from the repository you want it to work on:

```bash
mai "add tests for the parser"
```

Start a new task without `--last`. This replaces the saved task.

Resume the saved task with `--last`:

```bash
mai "now fix the failing test" --last
```

`--last` restores the original working directory, model, effort and conversation
history.

Run `mai` without a prompt to show concise usage text and the current model and
effort defaults. Run `mai --help` for all options.

## Choose a model and effort

Use `-m` to choose a model:

- `luna` uses `gpt-5.6-luna`
- `terra` uses `gpt-5.6-terra`
- `sol` uses `gpt-5.6-sol`

Use `-e` to choose the reasoning effort:

- `l` means low
- `m` means medium
- `h` means high
- `x` means extra high
- `max` means maximum

The first run uses `luna` with medium effort. An explicit `-m` or `-e` value only
applies to that task. The saved task keeps those values when you use `--last`.

You can use these options with a new or saved task:

```bash
mai "refactor this package" -m sol -e h
mai "continue the refactor" --last -m terra -e max
```

Add `--save-defaults` to save an explicit model or effort for future tasks:

```bash
mai -m sol -e h --save-defaults
mai "refactor this package" -m sol -e h --save-defaults
```

## Timeouts and interactive input

Each Codex request has a 10-minute timeout. Set a different positive Go-style
duration when necessary:

```bash
mai "investigate the failure" --timeout 20m
```

Each `bash` result reports its duration and original output byte counts. Mai
keeps at most 64 KiB from each stream. For longer output, it preserves the
beginning and end and reports the omitted byte count.

Use `--no-input` in scripts and other non-interactive environments. If a command
needs approval, `mai` rejects it instead of opening a terminal prompt.

## Authentication

`mai` reads `$CODEX_HOME/auth.json`. It reads `~/.codex/auth.json` when
`CODEX_HOME` is not set.

`mai` does not read `OPENAI_API_KEY`. It does not copy your Codex credentials
into its configuration or state files.

If Codex stores credentials only in the system keychain, set this option in your
Codex configuration:

```toml
cli_auth_credentials_store = "file"
```

Run `codex login` again after you change the option.

## Saved settings and tasks

`mai` follows the XDG Base Directory Specification. It stores its own data in 2
files by default:

- `~/.config/mai/config.json` stores the default model and effort
- `~/.local/state/mai/session.json` stores the single saved task and its conversation history

`XDG_CONFIG_HOME` and `XDG_STATE_HOME` change these base directories. If the XDG
files do not exist, `mai` copies existing files from `~/.mai` and leaves the old
files in place. State directories use permission mode `0700`. Both files use
mode `0600`.

The saved history includes completed model output and encrypted reasoning state.
`mai` sends a stable cache key for each task so compatible requests can reuse
cached input.

## Safety

`apply_patch` can only change files inside the repository. It rejects paths and
symbolic links that lead outside the repository.

`bash` can run any command available to your shell. It is not sandboxed.

`mai` checks recognisable `rm` commands before it runs them. It asks for approval
when a target is outside the repository or cannot be resolved safely.

Approval is only interactive when standard input is a terminal and `--no-input`
is not set.

This check only covers `rm`. Other shell commands can still delete or overwrite
data.

## Test the project

```bash
go test -race ./...
go vet ./...
```

## Backend status

`mai` uses the ChatGPT Codex backend rather than the public OpenAI API. This is
suitable for personal use with your own ChatGPT subscription.

The backend is not a public API contract. OpenAI may change it without notice.
