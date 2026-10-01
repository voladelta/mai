#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cases_dir="$repo_root/evals/cases"
. "$repo_root/evals/lib.sh"
result_dir=$(mktemp -d "${TMPDIR:-/tmp}/mai-eval.XXXXXX") || exit 1
trap 'printf "Results: %s\n" "$result_dir"' EXIT
mai_bin=${MAI_EVAL_BIN:-"$result_dir/mai"}
model=${MAI_EVAL_MODEL:-ds-flash}

if [ -z "${MAI_EVAL_BIN:-}" ]; then
    if ! (cd "$repo_root" && go build -o "$mai_bin" ./cmd/mai); then
        printf 'Mai build failed. Results: %s\n' "$result_dir" >&2
        exit 1
    fi
fi

if [ "$#" -eq 0 ]; then
    for case_dir in "$cases_dir"/*/; do
        [ -d "$case_dir" ] || continue
        set -- "$@" "$(basename "$case_dir")"
    done
fi

overall=0
printf 'case\tgrade\tseconds\trequests\ttools\tnonzero_tools\n'

for case_name do
    case_dir="$cases_dir/$case_name"
    if [ "$(basename "$case_name")" != "$case_name" ] || [ ! -d "$case_dir/workspace" ] || [ ! -f "$case_dir/prompt.txt" ] || [ ! -f "$case_dir/check_test.go.txt" ]; then
        printf 'Unknown or incomplete case: %s\n' "$case_name" >&2
        overall=1
        continue
    fi

    prompt=$(cat "$case_dir/prompt.txt")
    run_case "$case_name" "$case_dir/workspace" "$case_dir/check_test.go.txt" "$prompt" go test ./...
    if [ "$grade" = fail ]; then
        overall=1
    fi

    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$case_name" "$grade" "$seconds" "$requests" "$tools" "$nonzero_tools"
done

exit "$overall"
