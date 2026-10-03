#!/bin/sh

if ! command -v jq > /dev/null 2>&1; then
    printf 'Eval runners require jq\n' >&2
    exit 2
fi

provider=${MAI_EVAL_PROVIDER:-deepseek}
case ${MAI_EVAL_MODE:-pro} in
    pro) mode_flag= ;;
    flash) mode_flag=--f ;;
    max) mode_flag=--max ;;
    *) printf 'MAI_EVAL_MODE must be pro, flash, or max\n' >&2; exit 2 ;;
esac

run_case() {
    case_name=$1
    fixture=$2
    check=$3
    prompt=$4
    shift 4
    followup_file="$(dirname "$fixture")/followup.txt"
    persist_flag=
    if [ -f "$followup_file" ]; then
        persist_flag=--persist
    fi

    output_dir="$result_dir/$case_name"
    work_dir="$output_dir/workspace"
    mkdir -p "$work_dir"
    cp -R "$fixture/." "$work_dir/"
    if [ -n "${MAI_EVAL_CONFIG:-}" ]; then
        cp "$MAI_EVAL_CONFIG" "$work_dir/.mai.config"
    fi
    git -C "$work_dir" init -q
    git -C "$work_dir" add -A
    git -C "$work_dir" -c user.name='Mai Eval' -c user.email='mai-eval@example.invalid' commit -qm 'Initial task state'

    started=$(date +%s)
    if (cd "$work_dir" && "$mai_bin" "$prompt" --provider "$provider" ${mode_flag:+"$mode_flag"} ${persist_flag:+"$persist_flag"} --jsonl --no-input --skip-skills > "$output_dir/events.jsonl" 2> "$output_dir/mai.stderr"); then
        mai_status=0
    else
        mai_status=$?
    fi
    if [ "$mai_status" -eq 0 ] && [ -f "$followup_file" ]; then
        followup_prompt=$(cat "$followup_file")
        if (cd "$work_dir" && "$mai_bin" "$followup_prompt" --last --provider "$provider" --jsonl --no-input --skip-skills >> "$output_dir/events.jsonl" 2>> "$output_dir/mai.stderr"); then
            mai_status=0
        else
            mai_status=$?
        fi
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

    counts=$(jq -r -s '
        [
            ([.[] | select(.type == "model.completed" or .type == "model.failed")] | length),
            ([.[] | select(.type == "tool.started")] | length),
            ([.[] | select(.type == "tool.completed")
                | .output | select(type == "string") | fromjson?
                | select(type == "object") | select(.ok == false)] | length)
        ] | @tsv
    ' "$output_dir/events.jsonl")
    set -- $counts
    requests=$1
    tools=$2
    nonzero_tools=$3

    grade=pass
    if [ "$mai_status" -ne 0 ] || [ "$grade_status" -ne 0 ]; then
        grade=fail
    fi

    seconds=$((ended-started))
    printf 'mai_exit=%s grader_exit=%s\n' "$mai_status" "$grade_status" > "$output_dir/status.txt"
}
