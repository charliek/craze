#!/usr/bin/env bash
# shellcheck shell=bash
# common.sh — sourced by every scripts/verify/*.sh (never run on its own).
#
# The contract every script keeps (Plan 037 §3.1):
#   - it verifies a COMMITTED sha, exported with `git archive` into a unique
#     short directory, mktemp -d "${CRAZE_VERIFY_TMP:-/tmp}/cv-<label>.XXXX",
#     refused (exit 2) when that path would be over 40 characters (SF-b);
#   - a label matches ^[A-Za-z0-9._-]{1,40}$;
#   - output goes to ${CRAZE_VERIFY_OUT:-${XDG_CACHE_HOME:-$HOME/.cache}/craze-verify}/<label>/,
#     never under the repo;
#   - every long step runs in its own process group, so SIGINT/SIGTERM (or
#     v1's watchdog) ends everything it started; the export directory is
#     removed on every exit;
#   - the last line is always <NAME>_EXIT=<rc> elapsed=<s>s sha=<sha> log=<path>;
#   - exit 0 ok, 1 a test failure, 2 a harness failure (usage, label, sha,
#     export, build), 3 a platform tool missing, 4 v1's tui lock refused,
#     130/143 interrupted (SIGINT/SIGTERM);
#   - nothing is ever retried: a failure is diagnosed, not re-run.
#
# The repository is the one holding these scripts, or $CRAZE_VERIFY_REPO.

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
	awk 'NR > 2 && /^#/ { sub(/^# ?/, ""); print; next } NR > 2 { exit }' "$0"
	[[ ${1:-} == --help || ${1:-} == -h ]] && exit 0
	exit 2
fi

set -u -o pipefail

CV_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)
export PATH="$HOME/.local/share/mise/shims:$HOME/.local/bin:/opt/homebrew/bin:$PATH"

CV_NAME=VERIFY
CV_START=$(date +%s)
CV_SHA=-
CV_SHA_FULL=
CV_LOG=-
CV_LABEL=
OUT=
ROOT=
CV_EXPORTS=()
CV_CHILD=
CV_WATCHDOG=
CV_LAST_EXPORT=

# The help footer every usage text ends with.
CV_HELP_COMMON='Environment:
  CRAZE_VERIFY_REPO   the repository (default: the one holding this script)
  CRAZE_VERIFY_TMP    where the export goes (default /tmp; the path must stay <= 40 chars)
  CRAZE_VERIFY_OUT    output root (default ${XDG_CACHE_HOME:-~/.cache}/craze-verify); <label>/ under it
Exit: 0 ok, 1 test failure, 2 harness failure (usage, label, sha, export,
build), 3 platform tool missing, 4 tui lock refused (v1), 130/143 interrupted.
The last line is always <NAME>_EXIT=<rc> elapsed=<s>s sha=<sha> log=<path>.
A failure is diagnosed, never re-run.'

# cv_wants_help "$@": true when any argument is --help (or the first is -h).
cv_wants_help() {
	[[ ${1:-} == -h ]] && return 0
	local a
	for a in "$@"; do [[ $a == --help ]] && return 0; done
	return 1
}

die() { # die <rc> <message…>
	local rc=$1
	shift
	printf '%s: %s\n' "$(basename "$0")" "$*" >&2
	exit "$rc"
}

cv_stamp() { printf '%s-%s' "$(date +%Y%m%d-%H%M%S)" "$$"; }

# cv_setup <NAME>: names the summary line and installs the traps, so every
# exit from here on (including a refusal) ends with that line.
cv_setup() {
	CV_NAME=$1
	trap cv_on_exit EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	trap 'exit 129' HUP
	local repo=${CRAZE_VERIFY_REPO:-$CV_DIR}
	ROOT=$(git -C "$repo" rev-parse --show-toplevel 2>/dev/null) ||
		die 2 "not a git repository: $repo"
}

cv_on_exit() {
	local rc=$?
	trap '' INT TERM HUP
	cv_stop_group "$CV_CHILD"
	CV_CHILD=
	cv_stop_group "$CV_WATCHDOG"
	CV_WATCHDOG=
	if declare -F cv_cleanup_hook >/dev/null; then cv_cleanup_hook; fi
	local d
	for d in "${CV_EXPORTS[@]+"${CV_EXPORTS[@]}"}"; do
		if [[ -n $d && (-e $d || -L $d) ]]; then
			chmod -R u+w "$d" 2>/dev/null
			rm -rf "$d"
		fi
	done
	summary_line "$rc"
	exit "$rc"
}

# summary_line <rc>: the last line of every run.
summary_line() {
	printf '%s_EXIT=%s elapsed=%ss sha=%s log=%s\n' \
		"$CV_NAME" "$1" "$(($(date +%s) - CV_START))" "$CV_SHA" "$CV_LOG"
}

