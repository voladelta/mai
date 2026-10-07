# Sessions and context

[Back to README](../README.md) · [CLI reference](cli.md) · [Tools and skills](tools.md)

```sh
mai "start the migration" --persist
mai "continue the migration" --last
```

## Saved tasks

Provider settings live in `.mai.config` in the working directory or home;
there is no global session file. When you use `--persist`, Mai stores
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

## Forking a task

`--fork` copies the project's current saved task into a new session;
`--fork-from ID` forks a specific session in the project. The child gets a new
session ID, records `parent_id` and `forked_at_turn`, and duplicates the
parent's committed transcript prefix so `mai.history` still reaches pre-fork
entries. The parent's file and lock are untouched, so it can keep running.
Forked tasks are saved like `--persist` tasks and move `current` to the child;
they cannot be combined with `--last` or `--persist`. `--provider`, `--model`,
and `--effort` override the inherited values; a provider switch drops the saved
backend settings, and a provider or model change excludes earlier reasoning
from requests, as with `--last`.

Saved history includes completed responses and provider reasoning. Native
DeepSeek reasoning is stored as plain text. Other Responses providers can replay
their summary or encrypted reasoning. Provider overrides exclude previous
reasoning from requests while keeping original history intact.
Reasoning effort is sent at the request's top level; `--last` keeps its saved
value. DeepSeek caching is automatic and depends on matching prefixes and cache
lifetime. Mai does not send server-side conversation IDs or cache keys.

Model tool calls run in sequence. Python cells can await host tools, whose
operations also run in sequence. Mid-turn steering is not enabled.

Mai tracks the active context size reported by the provider. The default budget is
1,000,000 tokens; `MAI_CONTEXT_WINDOW` can lower it.
At 80% of the budget, Mai builds a portable checkpoint before the next model
request. Saved tasks commit replacement history before continuing. Original
visible text is archived separately for `mai.history` and `--last`; the archive
grows with the task.

## Providers and model modes

Enclave is the built-in default at `https://router.enclave.ai/v1/responses`.
Set `ENCLAVE_API_KEY` in your environment before running, or configure another
provider in [`.mai.config`](../.mai.config.example). Local configuration takes
priority over `$HOME/.mai.config`, without merging. `--provider` overrides the
configured default for a new task; resume uses its saved settings.

```sh
mai "add useful tests for the parser" --persist
mai "continue the task" --last
mai "review this implementation" --effort max
mai "quick review" --provider enclave --model cyberouter/glm-5.3-flash
```

The built-in Enclave provider uses `cyberouter/deepseek-v4.1-flash`; configured providers set
their own `model` ID, sent upstream exactly as written. `--model`/`-m` overrides
the model and `--effort` selects `l`, `h`, or `max` reasoning (default `h`).

Responses stream through the existing text and function-tool flow. Mai replays
the full local history, including provider reasoning between tool calls;
Mai does not rely on server-side conversation state. Each request allows up to
32,768 output tokens, including reasoning. Credentials stay in the environment.
Saved tasks record the selected provider, protocol profile, endpoint, credential
environment-variable name, model, and effort.
The profile determines effort translation and reasoning handling independently
of the provider name.
Older snapshots without a profile infer it from the saved provider name:
`deepseek`, `openrouter`, or `responses` for other names.

New tasks use the provider's configured model unless `--model` overrides it.
Plain resume restores the saved settings and ignores current config files.
`--last --provider NAME` loads current config to switch providers and adopts
the new provider's configured model, since a saved model ID is
provider-specific; `--model` and `--effort` override the saved values on resume.
The new provider settings are saved for later runs.
Previous reasoning remains in original history but is excluded from requests to
the new backend; visible messages and tool relationships survive.
API keys are read afresh from the saved environment-variable name.
Sessions saved before backend settings were recorded use the
built-in default provider's endpoint. Saved sessions from older state versions are rejected;
start a new task to continue.

Mai builds portable checkpoints automatically, at 80% of a default
1,000,000-token budget. Set `MAI_CONTEXT_WINDOW` to a smaller input budget
between 32,768 and 1,000,000 if needed.
`MAI_BASE_URL` overrides the built-in default provider's complete Responses URL for local testing.
HTTPS is required except for loopback.

`view_image` and `read_skill` return typed image content that is replayed with
the rest of the history, so the configured model must accept inline images.
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
