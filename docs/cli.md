# CLI reference

[Back to README](../README.md) · [Tools and skills](tools.md) · [Sessions and context](sessions.md)

## Usage

```sh
mai "prompt" [options]
```

Mai has one task command and no subcommands. Running `mai` without arguments
prints concise usage; `mai --help` lists all options.

Options may appear before or after the prompt. Options with values also accept
`--option=value`. Use `--` to end parsing when the prompt starts with `-`:

```sh
mai --f -- "--no-input: explain this flag"
mai "investigate this failure" --timeout=20m
```

## Options

| Option | Behavior | Example |
| --- | --- | --- |
| `-h`, `--help` | Show full help without starting a task. | `mai --help` |
| `--version` | Print the Mai version. | `mai --version` |
| `--persist` | Save a new task in the current project. | `mai "fix the parser" --persist` |
| `--last` | Resume the current saved task. | `mai "review the fix" --last` |
| `--f` | Select Flash/high without a sidekick. | `mai "explain this package" --f` |
| `--max` | Select Pro/max; sidekick remains Flash/high. | `mai "review this refactor" --max` |
| `-m`, `--model MODEL` | Select `ds-flash` or `ds-pro`. Explicit model selection uses high effort unless Pro/max is selected. | `mai "review this package" -m ds-pro` |
| `--max-turns COUNT` | Limit model turns; default 64, or `-1` for unlimited. | `mai "finish the migration" --max-turns -1` |
| `--timeout DURATION` | Model request first-byte/idle timeout; default `10m`. | `mai "investigate the failure" --timeout 20m` |
| `--cell-timeout DURATION` | Python cell wall-clock limit; default `10m`. | `mai "explore sales.csv" --cell-timeout 5m` |
| `--no-input` | Reject commands requiring approval instead of prompting. | `mai "review the diff" --no-input` |
| `-s`, `--skip-skills` | Skip skill discovery and explicit skill loading for this run. | `mai "explain this package" -s` |
| `--jsonl` | Write JSON Lines events to stdout; progress goes to stderr. | `mai "review the diff" --jsonl > run.jsonl` |

A task prompt is required except for help, version, or an empty invocation.
Task commands make paid API requests.

## Model selection

New tasks default to Pro/high with a Flash/high sidekick available.

```sh
mai "refactor this package"
mai "explain this package" --f
mai "review the implementation carefully" --max
mai "continue the saved task" --last -m ds-pro
```

`--f` and `--max` cannot be combined. `--f` conflicts with `-m ds-pro`;
`--max` requires Pro and conflicts with `-m ds-flash`. `-m ds-pro --max`
is valid. `-e` and `--effort` are unsupported.

## Saving and resuming

```sh
mai "add useful tests for the parser" --persist
mai "fix the failures" --last
mai "review the result carefully" --last --max
```

`--persist` and `--last` cannot be combined. Resume restores the original
working directory, model, effort, and conversation, unless a model mode is
explicitly selected. Python starts a new environment; sidekick IDs expire
when the previous run ends.

Turn limits, request and cell timeouts, skip-skills, input, and output options
apply only to this run. Pass them again when resuming.

## Turn limits and timeouts

Each run allows up to 64 model turns by default. Use `--max-turns` with a
positive integer to set a different limit, or `-1` to run without a turn cap
until Mai finishes, encounters an error, or is interrupted:

```bash
mai "complete the migration" --max-turns 128 --persist
mai "continue the migration" --last --max-turns 128
mai "finish the migration" --max-turns -1 --persist
```

A turn is one model request and execution of its returned tool calls. A final
answer also consumes a turn. The limit applies to the current run and is not
saved; `--last` defaults to 64 unless you pass `--max-turns` again. When the
limit is reached, Mai stops with an error after saving completed work for a
persisted task, which you can continue with `--last`.

Each DeepSeek Responses request has a 10-minute time-to-first-byte and idle timeout. Each
received stream chunk restarts the idle timer, so an active response can run
longer than 10 minutes. Set a different positive Go-style duration when necessary:

```bash
mai "investigate the failure" --timeout 20m
```

Failed requests return an error. Mai does not automatically replay a failed
request or tool call.

Each Python cell has a separate 10-minute wall-clock limit. Change it with
`--cell-timeout`; `--timeout` controls model requests.

## Non-interactive runs

Use `--no-input` in scripts and other non-interactive environments. If a
command requires approval, Mai rejects it instead of opening a terminal prompt.
This option does not sandbox Bash or Python.

```sh
mai "review this package" --jsonl --no-input > run.jsonl
```

See [JSONL events](events.md) for event fields, token usage, and elapsed-time
accounting, and [Tools and skills](tools.md) for execution boundaries.
