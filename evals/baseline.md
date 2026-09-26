# Local smoke baseline

Runs on 2026-09-26 with Mai at medium effort. Each case started in a fresh Git
repository. The runner used hidden Go checks after Mai finished.

| Model | Case | Grade | Wall time | Model requests | Tool calls | Nonzero tool results |
| --- | --- | --- | ---: | ---: | ---: | ---: |
| Luna | `empty-parser` | Pass | 34 s | 6 | 5 | 0 |
| Luna | `complete-todo` | Pass | 36 s | 10 | 9 | 2 |
| Luna | `startup-timeout` | Pass | 35 s | 6 | 5 | 0 |
| Sol | `empty-parser` | Pass | 33 s | 8 | 7 | 1 |
| Sol | `complete-todo` | Pass | 40 s | 7 | 6 | 0 |
| Sol | `startup-timeout` | Pass | 25 s | 6 | 5 | 0 |

In Luna's todo run, Mai first produced a test with an unused variable, then
attempted an ambiguous patch. It corrected both and passed the hidden check.
The ambiguity error came from the new conservative patch check; it prevented
Mai from silently choosing the first matching line. In Sol's parser run, the
nonzero result was an intentional regression test failure before the fix.

Each model ran each case once, so both 3/3 results are functional smoke results,
not estimates of general pass probability. Wall time includes model and tool
waits on this machine; one run is too little evidence to rank the models by
speed. Nonzero tool results include expected failing tests. The runner does not
measure billable tokens or monetary cost.

Reproduce with `./evals/run.sh`. The command prints a fresh result directory
containing each task's JSONL events, stderr, diff, grader output, and workspace.

## Repeated-context patch sample

On the same date, each model also received twelve separate requests to change
one named function among twelve that all returned `30`. A hidden check called
every function after each run. The observed wrong-edit rates were:

| Model | Correct edits | Wrong edits | Other failures | Nonzero tool results |
| --- | ---: | ---: | ---: | ---: |
| Sol, medium | 12/12 | 0/12 | 0/12 | 0 |
| Luna, medium | 12/12 | 0/12 | 0/12 | 0 |

All 24 saved diffs show one intended `return 30` to `return 45` change. As a
negative check, grading one Sol result against a different target reported both
the unintended change and the unchanged target.

The twelve targets share one small Go file and one edit pattern. These results
show that Mai handled this repeated-context pattern in these runs; they do not
establish its mistake rate for varied repositories, languages, or concurrent
edits. Reproduce a model's sample with `MAI_EVAL_MODEL=sol ./evals/patch-rate.sh`
or `MAI_EVAL_MODEL=luna ./evals/patch-rate.sh`.

## Where task time went

Fresh three-case runs on 2026-09-26 used JSONL elapsed times. All six cases
passed. Times below cover the Mai agent run, excluding runner setup and hidden
grading.

| Model | Case | Task | Codex requests | Tools |
| --- | --- | ---: | ---: | ---: |
| Sol | `empty-parser` | 30.61 s | 29.29 s | 1.31 s |
| Sol | `complete-todo` | 39.17 s | 38.17 s | 0.98 s |
| Sol | `startup-timeout` | 24.08 s | 23.32 s | 0.75 s |
| Luna | `empty-parser` | 20.75 s | 20.05 s | 0.69 s |
| Luna | `complete-todo` | 36.52 s | 35.68 s | 0.82 s |
| Luna | `startup-timeout` | 24.03 s | 23.29 s | 0.74 s |

Across these three runs, Codex requests occupied 90.78 of 93.86 seconds for
Sol (96.7%) and 79.02 of 81.31 seconds for Luna (97.2%). Tools occupied 3.04
seconds and 2.25 seconds respectively. The small remainder covers agent work
outside either bucket. Request time includes network transfer, generation,
streaming, and any request retry; it does not isolate model computation.

Each completed model response in these traces led to at most one tool call.
Parallelizing execution of the existing calls therefore offers no overlap in
this sample. Even eliminating all tool execution could save at most about 3%
of these agent runs. A harness that batches independent calls might also remove
model round trips, but these timings do not establish which calls are
independent or how many requests that would save. Larger builds, slow network
tools, and concurrent editing may have a different profile.

Use `./evals/timing.sh` on a run's `events.jsonl` to reproduce the buckets.
