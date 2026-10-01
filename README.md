# mai

![mai project banner](assets/mai-banner.png)

`mai` is a small coding agent for macOS and Linux. It uses your existing Codex
ChatGPT login by default, so you do not need an OpenAI API key. An experimental
Go Chat Completions backend supports other providers with portable compaction.

The agent has 7 tools:

- `bash` reads files, searches code and runs commands
- `python` explores data and task history in a persistent Python namespace
- `apply_patch` creates, changes, moves and deletes files
- `read_skill` loads a skill's complete `SKILL.md`, or a required supporting file when `file` is provided
- `view_image` shows a local image to the model
- `spawn_subagent` runs one installed custom agent and returns its final output
- `edit_context` shortens successful Bash stdout in future model requests while keeping the original searchable

Skills are read only from `~/.agents/skills`. Each main-agent request includes
all eligible skill names and descriptions, then loads a complete `SKILL.md` only
when needed. Each skill description must be 1,024 characters or fewer.
Set `policy.allow_implicit_invocation` to `false` in `agents/openai.yaml` to hide
a skill from automatic selection; an explicit `$skill-name` still loads it.
Use `-s` or `--skip-skills` to skip skill discovery for one run, including
resolution of explicit `$skill-name` mentions. The flag also works with
`--last` and must be passed again on each resumed run. The `read_skill` tool
remains available when you know a skill's directory id.
Images use typed image output; other binary files are rejected.

Custom agents are read from `$CODEX_HOME/agents`, or `~/.codex/agents` when
`CODEX_HOME` is not set. Each `<name>.toml` file must define matching `name`,
`description`, and `developer_instructions` fields, plus a reasoning effort.

Choose an installed agent, such as `implementor` for an assigned code change or
`verifier` for an independent review. Run a custom agent directly with:

```bash
mai --subagent verifier "Review the current diff against the task contract"
```

This mode uses the model, effort, and developer instructions from the selected
agent's TOML file. It implies `--no-input` and cannot be combined with
`--persist`, `--last`, `--effort`, or `--model`. A custom subagent does not
receive the general skill catalog. Its developer instructions must identify any
required skills.

`mai` uses server-sent events (SSE). The Go standard library provides everything
it needs, so the project has no third-party dependencies.

## Requirements

You need:

- Go 1.27 or later
- for the default backend: a ChatGPT account with Codex access and the Codex CLI
- for the experimental chat backend: a supported Chat Completions endpoint and API key

For the default backend, log in before you run `mai`:

```bash
codex login
```

## Build and install mai

```bash
cd ~/Codehub/mai
go build -o mai ./cmd/mai
```

Move the `mai` binary to a directory in your `PATH` if you want to run it from
any directory.

To install it into your Go binary directory instead, run:

```bash
go install ./cmd/mai
```

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

`--last` restores the original working directory, model, effort, and conversation
history. Two processes cannot use the same saved task at the same time. Other
saved tasks can run at the same time.

Run `mai` without a prompt to show concise usage text. Run `mai --help` for all
options.

## JSONL events

Use `--jsonl` to write one JSON event per line on standard output:

```bash
mai "add tests for the parser" --jsonl > run.jsonl
```

Events include `task.started`, `model.started`, `model.delta`,
`model.completed`, `model.failed`, `compaction.completed`, `tool.started`, `tool.completed`,
`task.completed`, and `error`. Model text is
reported in `model.delta` events instead of being printed directly. Progress
messages remain on standard error. A `tool.completed` event includes its output
up to 256 KiB; larger outputs report `output_bytes` and `output_omitted` instead.
Completed model, tool, and task events include `duration_ms` for elapsed time.
Completed model events also include `input_tokens`, `output_tokens`, and
`cached_input_tokens` when the backend supplies them. Missing fields mean
unavailable; `total_tokens` keeps its existing context-size meaning.
`compaction.completed` includes elapsed time and a `usage` object with the
backend's available token fields, so native compaction work can be counted too.
Model duration covers the full Codex request, including any internal retry.
Tool duration covers execution of that call; task duration covers the agent run.
The default output remains human-readable.

## Choose a model

New tasks use `gpt-6-luna` by default. Select `gpt-6-sol` with `-m sol` or
`--model gpt-6-sol`. Saved tasks retain their selected model; pass `-m` with
`--last` to change it. Older saved tasks that used another model switch to Luna
when resumed.

Use `-m gpt-6.1-sol -e m` for GPT-6.1 Sol with medium effort.

