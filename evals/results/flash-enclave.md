# Enclave Flash eval results

All seven implementation cases passed their hidden checks. All twelve
repeated-context patch targets passed; that final patch run had zero tool
errors. The five enabled Flash tools passed their integration probes.

## Configuration

- Provider: `enclave`, Responses profile from `.mai.config.example`.
- Upstream model: `cyberouter/deepseek-v4.1-flash`.
- Implementation cases: Flash/high, skills disabled, noninteractive.
- Repository base: `a3fca75`, with the harness/eval changes in this report's commit.
- Grades below are the latest successful trial for each case across iterations,
  rather than one uninterrupted run. A superseded sequential run was stopped
  after its completed cases were replaced by focused reruns.

## Implementation grades

| Case | Grade | Seconds | Model requests | Tool calls | Failed tool calls |
| --- | --- | ---: | ---: | ---: | ---: |
| complete-todo | PASS | 14 | 6 | 5 | 0 |
| empty-parser | PASS | 15 | 6 | 5 | 0 |
| startup-timeout | PASS | 11 | 5 | 5 | 0 |
| todo-app | PASS | 62 | 14 | 13 | 0 |
| todo-dashboard | PASS | 181 | 32 | 31 | 1 |
| twitter-clone | PASS | 133 | 19 | 18 | 1 |
| twitter-thread | PASS | 198 | 43 | 43 | 3 |

The thread case includes an initial persisted task and a resumed correction.
Hidden checks verify the tombstone, retained replies, idempotent likes, and
removal of the deleted post's text from disk. Coding graders run with the race
detector. All four new mini-app graders also rejected untouched handler stubs
for the intended HTTP behavior failures.

Five failed tool calls remained in the latest implementation trials. They were
recovered: a duplicate patch target, an absolute patch path, and three calls in
the thread case (duplicate target, rejected cleanup, stale test-file context).
Passing task grades do not mean every tool invocation was correct.

### Artifacts

Each directory retains JSONL events, stderr, changes, grader output, and the
implemented workspace. Paths are local temporary artifacts and can expire.

- Small cases: `/var/folders/kx/nf54r3cx6lg5tz23l72qs14h0000gn/T/mai-eval.iHoDeH/`.
- Todo app: `/var/folders/kx/nf54r3cx6lg5tz23l72qs14h0000gn/T/mai-eval.i90QNt/todo-app/`.
- Dashboard: `/var/folders/kx/nf54r3cx6lg5tz23l72qs14h0000gn/T/mai-eval.dqDoKZ/todo-dashboard/`.
- Twitter timeline: `/var/folders/kx/nf54r3cx6lg5tz23l72qs14h0000gn/T/mai-eval.AJu14e/twitter-clone/`.
- Twitter correction: `/var/folders/kx/nf54r3cx6lg5tz23l72qs14h0000gn/T/mai-eval.dwhRHP/twitter-thread/`.
- Twelve patch targets: `/var/folders/kx/nf54r3cx6lg5tz23l72qs14h0000gn/T/mai-patch-rate.W5sNjz/`.

## Other checks

| Check | Result |
| --- | --- |
| Flash tool integration: Bash, direct patch, image, two persistent Python cells, Python-to-Bash bridge, exact final answer | PASS |
| Context inspect/shrink, projected requests, resume, original-history retrieval | PASS |
| Three-stage coding with full history | PASS, hidden checks and original recall |
| Three-stage coding with two checkpoints | PASS, hidden checks and original recall |
| Flash Responses conformance at low, high, and max effort | PASS for all three |
| Enclave CLI Flash Bash marker and tool replay | PASS |
| `go test -race ./...` | PASS |
| `go vet ./...`, `git diff --check`, shell syntax checks | PASS |

Pro and its sidekick were outside this Flash run. Skills are enabled by default
in the CLI; these eval runs explicitly disable them with `--skip-skills`.

## Iteration

1. Added executable HTTP mini-app cases and a persisted follow-up runner.
2. Corrected the continuity probe to select its requested model rather than
   silently using the default Pro tier; added configured-provider support.
3. Added repository-local temporary-file and in-process HTTP-test instructions
   after repeated background-server and cleanup failures.
4. Added patch grammar, actual-newline guidance, and the one-operation-per-path
   rule to the tool schema after malformed and duplicate-target patches.
5. Made requested response formats explicit exceptions to the default final
   reporting style after correct tool effects failed the exact-answer check.
6. Added exact `solid_color` pixel metadata for small, fully opaque uniform
   images after repeated incorrect model color descriptions. Mixed images,
   transparency, and colors not exactly representable in six-digit hex omit it.

The final todo-app trial had zero failed tool calls, compared with eight before
the focused instructions. These are individual trials, not a statistical
speed, cost, or quality comparison.

## Limits

The image probe now verifies an exact uniform-color contract. Arbitrary visual
interpretation remains unreliable: earlier runs described a blue image as
black, green, purple, or white. A direct-image probe identified blue but also
estimated an incorrect RGB value. No general vision guarantee is established.

Mini-app graders verify HTTP behavior, persistence, and basic HTML responses.
They do not establish browser interactions, accessibility, or screenshot
fidelity. The dashboard uses a supplied HTML reference rather than a PNG.

## Reproduce

With the configured Enclave credential available in its environment variable:

```sh
MAI_EVAL_PROVIDER=enclave MAI_EVAL_MODE=flash MAI_EVAL_CONFIG="$PWD/.mai.config.example" ./evals/run.sh
MAI_EVAL_PROVIDER=enclave MAI_EVAL_MODE=flash MAI_EVAL_CONFIG="$PWD/.mai.config.example" ./evals/patch-rate.sh
MAI_LIVE_DEEPSEEK_FLASH_TOOLS=1 MAI_LIVE_PROVIDER=enclave MAI_LIVE_CONFIG="$PWD/.mai.config.example" go test -v ./internal/mai -run '^TestLiveDeepSeekFlashTools$' -count=1 -timeout=6m
MAI_LIVE_DEEPSEEK_CONTEXT_EDIT=1 MAI_LIVE_PROVIDER=enclave MAI_LIVE_CONFIG="$PWD/.mai.config.example" go test -v ./internal/mai -run '^TestLiveDeepSeekContextEdit/flash$' -count=1 -timeout=10m
```

These commands make paid provider requests.
