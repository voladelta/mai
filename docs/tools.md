# Tools and skills

[Back to README](../README.md) · [CLI reference](cli.md) · [Sessions and context](sessions.md)

Mai exposes eight general tools.
Tools are selected by the model; Python examples below are cells sent to Mai's
`python` tool, not commands for a standalone Python interpreter.

## Tool reference

| Tool | Input | Behavior |
| --- | --- | --- |
| `bash` | `command`, optional `description`, `timeout_ms` | Run a fresh `bash -c` in the task working directory. `description` is an optional label shown in the progress line. |
| `read` | `file_path`, optional `offset`, `limit` | Read numbered UTF-8 text lines and observe the file version. Paths are relative to the repository root. |
| `write` | `file_path`, `content` | Create or fully replace a repository UTF-8 text file. Existing files require a fresh observation. |
| `edit` | `file_path`, `old_string`, `new_string`, optional `replace_all` | Replace literal text in an observed repository file. |
| `python` | `code` or `reset: true` | Execute a cell in the persistent namespace, or discard that namespace. |
| `read_skill` | `path`, optional `file` | Read a discovered skill by directory id; `file` defaults to `SKILL.md`. Disabled by `-s` or `--skip-skills`. |
| `view_image` | `path` | Read a repository image using an absolute path or a path relative to the working directory. |
| `edit_context` | `action: "inspect"` or `action: "shrink"`, with `edits` for shrinking | Inspect eligible Bash outputs and shorten their future request representation. |

For example, a Bash tool call uses:

```json
{"command":"git diff --check","timeout_ms":10000}
```

