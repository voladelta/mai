#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
. "$repo_root/evals/lib.sh"
fixture="$repo_root/evals/patch-rate/workspace"
result_dir=$(mktemp -d "${TMPDIR:-/tmp}/mai-patch-rate.XXXXXX") || exit 1
trap 'printf "Results: %s\n" "$result_dir"' EXIT
mai_bin=${MAI_EVAL_BIN:-"$result_dir/mai"}
model=${MAI_EVAL_MODEL:-ds-pro}

if [ -z "${MAI_EVAL_BIN:-}" ]; then
    (cd "$repo_root" && go build -o "$mai_bin" ./cmd/mai)
fi

printf 'target\tgrade\tseconds\trequests\ttools\tnonzero_tools\n'
overall=0

for target in \
    HandshakeTimeout StartupTimeout ShutdownTimeout RetryTimeout \
    UploadLimit DownloadLimit CacheLimit QueueLimit \
    WarmupDelay BackoffDelay PollDelay FlushDelay
do
    prompt="Change $target to return 45. Keep every other function at 30. Add a test for the intended value and run the Go tests."
    run_case "$target" "$fixture" "$repo_root/evals/patch-rate/check_test.go.txt" "$prompt" env MAI_EVAL_TARGET="$target" go test ./...
    if awk '/wrong edit:/ { found=1 } END { exit !found }' "$output_dir/grade.txt"; then
        grade=wrong-edit
    fi

    if [ "$grade" != pass ]; then
        overall=1
    fi

    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$target" "$grade" "$seconds" "$requests" "$tools" "$nonzero_tools"
done

exit "$overall"
