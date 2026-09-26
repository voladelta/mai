# Test batched tool calls in Mai

Test whether Mai finishes coding tasks faster when a model can request several
tool calls in one response. Keep tool execution sequential during the first
comparison. Consider concurrent execution only after the results show which
calls are independent and where time is spent.

## What we know

In the six small tasks recorded on 26 September 2026, Codex requests took
96.7% of Sol's agent time and 97.2% of Luna's. Tool execution took about 3%.
Each model response led to at most one tool call. The [timing baseline](baseline.md#where-task-time-went)
records the measurements and their limits.

These tasks show little opportunity to save time by executing existing tool
calls concurrently. They do not show whether requesting several calls in one
response would reduce the number of model requests. They also do not represent
slow builds, network tools or large repositories.

## Compare one change at a time

Use the current Mai behaviour as the control. It sends
`parallel_tool_calls: false` and runs returned calls in order.

For the test, allow the model to return several calls in one response. Keep
Mai's dispatcher sequential. This changes how many model requests a task may
need without adding concurrent file or process operations.

Do not treat a tool name as proof that calls are independent. A `bash` command
can change files. `apply_patch`, `python` and subagents can also have effects.
Review the returned calls and their arguments before testing concurrent
execution.

## Choose tasks that can reveal a difference

Build a fixed set of distinct tasks from real Mai use. Include:

- tasks that need several independent file or documentation reads
- tasks that require a result before the next command can start
- tasks with slow builds, tests or network calls
- tasks where the intended edit is easy to confuse with another location

Use different files and repositories where available. Do not count changes to
different names in the same fixture as distinct kinds of work. Keep each
task's starting commit, prompt and hidden success check outside Mai's
workspace.

Start with a small pilot to confirm that the model returns multiple calls when
allowed. Use at least 30 distinct tasks for a decision about changing Mai's
default. Repeat each task in each setup to see how much results vary between
runs. Test Sol and Luna separately at the same reasoning effort.

## Run the comparison

1. Record the task set, starting commits, prompts, model IDs, reasoning
   effort, timeout and success checks before running either setup.
2. Run each task from a fresh checkout with the current setting and save its
   JSONL event log, final diff and check result.
3. Run the same task with batched calls allowed. Alternate which setup runs
   first across repeated trials.
4. Compare task success, unintended edits, model requests, total agent time,
   Codex request time, tool time and nonzero tool results.
5. Inspect every response with multiple calls. Record whether its calls were
   independent, whether Mai ran them in order and whether any call had an
   effect outside its intended scope.

Use the [timing helper](timing.sh) to sum elapsed times in each JSONL trace.
The `total_tokens` event field reports context size, not billable use. Do not
use it to calculate cost per successful task.

## Decide what to change

Keep the current behaviour if the model rarely returns multiple calls, task
success falls, unintended edits rise or elapsed time does not improve beyond
normal variation between runs.

If batching reduces model requests and task time without harming results,
consider making it the default. Report results by task type and model, not
only as one average. Review diffs as well as hidden checks so a passing task
does not conceal unnecessary or hard-to-review changes.

Only then test concurrent execution as a separate change. Start with calls
whose read-only behaviour is known. Keep file writes and commands with unknown
effects in order. Use another matched comparison to measure any additional
time saved and any change in failures.

Even a set of 30 tasks cannot rule out rare mistakes. Keep reviewing real Mai
work after any change to the default.