Custom agents may set `model` to `gpt-6-sol`, `gpt-6.1-sol` or `gpt-6-luna` in their TOML
file. When omitted, they use Luna.

```bash
mai "complex refactor" -m sol
mai "continue the refactor" --last -m luna
```

### Portable compaction and other providers (experimental)

Set `MAI_COMPACTION=portable` to use readable, model-authored continuity
checkpoints instead of Codex encrypted compaction. The Go context owner keeps
the current user turn and original transcript, including exact tool results.
Checkpoints contain goals, corrections, unresolved outcomes and history search
anchors. Consecutive identical log lines are encoded with their occurrence
counts before summarization; unique records are folded in bounded chunks.
Summary requests cannot execute tools. Invalid summaries leave session state
unchanged. Native remains the default for existing Codex tasks.

The opt-in Chat Completions adapter can run both coding and compaction without
an OpenAI account. Supply a complete endpoint URL, model, key-variable name,
and a conservative input context budget appropriate to that model:

```sh
MAI_PROVIDER=chat \
MAI_CHAT_URL=https://api.deepseek.com/chat/completions \
MAI_CHAT_MODEL=deepseek-flash \
MAI_CHAT_KEY_ENV=DEEPSEEK_API_KEY \
MAI_CONTEXT_WINDOW=65536 \
mai "add useful tests for the parser" --persist
```

The named key must already be in the environment. It is never copied into task
state. Use the same endpoint and model when resuming; saved chat tasks reject a
different backend. `-m` and reasoning-effort selections apply to Codex; the
chat model is selected through `MAI_CHAT_MODEL`. Chat automatically uses
portable compaction at 90% of `MAI_CONTEXT_WINDOW` and buffers each response.
The adapter supports text and function tools, not images, provider-specific
thinking state, or every extension to the Chat Completions protocol. DeepSeek
thinking is explicitly disabled. Native encrypted checkpoints cannot migrate
to this backend; start a new portable task. An active turn exceeding the budget
still needs context editing or a smaller tool output: portable compaction does
not discard pending calls or silently truncate that turn.

## Choose reasoning effort

Use `-e` to choose the reasoning effort:

- `l` means low
- `m` means medium
- `h` means high
- `x` means extra high
- `max` means maximum

Each new task uses medium effort unless you set `-e`. The
saved task keeps its values when you use `--last`.

You can use these options with a new or saved task:

```bash
mai "refactor this package" -e h
mai "continue the refactor" --last -e max
```

## Timeouts and interactive input

Each Codex request has a 10-minute time-to-first-byte and idle timeout. Each
received stream chunk restarts the idle timer, so an active response can run
longer than 10 minutes. Set a different positive Go-style duration when necessary:

```bash
mai "investigate the failure" --timeout 20m
```

Mai retries transient Codex request failures with a bounded delay. It respects
the server's `Retry-After` header up to a five-second cap. Authentication,
quota, and other permanent failures are returned without retrying. A failed
stream is not retried after Mai has printed any of its text.

Each Python cell has a separate 10-minute wall-clock limit, and each subagent
has a separate one-hour wall-clock limit. Change them with `--cell-timeout` and
`--subagent-timeout`; `--timeout` still controls each subagent request. A parent
runs one child at a time. The child uses the parent's working directory, starts
with new in-memory history, and cannot spawn another child. The parent receives
the child's final standard output. Failed calls also include the child's
standard error.

Each `bash` result reports its duration and original output byte counts. Mai
keeps at most 64 KiB from each stream. For longer output, it preserves the
beginning and end and reports the omitted byte count. When a stream is
truncated, the result includes a `stdout_capture_path` or
`stderr_capture_path` to a private temporary file containing the complete
stream up to a 32 MiB per-stream capture limit. `capture_truncated` reports
when that limit is reached, and `capture_error` reports a disk capture failure.

Use `--no-input` in scripts and other non-interactive environments. If a command
needs approval, `mai` rejects it instead of opening a terminal prompt.

The `view_image` tool reads a PNG, JPEG, or GIF inside the repository and sends
it as typed image content to the model. Files are limited to 8 MiB and 8,192
pixels per side.

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
large datasets. Each cell uses the `--cell-timeout` limit and the same 64 KiB per-stream
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

Search this task's visible conversation and tool history from Python:

```python
found = await mai.history("release date", limit=5)
for item in found["matches"]:
    print(item["index"], item["kind"], item["text"])
```

To continue when a query has more than one page of matches:

