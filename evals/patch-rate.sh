#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
fixture="$repo_root/evals/patch-rate/workspace"
result_dir=$(mktemp -d "${TMPDIR:-/tmp}/mai-patch-rate.XXXXXX") || exit 1
trap 'printf "Results: %s\n" "$result_dir"' EXIT
mai_bin=${MAI_EVAL_BIN:-"$result_dir/mai"}
model=${MAI_EVAL_MODEL:-sol}

if [ -z "${MAI_EVAL_BIN:-}" ]; then
    (cd "$repo_root" && go build -o "$mai_bin" ./cmd/mai)
fi

printf 'target\tgrade\tseconds\trequests\ttools\tnonzero_tools\n'

for target in \
    HandshakeTimeout StartupTimeout ShutdownTimeout RetryTimeout \
    UploadLimit DownloadLimit CacheLimit QueueLimit \
    WarmupDelay BackoffDelay PollDelay FlushDelay
do
    output_dir="$result_dir/$target"
    work_dir="$output_dir/workspace"
    mkdir -p "$work_dir"
    cp -R "$fixture/." "$work_dir/"
    git -C "$work_dir" init -q
    git -C "$work_dir" add -A
    git -C "$work_dir" -c user.name='Mai Eval' -c user.email='mai-eval@example.invalid' commit -qm 'Initial task state'

    prompt="Change $target to return 45. Keep every other function at 30. Add a test for the intended value and run the Go tests."
    started=$(date +%s)
    if (cd "$work_dir" && "$mai_bin" "$prompt" --jsonl --no-input -m "$model" > "$output_dir/events.jsonl" 2> "$output_dir/mai.stderr"); then
        mai_status=0
    else
        mai_status=$?
    fi
    ended=$(date +%s)

    git -C "$work_dir" add -N .
    git -C "$work_dir" diff --no-ext-diff HEAD > "$output_dir/changes.diff"
    cp "$repo_root/evals/patch-rate/check_test.go.txt" "$work_dir/check_test.go"
    if (cd "$work_dir" && MAI_EVAL_TARGET="$target" go test ./... > "$output_dir/grade.txt" 2>&1); then
        grade_status=0
    else
        grade_status=$?
    fi
    rm "$work_dir/check_test.go"

    grade=pass
    if [ "$mai_status" -ne 0 ] || [ "$grade_status" -ne 0 ]; then
        grade=fail
    fi
    if awk '/wrong edit:/ { found=1 } END { exit !found }' "$output_dir/grade.txt"; then
        grade=wrong-edit
    fi

    requests=$(awk '/"type":"model.completed"/ { n++ } END { print n+0 }' "$output_dir/events.jsonl")
    tools=$(awk '/"type":"tool.started"/ { n++ } END { print n+0 }' "$output_dir/events.jsonl")
    nonzero_tools=$(awk '/"type":"tool.completed"/ && /\\"ok\\":false/ { n++ } END { print n+0 }' "$output_dir/events.jsonl")

    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$target" "$grade" "$((ended-started))" "$requests" "$tools" "$nonzero_tools"
    printf 'mai_exit=%s grader_exit=%s\n' "$mai_status" "$grade_status" > "$output_dir/status.txt"
done
