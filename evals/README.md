# Mai coding task evals

This small suite checks whether Mai finishes realistic repository tasks. Each case
starts from a fresh copy of `workspace/`. Its `prompt.txt` is sent to Mai, then
`check_test.go.txt` is copied in as `check_test.go` and run with
`go test ./...`. The check stays outside the workspace while Mai works.

Cases:

| Case | User request | Main failure it detects |
| --- | --- | --- |
| `empty-parser` | Fix an empty-input crash | Returning a plausible answer without fixing the code |
| `complete-todo` | Add task completion | Breaking existing task insertion or failing unknown IDs |
| `startup-timeout` | Change one of two similar values | Editing the wrong matching line |

Run the suite from the repository root:

```sh
./evals/run.sh
./evals/run.sh startup-timeout
```

The runner builds Mai once, initializes a fresh Git repository for each case,
and runs each selected case with `--jsonl --no-input`. It prints the result
directory and keeps Mai's output, the final workspace, a diff, and the grader
result for each case. It reports model requests, tool calls, nonzero tool results,
and wall time so a passed task is not the only signal. These three cases are a
smoke test, not a statistical benchmark.
With no case names, the runner discovers every directory under `evals/cases/`.
Nonzero tool results include intentionally failing tests during a fix, so inspect
the events before treating one as harness friction.
`model.completed.total_tokens` is context size, so the runner does not mistake it
for billable tokens or calculate cost per pass.

The runner uses the default Luna model at medium effort. Set `MAI_EVAL_BIN` to
the absolute path of an existing Mai binary or `MAI_EVAL_MODEL` to `sol` to
compare a different setup.
The cases use temporary directories and make live Codex requests. Their result
directory is printed even if a case fails, so failures remain inspectable.

The hidden checks establish functional behavior. Review the diff and events to
assess whether the implementation is readable, scoped, and reproducible. Do not
turn that review into a single score without a written rubric.

The first local smoke result is recorded in [baseline.md](baseline.md).

## Searchable history recall

Run the opt-in live eval with your normal Codex login:

```sh
MAI_LIVE_HISTORY_EVAL=1 go test -v ./internal/mai -run '^TestLiveHistoryRecall$' -count=1
```

It runs three paired attempts with Luna at medium effort. Each pair asks for a
new random code. The test arm places the code in Mai's in-memory archived
transcript; the control has no archived entry. Neither arm puts the code in the
model's current context or in a workspace file. The test reports exact recall
and how often Mai called `mai.history`. It makes live model requests and is
skipped by normal `go test` runs.

On 2026-09-27, the first run was 0/3 in both arms: Mai searched a long phrase
that did not occur literally in the old message and stopped after zero hits.
After the system hint explained literal substring search and short queries, the
same eval design scored 3/3 with archived history and 0/3 without it. This is a
small recall probe, not a statistical benchmark. Compaction and saved-task
recovery are covered by separate deterministic tests; this live probe seeds the
archive directly so it tests model use of history without exposing a session
file to Bash.

## Waiting time

New JSONL traces include `duration_ms` on model, tool, and completed task
events. Summarize one or more traces with `./evals/timing.sh EVENTS.jsonl`.
This helper uses `jq`. `model_ms` covers Codex requests, `tool_ms` covers
executing model-issued tool calls, and `other_ms` is the remainder of the agent
run, including setup, history writes, and compaction. A Python cell's host
operations are included in that outer `python` tool duration.

These buckets explain elapsed time in a completed run. They do not by
themselves prove that any two calls are independent or safe to run in parallel.

The [batched tool call experiment plan](batching-experiment.md) explains how to
test fewer model requests before considering concurrent tool execution.

## Repeated-context edit sample

Run `MAI_EVAL_MODEL=sol ./evals/patch-rate.sh` to ask Mai for twelve separate
edits in fresh copies of the same settings repository. Each request names one
function among twelve that initially return the same value. A hidden test calls
all twelve functions to detect a change to the wrong one. The runner reports
`wrong-edit` separately from other failures and saves the full trace for each
request. These targets share one code shape, so their rate describes this sample
only; it is not a general patch error rate.
