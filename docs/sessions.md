# Sessions and context

[Back to README](../README.md) · [CLI reference](cli.md) · [Tools and skills](tools.md)

```sh
mai "start the migration" --persist
mai "continue the migration" --last
```

## Saved tasks

`mai` has no global settings or session file. When you use `--persist`, it stores
project-local state under the repository root:

- `.mai/current` contains the current session ID
- `.mai/sessions/<session-id>.json` contains one task and its history
- `.mai/sessions/<session-id>.transcript.jsonl` archives compacted visible history when needed
- `.mai/locks/<session-id>.lock` prevents concurrent use of one task

For a directory outside Git, `.mai` is stored in the working directory. Mai
creates `.mai/.gitignore` so Git does not add the saved state. State directories
use permission mode `0700`. State files use mode `0600`.

Saved state must use the supported format. If a saved task has an incompatible
older format, start a new task.

Each persisted task has a separate session file. Starting concurrent tasks does
not replace their history. An atomic update to `current` selects the task that a
later `--last` command will resume.

Saved history includes completed responses and plain DeepSeek reasoning.
Reasoning is replayed across tool turns; opaque encrypted history is unsupported.
Effort changes are sent at the request's top level, without rewriting earlier
history. DeepSeek caching is automatic and depends on matching prefixes and
cache lifetime. Mai does not send server-side conversation IDs or cache keys.

Model tool calls run in sequence. Python cells can await host tools, whose
operations also run in sequence. Mid-turn steering is not enabled.

Mai tracks the active context size reported by DeepSeek. The default budget is
1,000,000 tokens; `MAI_CONTEXT_WINDOW` can lower it.
At 80% of the budget, Mai builds a portable checkpoint before the next model
request. Saved tasks commit replacement history before continuing. Original
visible text is archived separately for `mai.history` and `--last`; the archive
grows with the task.

## Context editing

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

The tool edits only successful Bash stdout. It preserves stderr,
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

## DeepSeek backend

Selecting `ds-flash` or `ds-pro` automatically uses
`https://api.deepseek.com/responses`. No provider flag is needed.
Set `DEEPSEEK_API_KEY` in your environment before running:

```sh
mai "add useful tests for the parser" --f --persist
mai "continue the task" --last --max
mai "review this implementation" --max
```

| CLI mode | API model | Effort |
| --- | --- | --- |
| Default | `deepseek-v4-pro` | `high` |
| `--max` | `deepseek-v4-pro` | `max` |
| `--f` | `deepseek-flash` | `high` |

Responses stream through the existing text and function-tool flow. Mai replays
the full local history, including DeepSeek's plain reasoning between tool calls;
Mai does not rely on server-side conversation state. Each request allows up to
32,768 output tokens, including reasoning. Credentials stay in the environment.
Saved tasks retain their model and effort, including older low-effort tasks.
Use `--last --f` to switch to Flash/high, `--last --max` to switch to Pro/max,
or `--last -m ds-pro` to switch to Pro/high. Pro rejects image-bearing history.

Mai builds portable checkpoints automatically, at 80% of a default
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

## Portable compaction

Mai uses readable, model-authored continuity checkpoints. The Go context owner
keeps the current user turn and exact tool results. Checkpoints preserve goals,
corrections, unresolved outcomes and history search anchors. Consecutive
identical log lines are encoded with occurrence counts; unique records are
folded in bounded chunks. Summary requests cannot execute tools. Invalid
summaries leave session state unchanged.

Keep full history while it fits. Checkpoints can add generation time and reduce
prompt-cache reuse, so smaller requests do not automatically mean lower cost.
