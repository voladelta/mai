#!/bin/sh
set -eu

if [ "$#" -eq 0 ]; then
    printf 'Usage: %s EVENTS.jsonl [EVENTS.jsonl ...]\n' "$0" >&2
    exit 2
fi
if ! command -v jq > /dev/null 2>&1; then
    printf 'timing.sh requires jq\n' >&2
    exit 2
fi

printf 'events\ttask_ms\tmodel_ms\ttool_ms\tother_ms\n'
for events_path do
    jq -r -s --arg path "$events_path" '
        def duration($kind):
            [.[] | select(.type == $kind) | (.duration_ms // 0)] | add // 0;

        (duration("model.completed") + duration("model.failed")) as $model
        # Sidekick durations enclose worker events already counted here.
        | ([.[] | select(.name != "sidekick")] | duration("tool.completed")) as $tool
        | ([.[] | select(.type == "task.completed") | .duration_ms] | last // null) as $task
        | [$path, $task, $model, $tool, (if $task == null then null else $task - $model - $tool end)]
        | @tsv
    ' "$events_path"
done
