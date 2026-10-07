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
mai --no-input -- "--no-input: explain this flag"
mai "investigate this failure" --timeout=20m
```

## Options

| Option | Behavior | Example |
| --- | --- | --- |
| `-h`, `--help` | Show full help without starting a task. | `mai --help` |
| `--version` | Print the Mai version. | `mai --version` |
| `--persist` | Save a new task in the current project. | `mai "fix the parser" --persist` |
| `--last` | Resume the current saved task. | `mai "review the fix" --last` |
| `--fork` | Start a new saved session from the current saved task. | `mai "audit the logs" --fork` |
| `--fork-from ID` | Start a new saved session from the given saved session. | `mai "audit the logs" --fork-from 01234567-89ab-cdef-0123-456789abcdef` |
| `-m`, `--model NAME` | Override the provider's configured model for this run. | `mai "quick review" --model cyberouter/glm-5.3-flash` |
| `--effort VALUE` | Select reasoning effort: `l`, `h`, or `max` (default `h`). | `mai "review this refactor" --effort max` |
| `--provider NAME` | Override `default_provider` from the selected config; built-in default is `enclave`. | `mai "quick review" --provider enclave` |
| `--max-turns COUNT` | Limit model turns; default 64, or `-1` for unlimited. | `mai "finish the migration" --max-turns -1` |
| `--timeout DURATION` | Model request first-byte/idle timeout; default `10m`. | `mai "investigate the failure" --timeout 20m` |
| `--cell-timeout DURATION` | Python cell wall-clock limit; default `10m`. | `mai "explore sales.csv" --cell-timeout 5m` |
| `--no-input` | Reject commands requiring approval instead of prompting. | `mai "review the diff" --no-input` |
| `-s`, `--skip-skills` | Disable skill discovery and loading for this run. | `mai "explain this package" -s` |
| `--jsonl` | Write JSON Lines events to stdout; progress goes to stderr. | `mai "review the diff" --jsonl > run.jsonl` |

A task prompt is required except for help, version, or an empty invocation.
Task commands make paid API requests.

## Model selection

Each provider config sets one `model`; new tasks use it at high effort.

```sh
mai "refactor this package"
mai "explain this package" --model cyberouter/glm-5.3-flash
mai "review the implementation carefully" --effort max
mai "continue the saved task" --last
mai "quick review" --provider openrouter --model deepseek/deepseek-v4-pro
```

`--model` (or `-m`) overrides the provider's configured model for the current
run and is sent upstream exactly as written. `--effort` accepts `l`, `h`, or
`max` (default `h`).

## Providers and configuration

Mai reads `.mai.config` in the current working directory first, otherwise
`$HOME/.mai.config`. It uses one JSON file without merging. An invalid selected
file fails with its path; it never silently falls back. With neither file,
only the built-in Enclave provider is available.

See [the example config](../.mai.config.example) for DeepSeek, OpenRouter, and
Enclave. It has `default_provider` and a `providers` object. Each provider needs
`base_url`, `api_key_env`, and a single `model` ID. Credentials
come from the named environment variable. URLs must be HTTPS, with HTTP allowed
for loopback tests, and contain no embedded credentials, query or fragment.
Mai appends `/responses` to the base URL.

Set `profile` to `deepseek`, `openrouter`, or `responses` to select API behavior
independently of the provider name. DeepSeek replays only plain reasoning;
OpenRouter and Responses retain supported summary or encrypted reasoning.
OpenRouter translates maximum effort to `xhigh`; the other profiles use `max`.
For existing configs that omit `profile`, the names `deepseek` and `openrouter`
select their corresponding profiles; other names select `responses`.
Use an explicit profile for custom aliases. See the example config.

`--provider NAME` overrides the configured default for a new task; the whole
run, including checkpoints, uses the selected provider and its model.
A missing provider, model, or credential is an
error; Mai never falls back to another provider. Upstream model IDs retain their
exact casing. OpenRouter's maximum effort is sent as `xhigh`; native DeepSeek
uses `max`.

## Saving and resuming

```sh
mai "add useful tests for the parser" --persist
mai "fix the failures" --last
mai "review the result carefully" --last
```

`--persist` and `--last` cannot be combined. Resume restores the original
working directory, conversation, provider, profile, endpoint, model, and
reasoning effort. Plain resume ignores current config files. Use
`--last --provider NAME` to load current provider config and switch backends;
because a saved model ID is provider-specific, the switch adopts the new
provider's configured model unless `--model` overrides it. `--model` and
`--effort` also override the saved values on resume. The switch is saved for
subsequent resumes. Credentials are read from the saved environment
variable name each time, so rotating a key still works. Sessions saved
before backend settings were recorded use the built-in default provider's endpoint.
Python starts a new environment when the previous run ends.

`--fork` and `--fork-from ID` start a new saved session from an existing one,
copying its history and transcript archive while leaving the parent untouched —
the parent may still be running. The fork moves `current` to the child and
implies saving, so it cannot be combined with `--last` or `--persist`.
`--provider`, `--model`, and `--effort` override the inherited values with the
same reasoning-boundary rules as resume.

On a provider override, previous reasoning stays in original task history but
is excluded from requests to the new backend. User messages, assistant answers,
tool calls/results, and context edits remain intact. This avoids replaying
provider-specific encrypted reasoning to a different server.

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

Each Responses request has a 10-minute time-to-first-byte and idle timeout. Each
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
