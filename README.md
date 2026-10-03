# mai

![mai project banner](assets/mai-banner.png)

**Give a repository task to a small Go agent, then pick it up later with
searchable history and portable checkpoints.**

Mai runs on macOS and Linux and uses the DeepSeek Responses API. It can read
code, edit files, run checks, and return a final answer from your terminal.

[![Go 1.27+](https://img.shields.io/badge/Go-1.27%2B-00ADD8)](go.mod)
[![Version 0.1.0](https://img.shields.io/badge/version-0.1.0-blue)](internal/mai/app.go)

```sh
git clone https://github.com/voladelta/mai.git
cd mai
go install ./cmd/mai
```

Requires Go 1.27+ to build and `DEEPSEEK_API_KEY` to run tasks.
See [Installation](#installation) for binary location and `PATH` setup.

[Quick start](#quick-start) · [CLI](docs/cli.md) · [Tools](docs/tools.md) ·
[Sessions](docs/sessions.md) · [JSONL events](docs/events.md) · [Evals](evals/README.md)

## Why Mai?

Repository work often spans several tool calls and follow-up requests. Mai
keeps that work in a terminal workflow: start a task, let the agent inspect and
change the code, then save the conversation when you need to return to it.

| Capability | What it gives you | Try it |
| --- | --- | --- |
| Repository work | Bash, structured patches, and local image inspection. | `mai "fix the empty-input crash and run tests"` |
| Saved tasks | Resume the original directory, model, effort, and conversation. | `mai "continue the fix" --last` |
| Model selection | Pro/high by default, Pro/max or Flash/high when selected. | `mai "review this refactor" --max` |
| Pro sidekick | One Flash/high worker with its own conversation for bounded assignments. | [Sidekick behavior](docs/tools.md#pro-sidekick) |
| Searchable history | Recall original visible text after context editing and compaction. | [Python history search](docs/tools.md#persistent-python) |
| Repository skills | Load project instructions before global skills, with explicit skill selection. | `mai 'Use $my-skill to review this package'` |
| Script output | Stream task, model, and tool events as JSON Lines. | `mai "review this package" --jsonl --no-input > run.jsonl` |

The Go runtime uses only the standard library. Python is optional and starts
only when the model calls its tool.

## Quick start

After installing Mai:

1. Set your API key in the shell that will launch it:

   ```sh
   export DEEPSEEK_API_KEY='your-api-key'
   ```

2. Change into the repository you want Mai to work on:

   ```sh
   cd /path/to/your/repository
   ```

3. Start a task, inspect the changes, and continue the saved conversation:

   ```sh
   mai "fix the parser's empty-input crash and run the relevant tests" --persist
   git diff
   mai "review the fix for edge cases" --last
   git diff --check
   mai --help
   ```

Replace the key and repository path with your own values. Task commands make
paid DeepSeek API requests.

Tasks are stateless by default. Use `--persist` to save a new task and `--last`
to resume the current saved task in the same project. Only one process can use
a particular saved task at a time.

## Installation

### Requirements

- **macOS or Linux**, with `/bin/bash` for the Bash tool.
- **Go 1.27 or later** to build from source.
- **A DeepSeek API key** in `DEEPSEEK_API_KEY` for task execution.
- **Git** for repository-root discovery. Without it, the working directory is
  the repository boundary.
- **Python 3.9 or later**, optional, for persistent Python cells.
- **jq**, optional, for the coding eval runners and timing helper.

No Node runtime, SDK, or third-party Go dependency is required.

### Install from a checkout

```sh
git clone https://github.com/voladelta/mai.git
cd mai
go install ./cmd/mai
```

Go installs into `GOBIN` when set, otherwise `$(go env GOPATH)/bin`. Add that
directory to your shell's `PATH`. For the default Go binary directory:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
mai --version
```

### Build a local binary

From the cloned repository:

```sh
go build -o mai ./cmd/mai
./mai --help
```

Move the binary into a directory on `PATH` to use it from other projects.

### Run from source

From the cloned repository, show help without installing a binary:

```sh
go run ./cmd/mai --help
```

When running a task this way, the clone is the working repository. Use an
installed or built binary to work in another project.

## Models and commands

```sh
mai "add useful tests for the parser"              # Pro/high
mai "review the implementation carefully" --max   # Pro/max
mai "explain this package" --f                    # Flash/high
mai "continue the saved task" --last --max        # Resume with Pro/max
```

| Selection | API model | Reasoning effort | Sidekick |
| --- | --- | --- | --- |
| Default or `-m ds-pro` | `deepseek-v4-pro` | High | Flash/high |
| `--max` | `deepseek-v4-pro` | Max | Flash/high |
| `--f` or `-m ds-flash` | `deepseek-flash` | High | None |

A saved task keeps its model and effort unless you select another mode.
Conflicting selections are rejected. There is no separate `--effort` flag.

The default run limit is 64 model turns. Increase it or use `-1` for unlimited
turns until completion, error, or interruption:

```sh
mai "complete the migration" --max-turns 128 --persist
mai "continue the migration" --last --max-turns -1
mai "review this package" --jsonl --no-input > run.jsonl
```

See the [CLI reference](docs/cli.md) for every option, examples, and timeouts.

## Configuration

Mai reads environment variables and CLI options; it has no settings file.

| Variable | Purpose | Default |
| --- | --- | --- |
| `DEEPSEEK_API_KEY` | Required credential for task requests. | Unset |
| `MAI_DEEPSEEK_URL` | Complete Responses endpoint; HTTPS required except for loopback. | `https://api.deepseek.com/responses` |
| `MAI_CONTEXT_WINDOW` | Input context budget, from 32,768 to 1,000,000 tokens. | `1000000` |
| `MAI_PYTHON` | Executable for the optional persistent Python tool. | `python3` |

For example, lower the context budget and give each Python cell five minutes:

```sh
MAI_CONTEXT_WINDOW=65536 mai "investigate the failure" --cell-timeout 5m
```

The API credential is read from the environment and is not copied into saved
task state or Mai's own logs. Shell commands run by the agent inherit its OS
access and can access that environment.

## Design and architecture

Mai follows four principles:

- **Keep the runtime small.** A Go binary owns the model loop, tools, and local
  state; optional Python supports exploration.
- **Save only when asked.** A normal run leaves no saved conversation.
  `--persist` and `--last` use project-local state.
- **Keep original evidence accessible.** Context editing shortens successful
  Bash stdout for requests; history search still reads the original text.
- **Make recovery explicit.** Failed requests and interrupted tool calls are
  never replayed automatically.

```text
Prompt + CLI options
         |
         v
Go agent loop <----> DeepSeek Responses API (streaming)
         |
         +--> Bash / patches / skills / images / context editing
         +--> Optional persistent Python --> Go tool bridge
         +--> Pro sidekick --> Flash/high agent (synchronous)
         |
         +--> Terminal text or JSONL events
         +--> With --persist / --last: .mai/ session + history archive
```

Mai sends local conversation history, including plain reasoning, with model
requests. At 80% of the context budget it builds a readable checkpoint. The
original visible history remains searchable; checkpoints are model-authored
summaries, so useful details may still need retrieval.

| Approach | Repository workflow | Continuity | Execution |
| --- | --- | --- | --- |
| Mai | Model chooses tools, edits files, and runs checks. | Optional saved tasks, checkpoints, and history search. | Go harness runs tools locally. |
| Direct API script | You implement the tool loop and integrations. | You implement storage and context management. | Your script dispatches operations. |
| Manual terminal work | You inspect, edit, and run every command. | You keep notes and shell history. | You run each operation yourself. |

## Limits and safety

Bash and Python are **not sandboxed**. The patch tool enforces repository
boundaries, and Mai asks for approval for recognizable `rm` commands with
external or unresolved targets. That check covers `rm` only; other commands
can overwrite or delete data. `--no-input` rejects approval requests.

- Pro cannot accept image-bearing history. Its image tools ask Flash for a
  labeled description; Flash can receive inline images directly.
- Portable compaction currently handles text histories. Older image-bearing
  records may require a new task or text-only history.
- Python variables and sidekick conversations last for one run.
  `--last` restores conversation history, not those runtime environments.
- Sidekick execution is synchronous, with one worker and 32 total model turns.
- There is no mid-turn steering or automatic retry of failed work.
- Saved tasks must use the supported state format; incompatible older files
  require a new task.
- Smaller contexts and checkpoints can add model requests and change cache
  reuse. The included evals do not establish general speed or cost advantages.

See [Tools and skills](docs/tools.md#safety) for execution boundaries and
[Sessions and context](docs/sessions.md) for storage and recovery details.

## Troubleshooting

| Symptom | Action |
| --- | --- |
| `mai: command not found` | Add `GOBIN`, or the default Go binary directory, to `PATH`; use `./mai` for a local build. |
| `DeepSeek requires DEEPSEEK_API_KEY` | Export the key in the shell launching Mai. |
| `no saved task in this project` | Start with `--persist`, then resume from the same project. |
| `session ... is already running` | Wait for the process using that task to exit, or start a separate task. |
| `agent stopped after 64 model turns` | Resume a persisted task with a larger `--max-turns` value or `-1`. |
| `Python is unavailable` | Install Python 3.9+ on `PATH`, or set `MAI_PYTHON`. |
| `DeepSeek Pro does not support images` | Use `--last --f` for image-bearing history, or start a new Pro task. |
| `DeepSeek Responses request failed (network or timeout)` | Check connectivity and the endpoint; raise `--timeout` if needed. |

An interrupted tool call has an unknown outcome on resume. Inspect its file
and command effects before repeating work that may already have happened.

## FAQ

### Does Mai save every conversation?

No. Use `--persist` for a new saved task; ordinary runs are stateless.

### Where are saved tasks stored?

Under `.mai/` at the Git repository root, or in the working directory outside
Git. Keep the session JSON and transcript archive together when backing up a task.

### Can I switch models when resuming?

Yes: use `--last --f`, `--last --max`, or `--last -m ds-pro`.
Pro rejects image-bearing history.

### Do I need Python?

Only for the persistent Python tool. Bash, patches, skills, and image tools
work through Go. Mai does not install Python packages.

### Do Python variables survive compaction or restart?

They survive compaction within a run. Exiting Mai, including before `--last`,
ends the Python environment.

### Does the sidekick run in parallel?

No. Pro waits for its Flash/high worker, then integrates the result. The worker
has a separate conversation and Python namespace.

### Can I use Mai in scripts?

Yes: use `--jsonl --no-input`. Progress goes to stderr, events to stdout.
See [JSONL events](docs/events.md) for usage fields and timing rules.

## About contributions

For repository changes, describe the problem, resulting behavior, and checks
you ran. From the repository root, the local checks are:

```sh
go test -race ./...
go vet ./...
```

The [eval guide](evals/README.md) covers graded coding tasks, paid Responses and
sidekick probes, and context-continuity checks. Live probes require explicit
environment switches and make paid API requests.

## License

This repository currently contains no license file.
