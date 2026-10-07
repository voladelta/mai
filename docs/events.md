# JSONL events

[Back to README](../README.md) · [CLI reference](cli.md) · [Eval instructions](../evals/README.md)

Use `--jsonl` to write one JSON event per line on standard output:

```bash
mai "add tests for the parser" --jsonl > run.jsonl
```

Events include `task.started`, `model.started`, `model.delta`,
`model.completed`, `model.failed`, `compaction.completed`, `tool.started`, `tool.completed`,
`task.completed`, and `error`. Model text is
reported in `model.delta` events instead of being printed directly. Progress
messages remain on standard error. A `tool.completed` event includes its output
up to 256 KiB; larger outputs report `output_bytes` and `output_omitted` instead.
Completed model, tool, and task events include `duration_ms` for elapsed time.
Completed model events also include `input_tokens`, `output_tokens`, and
`cached_input_tokens` when the backend supplies them. Missing fields mean
unavailable; `total_tokens` keeps its existing context-size meaning.
`compaction.completed` includes elapsed time and a `usage` object with the
backend's available token fields, so checkpoint generation can be counted too.
It also carries a `changes` array describing what the checkpoint replaced: each
entry has `kind` (`"compacted"`), `source`, `reason`, the replaced `records`
count, per-tool `tool_calls` counts, the `call_ids` usable as `mai.history`
search anchors, and `call_ids_omitted` when the list was capped.
Model duration covers the full model request.
Tool duration covers execution of that call; task duration covers the agent run.
The default output remains human-readable.

`task.started` includes `provider`, `model`, and `effort`. The provider is the
selected configuration name, and the model is the upstream model ID sent in
requests. When the run forked a saved session (`--fork` or `--fork-from`), it
also includes `parent_id` (the source session ID) and `forked_at_turn` (the
number of history items copied at fork time).

Nested Python host calls count toward the outer
Python tool duration. With `jq` installed, summarize a run with:

```sh
./evals/timing.sh run.jsonl
```
