# mai

![mai project banner](assets/mai-banner.png)

Mai is a small coding agent for macOS and Linux, built in Go. It uses the
DeepSeek Responses API for streaming, reasoning and function tools. Portable
checkpoints keep long tasks moving while original history remains searchable.

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

Custom agents are read from `~/.agents/agents`, or the directory specified by
`MAI_AGENTS_DIR`. Each `<name>.toml` file must define matching `name`,
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

The DeepSeek Responses backend uses server-sent events (SSE). The Go standard library
provides everything it needs, so the project has no third-party dependencies.

## Requirements

You need Go 1.27 or later and a populated `DEEPSEEK_API_KEY` environment
variable. Python 3.9 or later is optional for the persistent Python tool.
No Node runtime, SDK or third-party Go dependency is required.

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
backend's available token fields, so checkpoint generation can be counted too.
Model duration covers the full model request.
Tool duration covers execution of that call; task duration covers the agent run.
The default output remains human-readable.

## Choose a model

New tasks use `ds-flash` with high effort. Select `ds-pro` with `-m ds-pro`.
Use `--last -m ds-pro` to change the model for a saved task.

### DeepSeek Responses

Selecting `ds-flash` or `ds-pro` automatically uses
`https://api.deepseek.com/responses`. No provider flag is needed.
Set `DEEPSEEK_API_KEY` in your environment before running:

```sh
mai "add useful tests for the parser" -m ds-flash -e l --persist
mai "continue the task" --last -e h
mai "review this implementation" -m ds-pro -e max
```

| CLI model | API model | Reasoning efforts | Default effort |
| --- | --- | --- | --- |
| `ds-flash` | `deepseek-flash` | `l` / `low`, `h` / `high`, `max` | `high` |
| `ds-pro` | `deepseek-v4-pro` | `l` / `low`, `h` / `high`, `max` | `high` |

Custom agents may use these aliases with `model_reasoning_effort = "low"`, `"high"`, or `"max"`.

Responses stream through the existing text and function-tool flow. Mai replays
the full local history, including DeepSeek's plain reasoning between tool calls;
the API does not retain conversations. Each request allows up to 32,768 output
tokens, including reasoning. Credentials stay in the environment.
Saved tasks retain their model and effort. Use `--last -m ds-pro` to switch
models within DeepSeek; Pro rejects image-bearing history.

DeepSeek uses portable compaction automatically, at 80% of a default
1,000,000-token budget. Set `MAI_CONTEXT_WINDOW` to a smaller input budget
between 32,768 and 1,000,000 if needed.
`MAI_DEEPSEEK_URL` overrides the complete Responses URL for local testing.
HTTPS is required except for loopback.

Flash accepts inline image output from `view_image` and `read_skill`. On Pro,
both tools send images to Flash for a task-relevant description and return only
labeled text to Pro. Each image adds one Flash request at low reasoning effort,
using the configured endpoint and request timeout. Descriptions include source
metadata and usage when available, and mark their interpretation as unverified.
Description failures return text tool errors without adding images to Pro history.
Responses image parts can reference existing Files API `file_id`
values, but Mai does not upload or manage remote files. Portable checkpoints
currently handle text histories: an older image-bearing message or tool output
cannot be silently compacted and requires a new task or a text-only history.

