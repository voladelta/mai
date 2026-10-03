# Mai coding task evals

This suite checks whether Mai finishes repository tasks. Each case starts from
a fresh copy of its workspace. Mai receives the prompt, then hidden Go checks
run against the resulting code.

| Case | Request | Failure detected |
| --- | --- | --- |
| `empty-parser` | Fix an empty-input crash | Answering without fixing code |
| `complete-todo` | Add task completion | Breaking insertion or unknown IDs |
| `startup-timeout` | Change one of two similar values | Editing the wrong occurrence |
| `todo-app` | Implement an HTTP todo app | CRUD, filtering, persistence, ID reuse, concurrent updates |
| `twitter-clone` | Implement posts, follows, and timelines | Session boundaries, Unicode limits, ordering, pagination, restart |
| `twitter-thread` | Add likes and replies, then correct deletion behavior | Lost correction, duplicate likes, missing replies, retained deleted text |
| `todo-dashboard` | Implement todo UI and CSV completion summary | Counting events rather than latest task state, timestamp ties, empty/invalid data |

## Graded coding tasks

With `DEEPSEEK_API_KEY` populated and `jq` installed, run from the repository root:

```sh
./evals/run.sh
./evals/run.sh startup-timeout
MAI_EVAL_MODE=flash ./evals/run.sh
```

The runner builds Mai once, creates fresh Git projects and uses
`--jsonl --no-input --skip-skills`. It defaults to Pro/high, matching Mai's
default, with its Flash/high sidekick available. Skills are disabled so local
and global skill catalogs do not change the tasks. `MAI_EVAL_BIN` selects an
existing binary; `MAI_EVAL_MODE` selects `pro`, `flash`, or `max`.
`MAI_EVAL_PROVIDER` selects a configured provider (default: `deepseek`). Eval
workspaces are fresh directories, so put shared provider settings in
`$HOME/.mai.config`, or set `MAI_EVAL_CONFIG` to an absolute config path to copy
into each workspace. These runs make
paid API requests. The printed result directory contains events, stderr, final
workspace, diff and grader output, including failures.

Model request counts include completed and failed requests, including sidekick
worker requests. Tool counts and wall time accompany each grade. Nonzero tool
results are decoded from each JSONL tool output's top-level `ok` field and
include intentional regression failures. Sidekick worker events count too;
Python host calls are represented by the outer Python tool event. These cases
are a functional smoke suite, not a statistical performance benchmark. Review
the diffs too.
Do not treat `model.completed.total_tokens` as billable usage; sum available
input/output/cache fields and checkpoint usage when measuring API work.

## Router Flash conformance

For all five enabled Flash tools, combine the four-tool integration probe with the
context-edit and original-history recall probe. The first requires actual
file and image reads, two persistent Python cells, a Python-to-Bash bridge,
a direct patch, and verification of the resulting file. Both are paid probes:

```sh
MAI_LIVE_DEEPSEEK_FLASH_TOOLS=1 go test -v ./internal/mai -run '^TestLiveDeepSeekFlashTools$' -count=1 -timeout=6m
MAI_LIVE_DEEPSEEK_CONTEXT_EDIT=1 go test -v ./internal/mai -run '^TestLiveDeepSeekContextEdit/flash$' -count=1 -timeout=10m
```

To run these probes against a configured Flash provider, set
`MAI_LIVE_PROVIDER=enclave` and `MAI_LIVE_CONFIG=/absolute/path/.mai.config`
on each command. This uses the specified config rather than the built-in
DeepSeek endpoint. Flash has no sidekick tool. Passing these prompted smoke
checks establishes integration, not reliable autonomous tool selection across
arbitrary tasks; use the graded coding suite to check task completion too.

The opt-in provider probe runs the CLI with `--provider` and `--f`, executes one
Bash marker command, and checks the follow-up answer. It uses the tracked example
config in a temporary directory and makes paid API requests:

```sh
MAI_LIVE_PROVIDER=openrouter go test -v ./internal/mai -run '^TestLiveProviderFlash$' -count=1 -timeout=5m
MAI_LIVE_PROVIDER=enclave go test -v ./internal/mai -run '^TestLiveProviderFlash$' -count=1 -timeout=5m
```

Export `OPENROUTER_API_KEY` or `ENCLAVE_API_KEY` for the selected probe. Ordinary
`go test ./...` skips this test.

## Responses conformance

```sh
MAI_LIVE_DEEPSEEK_RESPONSES=1 go test -v ./internal/mai -run '^TestLiveDeepSeek(Responses|Sidekick)$' -count=1 -timeout=20m
```

