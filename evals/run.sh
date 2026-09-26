#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cases_dir="$repo_root/evals/cases"
result_dir=$(mktemp -d "${TMPDIR:-/tmp}/mai-eval.XXXXXX") || exit 1
trap 'printf "Results: %s\n" "$result_dir"' EXIT
mai_bin=${MAI_EVAL_BIN:-"$result_dir/mai"}
model=${MAI_EVAL_MODEL:-luna}

if [ -z "${MAI_EVAL_BIN:-}" ]; then
    if ! (cd "$repo_root" && go build -o "$mai_bin" ./cmd/mai); then
        printf 'Mai build failed. Results: %s\n' "$result_dir" >&2
        exit 1
    fi
fi

if [ "$#" -eq 0 ]; then
    set -- empty-parser complete-todo startup-timeout
fi

overall=0
printf 'case\tgrade\tseconds\trequests\ttools\tnonzero_tools\n'

for case_name do
    case "$case_name" in
        empty-parser|complete-todo|startup-timeout) ;;
        *)
            printf 'Unknown case: %s\n' "$case_name" >&2
            overall=1
            continue
            ;;
    esac

    case_dir="$cases_dir/$case_name"
    if [ ! -d "$case_dir/workspace" ] || [ ! -f "$case_dir/prompt.txt" ] || [ ! -f "$case_dir/check_test.go.txt" ]; then
        printf 'Unknown or incomplete case: %s\n' "$case_name" >&2
        overall=1
        continue
    fi

    output_dir="$result_dir/$case_name"
    work_dir="$output_dir/workspace"
    mkdir -p "$work_dir"
    cp -R "$case_dir/workspace/." "$work_dir/"
    git -C "$work_dir" init -q
    git -C "$work_dir" add -A
    git -C "$work_dir" -c user.name='Mai Eval' -c user.email='mai-eval@example.invalid' commit -qm 'Initial task state'
    prompt=$(cat "$case_dir/prompt.txt")

    started=$(date +%s)
    if (cd "$work_dir" && "$mai_bin" "$prompt" --jsonl --no-input -m "$model" > "$output_dir/events.jsonl" 2> "$output_dir/mai.stderr"); then
        mai_status=0
    else
        mai_status=$?
    fi
    ended=$(date +%s)

    git -C "$work_dir" add -N .
    git -C "$work_dir" diff --no-ext-diff HEAD > "$output_dir/changes.diff"
    cp "$case_dir/check_test.go.txt" "$work_dir/check_test.go"
    if (cd "$work_dir" && go test ./... > "$output_dir/grade.txt" 2>&1); then
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
        overall=1
    fi

    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$case_name" "$grade" "$((ended-started))" "$requests" "$tools" "$nonzero_tools"
    printf 'mai_exit=%s grader_exit=%s\n' "$mai_status" "$grade_status" > "$output_dir/status.txt"
done

exit "$overall"