```python
page = await mai.history("release date")
while True:
    for item in page["matches"]:
        print(item["index"], item["text"])
    if page.get("next") is None:
        break
    page = await mai.history("release date", start=page["next"])
```

Search uses a case-insensitive literal substring. Start with short distinctive
text, then refine. The query `recovery code` can find "recovery code for ticket
H-1"; `recovery code ticket` cannot. Search returns up to 20 matches in task
order, a `total` match count, an optional `next` index, and each entry's index
in the current transcript. `start` is an inclusive,
zero-based transcript index. Each text excerpt is at most 2,048 Unicode code
points. Prompts, assistant text, tool calls, and tool results remain searchable
after conversation compaction and `--last`. Opaque reasoning, compaction
payloads, and image data are excluded. A saved task created before this feature
can search its current history; content removed by an earlier compaction cannot
be recovered. Search results are copies, so editing one does not change Mai's
history.

For saved tasks, compacted visible text is stored in a `.transcript.jsonl` file
beside the session JSON. Older saved tasks migrate their inline transcript on
the next compaction. Keep both files when moving or backing up a saved task.

`await mai.apply_patch(patch)` applies a repository patch, and
`await mai.spawn_subagent(name, prompt)` returns a completed child result.
These calls use the same validation, approvals, and repository boundaries as
direct tool calls. Children cannot spawn children. Host operations run in
sequence, with at most eight pending requests and 64 effectful calls per cell.
Read-only history searches and child status requests do not consume that budget.
The bridge accepts calls only from the cell's Python thread. It does not expose
recursive Python calls.

Use `mai.spawn` for background work when subagent use is authorized:

```python
first = await mai.spawn("verifier", "Review the storage changes against the task contract")
second = await mai.spawn("verifier", "Review the API changes against the task contract")
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
`--subagent-timeout` deadline, independent of its spawning cell. Ordinary leftover Python
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
replacement history before they continue. Mai archives visible text separately
so Python can search it after compaction or `--last`; the saved archive grows
with the task's visible history.

### Native context editing

Mai provides a native Go `edit_context` tool. The model can inspect eligible
outputs, then propose a shorter stdout summary using the returned call ID and
digest. Each batch accepts at most eight edits. Summaries must contain 1 to
16,384 bytes and reduce estimated request size. Digests reject stale edits, and
Mai saves an accepted batch before using it. At 50% of the context budget, a
request-tail reminder supplies current handles for up to eight outputs since
the last assistant answer, with at least 16 KiB of stdout. The model can shrink
these directly, then continue the task. The reminder leaves the system
instructions and preceding history intact; actual cache reuse still depends on
which output is edited.

This first version edits only successful Bash stdout. It preserves stderr,
exit status, capture paths, call IDs, item order, user instructions, and opaque
reasoning and compaction items. Failed, timed-out, interrupted, and other tool
outputs are not eligible. Summaries are explicitly marked as model-authored;
they are not fresh tool evidence. Mai validates structure and size, not whether
the model preserved every useful fact.

The session stores original history plus a separate projection. `mai.history`
continues to search original stdout, including after `--last` and native
compaction. Compaction uses the projected request, archives the original visible
history, and clears the superseded projection. No command is undone or replayed.
No Node runtime or bridge is involved.

Context editing can change prompt-cache reuse and add model calls. Reduced
request size alone does not establish faster or cheaper task completion. Native
Codex compaction remains the fallback at the existing threshold.

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

To run the live history-recall eval with your Codex login:

```bash
MAI_LIVE_HISTORY_EVAL=1 go test -v ./internal/mai -run '^TestLiveHistoryRecall$' -count=1
```

To check Codex compatibility with shortened stdout and real call/reasoning items:

```bash
MAI_LIVE_CONTEXT_EDIT_EVAL=1 go test -v ./internal/mai -run '^TestLiveContextEditCompatibility$' -count=1
```

This probe supplies a synthetic successful Bash result and executes no
model-proposed command. It checks backend acceptance and exact code retention,
not long-task performance.

In a three-pair local run, Mai recalled 3/3 random codes from archived history
and 0/3 without it. This small probe seeds the archive in memory; deterministic
tests cover two compactions and saved-task resume. See [eval details](evals/README.md#searchable-history-recall).

## Backend status

`mai` uses the ChatGPT Codex backend rather than the public OpenAI API. This is
suitable for personal use with your own ChatGPT subscription.

The backend is not a public API contract. OpenAI may change it without notice.