This paid probe runs Flash and Pro at low, high and max effort. Each trial runs
one harmless Bash printf, saves and reloads between responses, and produces a
tool-free checkpoint retaining an exact audit code and UNKNOWN outcome.
Reasoning items are replayed when returned; simple requests can omit them.
This tests protocol conformance, not coding performance. Deterministic tests
cover incomplete streams, malformed tools, credential isolation, timeouts,
model/effort selection, original-history recall and image preservation.

The sidekick probe checks Pro/high and Pro/max directing a Flash/high worker
through two assignments, with Bash execution and recall from its separate
conversation.

## Coding and context continuity

```sh
MAI_LIVE_DEEPSEEK_CODING=1 MAI_CONTEXT_RESEARCH_REPORT=/absolute/existing/directory/deepseek-coding.json go test -v ./internal/mai -run '^TestLiveDeepSeekCoding$' -count=1 -timeout=20m
```

This paid Flash/high coding probe uses a three-stage Go task with unique
diagnostic records, buried identifiers, a corrected batch value, UNKNOWN
deployment outcome and hidden checks. Sessions reload between stages. Two
arms use identical facts: full history and two forced checkpoints under a
32K input budget. Reports retain duration, usage and original-history recall
without tool output bodies or credentials. One pair is continuity evidence,
not a performance ranking.

## Explicit context editing and recall

```sh
MAI_LIVE_DEEPSEEK_CONTEXT_EDIT=1 MAI_CONTEXT_EDIT_REPORT=/absolute/existing/directory/context-edit.json go test -v ./internal/mai -run '^TestLiveDeepSeekContextEdit$' -count=1 -timeout=20m
```

This paid probe runs Flash/high and Pro/high. Each trial reads a real temporary
Bash build log, then removes the source file and captures. The model must
inspect and shrink that output, retaining the current release and UNKNOWN
deployment outcome while omitting a random retired audit code. An HTTP
observer rejects unshortened source outputs after an edit and verifies that
the first resumed provider request excludes the retired code entirely.

After saving, reloading, and starting a fresh agent, the probe checks that the
entire projected history excludes the retired code. The model must retrieve it
through Python's `mai.history`, return the original source call ID in its
search result, and answer with the exact facts. The grader checks preservation
of original history, positive estimated token savings, tool order, requests
after resume, retrieval evidence, and the final answer. Reports contain grades,
model IDs, durations and counts without credentials or tool output bodies.
Each model has one trial; these are workflow checks, not a quality ranking.
Deterministic negative controls reject claims of editing without tool calls
and correct answers without original-history retrieval evidence.

## Patch and timing probes

```sh
./evals/patch-rate.sh
MAI_EVAL_MODE=flash ./evals/patch-rate.sh
./evals/timing.sh /absolute/path/events.jsonl
```

The patch probe defaults to Pro/high and checks twelve repeated-context edits.
Both coding runners require `jq` and exit nonzero when any case fails.
The patch probe reports grades and tool failures; the grader catches edits to
the wrong similar line. The timing helper requires `jq` and reports task,
model, tool and remaining duration in milliseconds. Cache usage is available
in the JSONL events, rather than in the timing table. Worker model and tool events count
individually; the enclosing sidekick duration is excluded to avoid counting
that work twice. Tool execution within a Python cell counts toward that outer
Python tool duration. Small samples and service/cache
variation do not establish general speed, price or quality advantages.

## Mini-app contracts and follow-ups

The mini-app fixtures contain a standard-library Go HTTP handler stub and a
runnable `cmd/server`. Hidden checks exercise real HTTP requests through
`httptest` and recreate handlers to verify persisted state. The coding runner
uses `go test -race ./...` for grading. Frontend checks verify the HTML response
and required summary text; they do not establish browser interaction,
accessibility, or visual fidelity.

A case with `followup.txt` runs its initial prompt with `--persist`, then runs
the correction with `--last` before grading. Both stages contribute events,
time, and exit status. `twitter-thread` uses this to change deletion to retained
tombstones while preserving likes and replies. The dashboard uses a supplied
HTML reference so layout requirements remain inspectable with skills disabled;
the separate image probe continues to check image-tool behavior.

Image results can include exact `solid_color` hex metadata for small, fully
opaque uniform images. The tool probe checks this bounded color contract;
passing it does not establish reliable interpretation of arbitrary screenshots.

The Flash Responses and coding-continuity probes also accept
`MAI_LIVE_PROVIDER` and `MAI_LIVE_CONFIG` for configured providers.
