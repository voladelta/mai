# mai

![mai project banner](assets/mai-banner.png)

`mai` is a small coding agent for macOS and Linux. It uses your existing Codex
ChatGPT login, so you do not need an OpenAI API key.

The agent has 5 tools:

- `bash` reads files, searches code and runs commands
- `apply_patch` creates, changes, moves and deletes files
- `read_skill` loads the complete instructions for one skill
- `read_skill_file` loads a required supporting file from that skill
- `spawn_subagent` runs one installed custom agent and returns its final output

Skills are read only from `~/.agents/skills`. Each main-agent request includes
all eligible skill names and descriptions, then loads a complete `SKILL.md` only
when needed. Each skill description must be 1,024 characters or fewer.
Set `policy.allow_implicit_invocation` to `false` in `agents/openai.yaml` to hide
a skill from automatic selection; an explicit `$skill-name` still loads it.
Images use typed image output; other binary files are rejected.

Custom agents are read from `$CODEX_HOME/agents`, or `~/.codex/agents` when
`CODEX_HOME` is not set. Each `<name>.toml` file must define matching `name`,
`description`, `developer_instructions`, `model`, and `model_reasoning_effort`
fields. Mai supports the same model names and reasoning efforts as its command
line options.

Run a custom agent directly with:

```bash
mai --subagent repo_scout "map the parser"
```

This mode uses the model, effort, and developer instructions from
`repo_scout.toml`. It implies `--no-input` and cannot be combined with
`--persist`, `--last`, `--model`, or `--effort`. A custom subagent does not
receive the general skill catalog. Its developer instructions must identify any
required skills.

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

Tasks are stateless by default. A normal run does not write mai settings or
conversation history.

Use `--persist` to save a new task in the current project:

```bash
mai "add tests for the parser" --persist
```

Resume the current saved task in that project with `--last`:

```bash
mai "now fix the failing test" --last
```

`--last` restores the original working directory, model, effort and conversation
history. Two processes cannot use the same saved task at the same time. Other
saved tasks can run at the same time.

Run `mai` without a prompt to show concise usage text. Run `mai --help` for all
options.

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

Each new task uses `luna` with medium effort unless you set `-m` or `-e`. The
saved task keeps its values when you use `--last`.

You can use these options with a new or saved task:

```bash
mai "refactor this package" -m sol -e h
mai "continue the refactor" --last -m terra -e max
```

## Timeouts and interactive input

Each Codex request has a 10-minute timeout. Set a different positive Go-style
duration when necessary:

```bash
mai "investigate the failure" --timeout 20m
```

The same timeout limits the full lifetime of a spawned child process. A parent
runs one child at a time. The child uses the parent's working directory, starts
with new in-memory history, and cannot spawn another child. The parent receives
the child's final standard output. Failed calls also include the child's
standard error.

Each `bash` result reports its duration and original output byte counts. Mai
keeps at most 64 KiB from each stream. For longer output, it preserves the
beginning and end and reports the omitted byte count.

Use `--no-input` in scripts and other non-interactive environments. If a command
needs approval, `mai` rejects it instead of opening a terminal prompt.

## Authentication

`mai` reads `$CODEX_HOME/auth.json`. It reads `~/.codex/auth.json` when
`CODEX_HOME` is not set.

`mai` does not read `OPENAI_API_KEY`. It does not copy your Codex credentials
into its state files.

If Codex stores credentials only in the system keychain, set this option in your
Codex configuration:

```toml
cli_auth_credentials_store = "file"
```

Run `codex login` again after you change the option.

## Saved tasks

`mai` has no global settings or session file. When you use `--persist`, it stores
project-local state under the repository root:

- `.mai/current` contains the current session ID
- `.mai/sessions/<session-id>.json` contains one task and its history
- `.mai/locks/<session-id>.lock` prevents concurrent use of one task

For a directory outside Git, `.mai` is stored in the working directory. Mai
creates `.mai/.gitignore` so Git does not add the saved state. State directories
use permission mode `0700`. State files use mode `0600`.

Each persisted task has a separate session file. Starting concurrent tasks does
not replace their history. An atomic update to `current` selects the task that a
later `--last` command will resume.

The saved history includes completed model output and encrypted reasoning state.
`mai` sends a stable cache key for each task so compatible requests can reuse
cached input.

Children never create their own saved tasks. A persisted parent saves the
`spawn_subagent` call and its returned result in the parent task history.

Mai tracks the active context size reported by the Codex backend. At 90% of the
model context window, it sends a Codex V2 compaction request before the next
model request. The compacted history keeps recent user messages and the encrypted
compaction item. Persisted tasks save this replacement history before they
continue.

## Safety

`apply_patch` can only change files inside the repository. It rejects paths and
symbolic links that lead outside the repository.

`bash` can run any command available to your shell. It is not sandboxed.

A custom agent runs as a new `mai` process with the same file and command access
as its parent. Agent configuration fields such as `sandbox_mode` do not reduce
that access.

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
