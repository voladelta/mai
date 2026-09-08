# mai

![mai project banner](assets/mai-banner.png)

`mai` is a small coding agent for macOS and Linux. It uses your existing Codex
ChatGPT login, so you do not need an OpenAI API key.

The agent has 6 tools:

- `bash` reads files, searches code and runs commands
- `python` explores data in a persistent Python namespace
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
`description`, and `developer_instructions` fields, plus a reasoning effort.

Run a custom agent directly with:

```bash
mai --subagent repo_scout "map the parser"
```

This mode uses the effort and developer instructions from
`repo_scout.toml`. It implies `--no-input` and cannot be combined with
`--persist`, `--last`, or `--effort`. A custom subagent does not
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

`--last` restores the original working directory, effort and conversation
history. Two processes cannot use the same saved task at the same time. Other
saved tasks can run at the same time.

Run `mai` without a prompt to show concise usage text. Run `mai --help` for all
options.

## Choose reasoning effort

Use `-e` to choose the reasoning effort:

- `l` means low
- `m` means medium
- `h` means high
- `x` means extra high
- `max` means maximum

Each new task uses low effort unless you set `-e`. The
saved task keeps its values when you use `--last`.

You can use these options with a new or saved task:

```bash
mai "refactor this package" -e h
mai "continue the refactor" --last -e max
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

### Persistent Python

The optional `python` tool starts a Python subprocess on its first cell. Install
Python 3.9 or newer on `PATH`, or set `MAI_PYTHON` to a Python executable. An active
virtual environment works through `PATH`. Mai does not install Python packages.
For example, use `MAI_PYTHON=python3.14 mai "explore sales.csv"` to select Python 3.14.
Use `MAI_PYTHON=python3.14t` to select an installed free-threaded build. The default
remains `python3`.

Send `{"code":"..."}` to execute a cell or `{"reset":true}` to discard the
environment. Imports, variables, functions, and SQLite connections remain between
cells. The last expression is printed unless its value is `None`. Use standard
library `csv` and `sqlite3`, or packages already installed in the chosen environment.
For exploration, prefer read-only SQLite connections and selected summaries of
large datasets. Each cell uses the `--timeout` limit and the same 64 KiB per-stream
head-and-tail output limits as Bash. Tracebacks are part of stderr.

Cells support top-level `await`, `async for`, and `async with`. Sync and async
cells execute on the same Python thread, so SQLite connections remain usable.
One event loop persists between async cells; synchronous `asyncio.run(...)`
snippets still work. A returned awaitable is printed as a value unless the code
explicitly awaits it.

The preloaded `mai` module calls the existing Go tools:

```python
listing = await mai.bash("rg --files", timeout_ms=10_000)
paths = listing["stdout"].splitlines()
len(paths)
```

`await mai.apply_patch(patch)` applies a repository patch, and
`await mai.spawn_subagent(name, prompt)` returns a completed child result.
These calls use the same validation, approvals, and repository boundaries as
direct tool calls. Children cannot spawn children. Host operations run in
sequence, with at most eight pending requests and 64 calls per cell. Read-only
child status requests do not consume the 64-call budget. The bridge
accepts calls only from the cell's Python thread. It does not expose recursive
Python calls.

Use `mai.spawn` for background work when subagent use is authorized:

```python
first = await mai.spawn("repo_scout", "Inspect the storage code")
second = await mai.spawn("repo_scout", "Inspect the API code")
# Both children run independently, including between cells.
first_result = await first.wait()
second_result = await second.wait()
```

`await first.status()` returns the child ID, name, status, and terminal result
when available. `await first.cancel()` cancels and reaps the child, then returns
its terminal status and result. Cancellation is idempotent. Repeated waits return
the same result. Cancelling a wait, including with `asyncio.wait_for`, leaves the
child running. Waiting polls every 100 ms and does not block other host operations.

Go owns a maximum of four active background children and 64 retained background
child records per task, including across Python resets and persisted-task
resumes. Admission rejects overload; it never queues or retries a child. Start a
new task after the retained record limit is reached. Handles survive cells,
ordinary Python exceptions, and conversation compaction. Reset, kernel loss, parent
cancellation, and shutdown cancel and reap children. Each child has its own
`--timeout` deadline, independent of its spawning cell. Ordinary leftover Python
tasks are still cancelled at cell completion.

Persisted tasks keep a bounded `.children.json` journal beside the session file.
Admission is saved before the process starts, and completion is saved even when
Python is idle. A save failure prevents new admissions; an unsaved result has an
unknown outcome. On restart, no live handles are restored. Unfinished children
become unknown, and saved child IDs, statuses, and short result summaries appear
in task guidance. Inspect effects before deciding whether new work is safe;
Mai never relaunches a recovered child automatically.

State survives conversation compaction, but ends when Mai exits. `--last` restores
conversation history only; it starts a new Python environment. Results report the
kernel generation (local to this run), whether it is fresh, and whether state was
lost. Reset is lazy: the next cell starts the next generation. Save explicit files
for durable work; Mai does not snapshot variables or replay cells.

Results include the Python version, executable, and GIL status. Tracebacks use
cell filenames such as `<mai:g2:c7>`; a bounded source cache retains recent cell
text. In persisted tasks, Mai journals nested host calls before dispatch and
saves their results. The outer Python result includes bounded activity summaries;
large arguments and results are abbreviated, with omitted activities counted.
An interrupted pending operation has an unknown outcome on resume; it is never
replayed automatically.

An exception can leave partial changes in the namespace. A timeout, cancellation,
or kernel failure discards it and stops owned processes. Owner-lifetime watchers
also stop the Python, Bash, and child-agent process groups if Mai is forcibly
killed. Descendants that deliberately leave those process groups are outside this
cleanup. External effects can remain; failed cells are never retried automatically.
Python is not sandboxed and has Mai's OS access. Interactive input is unsupported.

Await all async work before returning. Mai cancels remaining cell tasks and waits
for active host calls to finish; the cell timeout discards a kernel that cannot
finish cleanup. Cells must finish scheduled callbacks, background threads, and
subprocess work before returning; output from work left running cannot be
attributed reliably. Native libraries must flush their own buffered output before
the cell returns.

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

The saved history includes completed responses and encrypted reasoning state.
`mai` sends a stable cache key for each task so compatible requests can reuse
cached input. `--last -e h` adds a `configuration_update` before the
new prompt and keeps the original request effort. Later resumes replay those
updates so an effort change can preserve the cached prefix. Cache hits still
depend on the backend and cache lifetime. Compacting history starts a new prompt
prefix at the selected effort.

Model tool calls and synchronous `spawn_subagent` calls run in sequence. Python
cells can await host tools, whose operations also run in sequence. Children
started with `mai.spawn` run concurrently. Mid-turn steering is not enabled.

Children never create their own saved tasks. A persisted parent saves the
`spawn_subagent` call and its returned result in the parent task history.

Mai tracks the active context size reported by the Codex backend. At 90% of the
configured context budget (272,000 tokens), it sends a Codex V2 compaction
request before the next response request. The compacted history keeps recent
user messages and the encrypted compaction item. Persisted tasks save this
replacement history before they continue.

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
