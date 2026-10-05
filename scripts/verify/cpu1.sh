#!/usr/bin/env bash
# cpu1.sh <label> <sha> <pkg>… — the pre-push one-CPU run on a committed tree.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

usage() {
	cat <<EOF
usage: cpu1.sh <label> <sha> <pkg>…

The pre-push one-CPU run (Plan 021 X45) on the COMMITTED tree at <sha>:
  CRAZE_GOLDEN_TRANSPORT=both go test -cpu=1 -count=2 -timeout 30m <pkg>…
<pkg> is a go package pattern relative to the repo root (./internal/tui,
./internal/engine/...). Prints the ok/FAIL/panic lines.

Log: \$OUT/cpu1-<stamp>.log
$CV_HELP_COMMON
EOF
}

if cv_wants_help "$@"; then usage; exit 0; fi
cv_setup CPU1
[[ $# -ge 3 ]] || { usage >&2; die 2 "want <label> <sha> <pkg>…"; }
cv_label "$1"
cv_resolve "$2"
shift 2
for p in "$@"; do [[ $p == -* ]] && die 2 "a package may not start with '-': $p"; done
CV_LOG=$OUT/cpu1-$(cv_stamp).log
: >"$CV_LOG"
export_tree "$CV_SHA_FULL"
tree=$CV_LAST_EXPORT

run_cpu1() { cd "$tree" && CRAZE_GOLDEN_TRANSPORT=both go test -cpu=1 -count=2 -timeout 30m "$@"; }
cv_run "$CV_LOG" run_cpu1 "$@"
rc=$?
grep -E '^(--- FAIL|FAIL|ok|panic)' "$CV_LOG" | head -20
cv_go_verdict "$rc" "$CV_LOG"
exit $?