See [Context editing](sessions.md#context-editing) for edit eligibility and
validation. Bash output, Python state, and skill discovery
are described below.

## Reading and editing files

File tool paths are relative to the repository root, even when Mai starts in a
subdirectory. Files and mutation content are limited to 16 MiB. Only regular
UTF-8 text files without NUL bytes are supported; file symlinks and special
files are rejected. Directory aliases inside the repository share a target
identity; aliases leading outside it are rejected.

`read` uses a 1-based `offset` (default 1) and a `limit` of 1–2,000 lines
(default 2,000). A successful read is plain text:

```text
<path>src/main.go</path>
<type>file</type>
<content>
1: package main
2: ...

(Showing lines 1-2 of 40. Use offset=3 to continue.)
</content>
```

The footer is one of `(Showing lines a-b of N. Use offset=M to continue.)`,
`(Output capped. Showing lines a-b. Use offset=M to continue.)` when the 50 KiB
(51,200-byte) output cap ends the window, or `(End of file - total N lines)`.
Lines longer than 2,000 characters end with `... (line truncated to 2000 chars)`;
inspect the remainder through Bash. An offset beyond the file fails with
`offset N is out of range for "path" (T lines)`. Even a partial read observes the
complete file version. Reading a missing file fails with
`cannot read "path": not found` and records that the path is absent: a later
`write` creates it, while `edit` reports not found.

Before replacing or editing an existing file, use `read`. Bash reads do not
record an observation. A successful `write` or `edit` observes its result,
allowing another mutation without rereading. Observations are scoped to the
task and reset on resume; saved history does not authorize
new mutations. A file changed since observation returns `FS_STALE_VERSION`;
reread it and reconcile the intended change before retrying. New files need
no prior read. Creation refuses to overwrite a file that appears concurrently.

`write` accepts complete contents, including an empty string. `edit` requires
nonempty literal `old_string` and a different `new_string`; an empty
replacement deletes the match. Zero matches return `FS_EDIT_NOT_FOUND`.
Multiple matches return `FS_AMBIGUOUS_EDIT` unless `replace_all` is true.
Whitespace matches exactly. Edits normalize CRLF/LF for matching and restore
the original line-ending style. Full writes store the supplied contents.

Results are plain text. `write` returns the envelope above with `Created file`
or `Updated file` as its content; `edit` returns
`The file <path> has been updated successfully.` or, with `replace_all`,
`The file <path> has been updated. All occurrences were successfully replaced.`
Failures are `Error: <message>`, for example
`cannot modify "path": file has not been read — read the file, then retry` or
`cannot modify "path": file changed since it was read — re-read the file, then retry`.
Python's `mai.read`, `mai.write`, and `mai.edit` return structured dictionaries
(`ok`, `code`, `error`, `content`, `total_lines`, …) instead of this text;
a Python `mai.read` past the end of the file returns empty `content` rather than an error.

For example, after reading `config.go`:

```json
{"file_path":"config.go","old_string":"func StartupTimeout() int { return 30 }","new_string":"func StartupTimeout() int { return 45 }"}
```

Mutations preserve existing permission bits and publish complete contents
atomically. Mutations take a lock for each target.
Freshness is rechecked immediately before publication; external programs do
not participate in these locks and can still race a replacement after that
check. Each call commits one file. Use Bash for moves and deletes.

Interrupted mutations have an unknown outcome: inspect the
target with `read` before retrying rather than automatically replaying a call.

## Skills

Skills are enabled by default and read from `agents/skills` in the current
repository, then from
`~/.agents/skills`. Valid repository skills take priority when a directory id or
skill name matches. Invalid skills produce warnings and are skipped, allowing a
valid global copy to be selected. Later `read_skill` calls for a discovered id
use that selected root, including supporting files. When launched from a
subdirectory, Mai uses the Git repository root; outside Git, it uses the current
working directory. Missing skill directories are ignored. Each model request
includes all eligible skill names and descriptions, then loads a complete
`SKILL.md` only when needed. Each skill description must be 1,024 characters or fewer.
Set `disable-model-invocation: true` in `SKILL.md` YAML front matter to hide
a skill from automatic selection; an explicit `$skill-name` still loads it.
Omitting the field or setting it to `false` allows automatic selection unless
`policy.allow_implicit_invocation` is `false` in `agents/openai.yaml`.
Use `-s` or `--skip-skills` to skip skill discovery for one run, including
resolution of explicit `$skill-name` mentions. The flag also works with
`--last` and must be passed again on each resumed run. When disabled, the
`read_skill` tool is omitted from requests and rejects unexpected calls.
Images use typed image output; other binary files are rejected.

Quote explicit skill mentions with single quotes so the shell does not expand
the dollar sign:

```sh
mai 'Use $my-skill to review this package'
mai "review this package" --skip-skills
```

To define a repository skill, create `agents/skills/my-skill/SKILL.md`:

```markdown
---
name: my-skill
description: Review this project's Go packages for parser edge cases.
disable-model-invocation: true
---

# Parser review

Inspect empty input, invalid tokens, and boundary values. Report actionable
findings with file locations. Change files only when the user requests edits.
```

This example requires an explicit `$my-skill` mention. Remove
`disable-model-invocation: true` to allow automatic selection, unless the
skill's `agents/openai.yaml` policy disables it.

## Bash and images

Bash runs through `/bin/bash` in the task working directory with
`NO_COLOR=1 TERM=dumb PAGER=cat GIT_PAGER=cat` set so pagers and colors do not
garble captured output. Its default
wall-clock timeout is two minutes; `timeout_ms` can raise it to ten minutes.

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

For images up to one million pixels, `solid_color` reports an exact hex color
only when every pixel is the same fully opaque color. Mixed-color and
transparent images omit it. This is decoded pixel evidence, independent of
the model's visual interpretation.

## Persistent Python

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

`await mai.read(file_path, offset=1, limit=2000)`,
`await mai.write(file_path, content)`, and
`await mai.edit(file_path, old_string, new_string, replace_all=False)` call the
Go file tools; these do not write files through Python. Host calls use the
same validation, approvals, and repository boundaries as direct tool calls.
Host operations run in sequence, with at most eight pending requests and 64
budgeted calls per cell, including file reads.
Read-only history searches do not consume that budget.
The bridge accepts calls only from the cell's Python thread. It does not expose
recursive Python calls.

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
also stop the Python and Bash process groups if Mai is forcibly
killed. Descendants that deliberately leave those process groups are outside this
cleanup. External effects can remain; failed cells are never retried automatically.
Python is not sandboxed and has Mai's OS access. Interactive input is unsupported.

Await all async work before returning. Mai cancels remaining cell tasks and waits
for active host calls to finish; the cell timeout discards a kernel that cannot
finish cleanup. Cells must finish scheduled callbacks, background threads, and
subprocess work before returning; output from work left running cannot be
attributed reliably. Native libraries must flush their own buffered output before
the cell returns.

## Delegation through the CLI

When delegation is authorized, the model can launch another ordinary Mai run
through `bash` or Python's `subprocess`. Install `mai` on `PATH`, or use the
binary's absolute path. For example:

```bash
mai --no-input --max-turns 32 --model cyberouter/glm-5.3-flash -- "Act as a reviewer. Inspect the current diff, report actionable findings, and do not change files."
```

Supply a complete task, role, and file scope in the prompt. Each run starts
with fresh conversation history and discovers skills normally. It inherits
the working directory and environment, including API credentials. Model,
effort, and turn limits use CLI defaults unless passed explicitly. Choose
`-m`/`--model` and `--effort` explicitly to avoid relying on the configured
defaults.

Use stateless runs for delegation so they do not change `.mai/current` or
contend for the parent's saved task. Wait for completion, capture stdout and
stderr, and inspect effects before retrying an interrupted run. For concurrent
work, give each run separate file ownership or a separate workspace.

Nested runs share the enclosing tool's lifetime: Bash defaults to two minutes
and allows up to ten minutes through `timeout_ms`; Python uses `--cell-timeout`.
The nested run's `--timeout` controls its model requests and does not extend
the enclosing tool's deadline. Python subprocesses must finish before the cell
returns. Mai provides no dedicated role configurations, background handles,
or child journals for these ordinary CLI runs. Nested runs are depth-limited
by the harness: `MAI_DEPTH` counts the nesting level (0 for a top-level run)
and `MAI_MAX_DEPTH` (default 2) is the limit; a run deeper than the limit
refuses to start.

## Safety

`read`, `write`, and `edit` operate only inside the repository. They reject
file symlinks and directory aliases that lead outside the repository.

`bash` can run any command available to your shell. It is not sandboxed.

`mai` checks recognisable `rm` commands before it runs them. It asks for approval
when a target is outside the repository or cannot be resolved safely.

Approval is only interactive when standard input is a terminal and `--no-input`
is not set.

This check only covers `rm`. Other shell commands can still delete or overwrite
data.
