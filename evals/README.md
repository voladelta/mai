# Mai coding task evals

This suite checks whether Mai finishes repository tasks. Each case starts from
a fresh copy of its workspace. Mai receives the prompt, then hidden Go checks
run against the resulting code.

| Case | Request | Failure detected |
| --- | --- | --- |
| `empty-parser` | Fix an empty-input crash | Answering without fixing code |
| `complete-todo` | Add task completion | Breaking insertion or unknown IDs |
| `startup-timeout` | Change one of two similar values | Editing the wrong occurrence |

## Graded coding tasks

With `DEEPSEEK_API_KEY` populated, run from the repository root:

```sh
./evals/run.sh
./evals/run.sh startup-timeout
MAI_EVAL_MODEL=ds-pro ./evals/run.sh
```

The runner builds Mai once, creates fresh Git projects and uses
`--jsonl --no-input`. It defaults to Flash/high. `MAI_EVAL_BIN` selects an
existing binary; `MAI_EVAL_MODEL` selects a supported model. These runs make
paid API requests. The printed result directory contains events, stderr, final
workspace, diff and grader output, including failures.

Model/tool request counts and wall time accompany each grade. Nonzero tool
results include intentional regression failures. Three cases are a functional
smoke suite, not a statistical performance benchmark. Review the diffs too.
Do not treat `model.completed.total_tokens` as billable usage; sum available
input/output/cache fields and checkpoint usage when measuring API work.

## Responses conformance

```sh
MAI_LIVE_DEEPSEEK_RESPONSES=1 go test -v ./internal/mai -run '^TestLiveDeepSeekResponses$' -count=1 -timeout=10m
```

This paid probe runs Flash and Pro at low, high and max effort. Each trial runs
one harmless Bash printf, saves and reloads between responses, and produces a
tool-free checkpoint retaining an exact audit code and UNKNOWN outcome.
Reasoning items are replayed when returned; simple requests can omit them.
This tests protocol conformance, not coding performance. Deterministic tests
cover incomplete streams, malformed tools, credential isolation, timeouts,
model/effort selection, original-history recall and image preservation.

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

## Patch and timing probes

```sh
./evals/patch-rate.sh
MAI_EVAL_MODEL=ds-flash ./evals/patch-rate.sh
./evals/timing.sh /absolute/path/events.jsonl
```

The patch probe defaults to Pro/high and checks twelve repeated-context edits.
It reports grades and tool failures; the grader catches edits to the wrong
similar line. The timing helper uses `jq`, separates model/tool/other duration
and reports available cache usage. Tool execution within a Python cell counts
toward that outer Python tool duration. Small samples and service/cache
variation do not establish general speed, price or quality advantages.