KV caching is automatic on DeepSeek. Mai sends no unsupported cache-key or
server conversation fields, and reports cached input tokens when supplied.
See the [Responses guide](https://api-docs.deepseek.com/guides/responses_api/),
[thinking guide](https://api-docs.deepseek.com/guides/thinking_mode/), and
[cache behavior](https://api-docs.deepseek.com/guides/kv_cache/).

### Portable compaction

Mai uses readable, model-authored continuity checkpoints. The Go context owner
keeps the current user turn and exact tool results. Checkpoints preserve goals,
corrections, unresolved outcomes and history search anchors. Consecutive
identical log lines are encoded with occurrence counts; unique records are
folded in bounded chunks. Summary requests cannot execute tools. Invalid
summaries leave session state unchanged.

Keep full history while it fits. Checkpoints can add generation time and reduce
prompt-cache reuse, so smaller requests do not automatically mean lower cost.

## Choose reasoning effort

Use `-e l` / `low`, `-e h` / `high`, or `-e max`. New tasks default to
high effort. A saved task keeps its current effort on `--last` unless you
override it:

```sh
mai "refactor this package" -e h
mai "continue the refactor" --last -e max
```

## Timeouts and interactive input

Each DeepSeek Responses request has a 10-minute time-to-first-byte and idle timeout. Each
received stream chunk restarts the idle timer, so an active response can run
longer than 10 minutes. Set a different positive Go-style duration when necessary:

```bash
mai "investigate the failure" --timeout 20m
```

Failed requests return an error. Mai does not automatically replay a failed
request or tool call.

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

Mai reads `DEEPSEEK_API_KEY`. Credentials are not copied into saved task state
or logs.

## Saved tasks

`mai` has no global settings or session file. When you use `--persist`, it stores
project-local state under the repository root:

- `.mai/current` contains the current session ID
- `.mai/sessions/<session-id>.json` contains one task and its history
- `.mai/locks/<session-id>.lock` prevents concurrent use of one task

For a directory outside Git, `.mai` is stored in the working directory. Mai
creates `.mai/.gitignore` so Git does not add the saved state. State directories
use permission mode `0700`. State files use mode `0600`.

Saved state uses the current format only. Older task files are unsupported;
start a new task after this state-format change.

Each persisted task has a separate session file. Starting concurrent tasks does
not replace their history. An atomic update to `current` selects the task that a
later `--last` command will resume.

Saved history includes completed responses and plain DeepSeek reasoning.
Reasoning is replayed across tool turns; opaque encrypted history is unsupported.
Effort changes are sent at the request's top level, without rewriting earlier
history. DeepSeek caching is automatic and depends on matching prefixes and
cache lifetime. Mai does not send server-side conversation IDs or cache keys.

Model tool calls and synchronous `spawn_subagent` calls run in sequence. Python
cells can await host tools, whose operations also run in sequence. Children
started with `mai.spawn` run concurrently. Mid-turn steering is not enabled.

Children never create their own saved tasks. A persisted parent saves the
`spawn_subagent` call and its returned result in the parent task history.

Mai tracks the active context size reported by DeepSeek. The default budget is
1,000,000 tokens; `MAI_CONTEXT_WINDOW` can lower it.
At 80% of the budget, Mai builds a portable checkpoint before the next model
request. Saved tasks commit replacement history before continuing. Original
visible text is archived separately for `mai.history` and `--last`; the archive
grows with the task.

### Context editing

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
exit status, capture paths, call IDs, item order, user instructions and
reasoning items. Failed, timed-out, interrupted, and other tool
outputs are not eligible. Summaries are explicitly marked as model-authored;
they are not fresh tool evidence. Mai validates structure and size, not whether
the model preserved every useful fact.

The session stores original history plus a separate projection. `mai.history`
continues to search original stdout, including after `--last` and
compaction. Compaction uses the projected request, archives the original visible
history, and clears the superseded projection. No command is undone or replayed.
No Node runtime or bridge is involved.

Context editing can change prompt-cache reuse and add model calls. Reduced
request size alone does not establish faster or cheaper task completion.
At the context threshold, Mai builds a portable checkpoint.

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

With `DEEPSEEK_API_KEY` populated, run the paid Responses conformance probe:

```sh
MAI_LIVE_DEEPSEEK_RESPONSES=1 \
go test -v ./internal/mai -run '^TestLiveDeepSeekResponses$' -count=1 -timeout=10m
```

All six Flash/Pro combinations at low, high and max passed the local live
probe: tool execution, saved-task resume and exact checkpoint fact retention.
This is protocol conformance, not a coding-performance benchmark.
See [eval instructions](evals/README.md) for graded repository tasks and
context-continuity tests.
