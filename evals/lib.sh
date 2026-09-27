#!/bin/sh

run_case() {
    case_name=$1
    fixture=$2
    check=$3
    prompt=$4
    shift 4

    output_dir="$result_dir/$case_name"
    work_dir="$output_dir/workspace"
    mkdir -p "$work_dir"
    cp -R "$fixture/." "$work_dir/"
    git -C "$work_dir" init -q
    git -C "$work_dir" add -A
    git -C "$work_dir" -c user.name='Mai Eval' -c user.email='mai-eval@example.invalid' commit -qm 'Initial task state'

    started=$(date +%s)
    if (cd "$work_dir" && "$mai_bin" "$prompt" --jsonl --no-input -m "$model" > "$output_dir/events.jsonl" 2> "$output_dir/mai.stderr"); then
        mai_status=0
    else
        mai_status=$?
    fi
    ended=$(date +%s)

    git -C "$work_dir" add -N .
    git -C "$work_dir" diff --no-ext-diff HEAD > "$output_dir/changes.diff"
    cp "$check" "$work_dir/check_test.go"
    if (cd "$work_dir" && "$@" > "$output_dir/grade.txt" 2>&1); then
        grade_status=0
    else
        grade_status=$?
    fi
    rm "$work_dir/check_test.go"

    requests=$(awk '/"type":"model.completed"/ { n++ } END { print n+0 }' "$output_dir/events.jsonl")
    tools=$(awk '/"type":"tool.started"/ { n++ } END { print n+0 }' "$output_dir/events.jsonl")
    nonzero_tools=$(awk '/"type":"tool.completed"/ && /\\"ok\\":false/ { n++ } END { print n+0 }' "$output_dir/events.jsonl")
    grade=pass
    if [ "$mai_status" -ne 0 ] || [ "$grade_status" -ne 0 ]; then
        grade=fail
    fi

    seconds=$((ended-started))
    printf 'mai_exit=%s grader_exit=%s\n' "$mai_status" "$grade_status" > "$output_dir/status.txt"
}
