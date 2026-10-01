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

## Native context-editing research

Run the controlled three-arm study with your Codex login:

```sh
MAI_LIVE_CONTEXT_RESEARCH=1 \
MAI_CONTEXT_RESEARCH_REPORT=/absolute/existing/directory/research.json \
go test -v ./internal/mai -run '^TestLiveContextResearch$' -count=1 -timeout=15m
```

It compares native Codex compaction, deterministic 1,000-byte head/tail sampling,
and model-proposed `edit_context` summaries. Two matched repetitions place facts
at the start, middle, and end of synthetic Bash logs. Each session goes through
two cycles, a user correction, and save/load. There are 36 graded answers, five
exact string fields per answer. Assessment answers are not appended to history.
Reports retain the strict count and also a count that treats `unknown` and
`UNKNOWN` as the same status; identifiers still require exact matches.
Every compaction, editing, and answer request records available input/output/cache
usage and elapsed time. Original-log recall is checked through the host's history
search after resume, including the prior cycle. No model-proposed command runs.

The first objective emphasizes release and audit codes; batch size is queried
later. Set `MAI_CONTEXT_RESEARCH_DECLARED_GOAL=1` for a focused follow-up with all
five graded fields declared up front. That follow-up uses middle placement, two
repetitions, and two cycles: 12 graded answers. Use a separate report filename.

These are context probes, not long coding-task benchmarks. Compaction is forced
at cycle boundaries below the normal 90% production threshold. Answer-phase
retrieval is prohibited, so lost projected facts can score poorly even though
the archive can recover them. CLM preparation is explicitly requested and gets
at most four requests per cycle. Arm order is fixed, and the sample is too small
for statistical performance claims. Missing usage fields are unavailable, not
zero. Inspection/summary overhead and cache effects must be counted before
claiming a gain.

### Coding and context-budget pilot

```sh
MAI_LIVE_CONTEXT_BUDGET_RESEARCH=1 \
MAI_CONTEXT_RESEARCH_REPORT=/absolute/existing/directory/budget-research.json \
go test -v ./internal/mai -run '^TestLiveContextBudgetResearch$' -count=1 -timeout=45m
```

This pilot executes real coding tools in temporary Go projects. It compares
native compaction at the production 90% threshold, selective CLM with inline
editing handles, and bounded 1,000-byte head/tail excerpts with retrieval.
The bounded policy is experimental harness behavior, not a production default.
All arms retain original history and can use `mai.history`. The harness removes
the editing tool and its hints from native/bounded request schemas so baseline
behavior does not depend on the model obeying a prohibition in the prompt.

Two matched repetitions randomize arm order with a recorded seed. Long sessions
warm an old-log prefix, calibrate new logs from reported input usage, then
perform four coding stages, including a batch correction and retrieval of an old
audit identifier. Short sessions have a small log and one coding stage, where
editing should stay inactive. Hidden Go checks verify constants, corrected limits,
parsing and safe retry behavior. Sessions save and reload between stages.
Checkpoint files live outside the coding project so repository searches cannot
read internal state instead of using the history interface.
Seeded logs are split into successful outputs no larger than Bash's production
64 KiB stdout cap; the bounded arm further reduces each new output to head/tail
excerpts. The study does not bypass MAI's existing output admission limit.
Warm-up uses `tool_choice: none` while keeping each arm's tool schema intact;
the model cannot accidentally start coding before the measured task.

Reports are saved after each arm, including partial failures, request/cache usage,
compaction events, edit receipts, wall time and source recall after resume. Tool
output bodies are omitted. Count warm requests and all management requests when
comparing strategies. These are synthetic staged tasks with two repetitions;
they cannot establish a general performance gain. Check whether native compaction
actually occurred before interpreting any claim about avoiding it.

## Portable compaction

Run a fresh matched native-versus-portable coding pilot (two long trials each):

```sh
MAI_LIVE_CONTEXT_BUDGET_RESEARCH=1 \
MAI_CONTEXT_RESEARCH_PORTABLE=1 \
MAI_CONTEXT_RESEARCH_REPORT=/absolute/path/portable-native.json \
go test -v ./internal/mai -run '^TestLiveContextBudgetResearch$' -count=1 -timeout=30m
```

This retains the staged coding task, warm cache policy, production compaction
threshold and hidden checks. Both arms hide `edit_context`; the portable arm
uses ordinary tool-free checkpoint generation. Count its summary requests as
management work. Repeated log lines make source encoding unusually effective;
do not extrapolate the result to arbitrary unique histories.

To check another provider with real coding tools and forced portable checkpoints:

```sh
MAI_LIVE_PORTABLE_CHAT=1 \
MAI_CHAT_URL=https://api.deepseek.com/chat/completions \
MAI_CHAT_KEY_ENV=DEEPSEEK_API_KEY \
MAI_CHAT_TEST_MODELS=deepseek-flash,deepseek-v4-pro \
MAI_CHAT_TEST_FULL_CONTROL=1 \
MAI_CONTEXT_RESEARCH_REPORT=/absolute/path/portable-chat.json \
go test -v ./internal/mai -run '^TestLivePortableChatProviders$' -count=1 -timeout=20m
```

The key variable must already be populated. This test uses the production chat
configuration, unique diagnostic records, chunk folding, literal identifiers,
an explicit UNKNOWN enum, a corrected numeric constraint, hidden Go checks and
session reloads. It requires no Codex login. With the control enabled, each model
also runs identical paired facts with the full original history and a larger
test budget that disables compaction. Checkpoints are forced twice under
a conservative 32K input budget; this is cross-model conformance evidence, not
a latency comparison with the warmed native pilot. Tool output bodies and keys
are excluded from the report.

## Cross-model portable comparison

Use the same three-stage coding fixture for DeepSeek Flash, GPT-6.1 Sol medium
and GPT-6 Luna medium, with two matched repetitions and both full-history and
portable arms. Each repetition uses identical facts for all three models;
model and arm order rotate. GPT uses the cached Codex subscription login.

```sh
MAI_LIVE_PORTABLE_MODEL_COMPARISON=1 \
MAI_CHAT_URL=https://api.deepseek.com/chat/completions \
MAI_CHAT_KEY_ENV=DEEPSEEK_API_KEY \
MAI_CONTEXT_RESEARCH_REPORT=/absolute/path/model-comparison.json \
go test -v ./internal/mai -run '^TestLivePortableModelComparison$' -count=1 -timeout=45m
```

DeepSeek thinking remains disabled; it is not the same reasoning configuration
as GPT medium. Each worker generates its own checkpoints, so the comparison
combines coding and summarization behavior. Both portable checkpoints are
forced; full history fits. Two repetitions support a descriptive comparison,
not a general model ranking or a native-compaction performance claim.

## DeepSeek Responses conformance

With `DEEPSEEK_API_KEY` already populated:

```sh
MAI_LIVE_DEEPSEEK_RESPONSES=1 \
go test -v ./internal/mai -run '^TestLiveDeepSeekResponses$' -count=1 -timeout=10m
```

This paid API probe runs Flash and Pro at low, high and max effort. Each trial
executes a harmless Bash printf, saves and reloads the task between responses,
checks plain reasoning replay, and generates a tool-free checkpoint retaining
an exact audit code and UNKNOWN outcome. It uses `/responses`, never Chat
Completions or the Codex backend. This establishes protocol conformance, not a
coding-performance ranking. Deterministic tests cover malformed/incomplete
responses, credential isolation, model/effort selection and backend identity.

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