# cv_label <label>: validates it and creates $OUT.
cv_label() {
	local label=$1
	[[ $label =~ ^[A-Za-z0-9._-]{1,40}$ ]] ||
		die 2 "label '$label' must match ^[A-Za-z0-9._-]{1,40}\$"
	[[ $label =~ ^\.+$ ]] && die 2 "label '$label' is only dots"
	CV_LABEL=$label
	local root_out=${CRAZE_VERIFY_OUT:-${XDG_CACHE_HOME:-$HOME/.cache}/craze-verify}
	[[ $root_out == /* ]] || root_out=$PWD/$root_out
	OUT=$root_out/$label
	case "$OUT/" in
	"$ROOT"/*) die 2 "output dir $OUT is inside the repository $ROOT" ;;
	esac
	mkdir -p "$OUT" || die 2 "cannot create output dir $OUT"
}

# cv_resolve <rev>: the commit to verify, as CV_SHA_FULL / CV_SHA.
cv_resolve() {
	CV_SHA_FULL=$(git -C "$ROOT" rev-parse --verify --quiet "$1^{commit}") ||
		die 2 "not a commit in $ROOT: $1"
	CV_SHA=$(git -C "$ROOT" rev-parse --short "$CV_SHA_FULL")
}

# cv_export_dir: a fresh unique short directory, removed on exit, in CV_LAST_EXPORT.
cv_export_dir() {
	local base=${CRAZE_VERIFY_TMP:-/tmp}
	local pattern=$base/cv-$CV_LABEL.XXXX
	((${#pattern} <= 40)) ||
		die 2 "export path $pattern is ${#pattern} characters, over the 40-character limit (SF-b): use a shorter label or CRAZE_VERIFY_TMP"
	CV_LAST_EXPORT=$(mktemp -d "$pattern" 2>/dev/null) ||
		die 2 "export failed: mktemp -d $pattern"
	CV_EXPORTS+=("$CV_LAST_EXPORT")
}

# export_tree <full-sha>: export that commit's tree into a fresh directory
# (CV_LAST_EXPORT). The working tree is never used: it may hold edits.
export_tree() {
	local sha=$1
	cv_export_dir
	echo "$CV_NAME export=$CV_LAST_EXPORT sha=$(git -C "$ROOT" rev-parse --short "$sha")"
	git -C "$ROOT" archive --format=tar "$sha" | tar -x -C "$CV_LAST_EXPORT" ||
		die 2 "export failed: git archive $sha | tar -x -C $CV_LAST_EXPORT"
}

# cv_start <log> <cmd…>: start cmd in its own process group (pgid = CV_CHILD),
# stdin from /dev/null, stdout and stderr appended to <log>, fd 9 (v1's lock)
# not inherited. A shell function is fine as <cmd>.
cv_start() {
	local log=$1
	shift
	set -m
	"$@" </dev/null >>"$log" 2>&1 9>&- &
	CV_CHILD=$!
	set +m
	echo "$CV_NAME running pgid=$CV_CHILD log=$log"
}

# cv_wait: wait for CV_CHILD, then end whatever is left in its group. Returns its rc.
cv_wait() {
	local rc=0 g=$CV_CHILD
	wait "$g" || rc=$?
	cv_stop_group "$g"
	CV_CHILD=
	return "$rc"
}

# cv_run <log> <cmd…>: cv_start + cv_wait.
cv_run() {
	cv_start "$@"
	cv_wait
}

# cv_stop_group <pgid>: TERM the group, give it 10 s, then KILL; returns
# once no process is left in it.
cv_stop_group() {
	local g=${1:-} i
	[[ -n $g ]] || return 0
	kill -0 -- "-$g" 2>/dev/null || return 0
	kill -TERM -- "-$g" 2>/dev/null
	for ((i = 0; i < 100; i++)); do
		kill -0 -- "-$g" 2>/dev/null || return 0
		sleep 0.1
	done
	kill -KILL -- "-$g" 2>/dev/null
	for ((i = 0; i < 50; i++)); do
		kill -0 -- "-$g" 2>/dev/null || return 0
		sleep 0.1
	done
	echo "$CV_NAME: process group $g survived SIGKILL" >&2
	return 1
}

# cv_go_verdict <rc> <log>: map a `go test` exit to the contract: 0, 1 for a
# test failure, 2 when the only failures are packages that did not build.
cv_go_verdict() {
	local rc=$1 log=$2
	((rc == 0)) && return 0
	if grep -qE '^(--- FAIL|FAIL[[:space:]]+[^[:space:]]+[[:space:]]+[0-9.]+s$|panic:)' "$log"; then
		return 1
	fi
	if grep -qE '^FAIL[[:space:]]+[^[:space:]]+[[:space:]]+\[(build|setup) failed\]' "$log"; then
		echo "$CV_NAME: build failed (see $CV_LOG)" >&2
		return 2
	fi
	return 1
}
