# mai

`mai` is a small coding agent for macOS and Linux. It uses your existing Codex
ChatGPT login, so you do not need an OpenAI API key.

The agent has 5 tools:

- `bash` reads files, searches code and runs commands
- `apply_patch` creates, changes, moves and deletes files
- `search_skills` searches installed skill names and descriptions
- `read_skill` loads the complete instructions for one skill
- `read_skill_file` loads a required supporting file from that skill

Skills are read only from `~/.agents/skills`. An empty skill search lists the
installed skills. Supporting files are loaded only after the agent reads the
skill's `SKILL.md`; binary files use base64 in tool output.

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

The first run uses `luna` with `max` effort. An explicit `-m` or `-e` value
becomes the new default for later tasks.

You can use these options with a new or saved task:

```bash
mai "refactor this package" -m sol -e h
mai "continue the refactor" --last -m terra -e max
```

## Authentication

`mai` reads `$CODEX_HOME/auth.json`. It reads `~/.codex/auth.json` when
`CODEX_HOME` is not set.

`mai` does not read `OPENAI_API_KEY`. It does not copy your Codex credentials
into `~/.mai`.

If Codex stores credentials only in the system keychain, set this option in your
Codex configuration:

```toml
cli_auth_credentials_store = "file"
```

Run `codex login` again after you change the option.

## Saved settings and tasks

`mai` stores its own data in 2 files:

- `~/.mai/config.json` stores the default model and effort
- `~/.mai/session.json` stores the single saved task and its conversation history

The `~/.mai` directory uses permission mode `0700`. Both files use mode `0600`.

The saved history includes completed model output and encrypted reasoning state.
`mai` sends a stable cache key for each task so compatible requests can reuse
cached input.

## Safety

`apply_patch` can only change files inside the repository. It rejects paths and
symbolic links that lead outside the repository.

`bash` can run any command available to your shell. It is not sandboxed.

`mai` checks recognisable `rm` commands before it runs them. It asks for approval
when a target is outside the repository or cannot be resolved safely.

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
