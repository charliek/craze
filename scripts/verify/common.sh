#!/usr/bin/env bash
# shellcheck shell=bash
# common.sh — sourced by every scripts/verify/*.sh (never run on its own).
#
# The contract every script keeps (Plan 037 §3.1):
#   - it verifies a COMMITTED sha, exported with `git archive` into a unique
#     short directory, mktemp -d "${CRAZE_VERIFY_TMP:-/tmp}/cv-<label>.XXXX",
#     refused (exit 2) when that path would be over 40 characters (SF-b),
#     checked with the label, before anything is written;
#   - a label matches ^[A-Za-z0-9._-]{1,40}$;
#   - output goes to ${CRAZE_VERIFY_OUT:-${XDG_CACHE_HOME:-$HOME/.cache}/craze-verify}/<label>/,
#     never under the repo, and neither is the export: both are checked as
#     physical paths, symlinks resolved and `..` collapsed;
#   - every long step runs in its own process group, so SIGTERM/SIGINT (or
#     v1's watchdog) ends everything it started; the export directory is
#     removed on every exit. A script launched with `&` from a non-interactive
#     shell inherits SIGINT ignored, which bash cannot re-arm: stop a
#     background run with SIGTERM (`kill <pid>`);
#   - the last line of every run is <NAME>_EXIT=<rc> elapsed=<s>s sha=<sha> log=<path>
#     (--help is not a run and prints none);
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
CV_EXPORT_BASE=
CV_PHYS=
CV_STOP_FAILED=0

# The help footer every usage text ends with.
CV_HELP_COMMON='Environment:
  CRAZE_VERIFY_REPO   the repository (default: the one holding this script)
  CRAZE_VERIFY_TMP    where the export goes (default /tmp; the path must stay <= 40 chars)
  CRAZE_VERIFY_OUT    output root (default ${XDG_CACHE_HOME:-~/.cache}/craze-verify); <label>/ under it
Exit: 0 ok, 1 test failure, 2 harness failure (usage, label, sha, export,
build), 3 platform tool missing, 4 tui lock refused (v1), 130/143 interrupted.
Stop a run with SIGTERM (kill <pid>): one launched with & from a
non-interactive shell inherits SIGINT ignored, and bash cannot re-arm it.
The last line is always <NAME>_EXIT=<rc> elapsed=<s>s sha=<sha> log=<path>.
A failure is diagnosed, never re-run.'

# cv_wants_help "$@": true when any argument is --help (or the first is -h).
# Accepted residual: a --help exit prints no summary line (help is not a run).
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
	# A SIGINT inherited as ignored (a `&` launch with job control off) stays
	# ignored whatever this says: SIGTERM is the documented stop signal.
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
	# A group that survives SIGKILL is a harness failure whatever the run's
	# status was (0, a test's 1, SIGINT's 130, SIGTERM's 143): the exit is 2,
	# and CV_STOP_FAILED tells cv_cleanup_hook (v1's lock) not to release what
	# it guards.
	cv_stop_group "$CV_CHILD" || CV_STOP_FAILED=1
	CV_CHILD=
	cv_stop_group "$CV_WATCHDOG" || CV_STOP_FAILED=1
	CV_WATCHDOG=
	((CV_STOP_FAILED)) && rc=2
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

# cv_physical <path>: the absolute physical path: every existing component
# resolved through symlinks (cd -P), `.` and `..` collapsed, the missing tail
# appended as is (it cannot be a symlink). Fails on a component that exists
# but is no directory (a file, a dangling symlink) or cannot be entered.
cv_physical() {
	local rest=$1 cur='' c nmiss=0
	[[ $rest == /* ]] || rest=$PWD/$rest
	while [[ -n $rest ]]; do
		c=${rest%%/*}
		if [[ $rest == */* ]]; then rest=${rest#*/}; else rest=; fi
		case $c in
		'' | .) continue ;;
		..)
			cur=${cur%/*}
			((nmiss > 0)) && nmiss=$((nmiss - 1))
			continue
			;;
		esac
		if ((nmiss > 0)) || [[ ! -e $cur/$c && ! -L $cur/$c ]]; then
			cur=$cur/$c
			nmiss=$((nmiss + 1))
		elif [[ -d $cur/$c ]]; then
			cur=$(cd -P -- "$cur/$c" 2>/dev/null && pwd -P) || return 1
			[[ $cur == / ]] && cur=
		else
			return 1
		fi
	done
	printf '%s\n' "${cur:-/}"
}

# cv_outside_repo <what> <path>: dies (2) when <path>, resolved physically,
# is the repository or inside it; else sets CV_PHYS to the resolved path.
cv_outside_repo() {
	local p root
	p=$(cv_physical "$2") || die 2 "$1 $2 cannot be resolved (a component is not a directory)"
	root=$(cv_physical "$ROOT") || die 2 "the repository $ROOT cannot be resolved"
	case "$p/" in
	"$root"/*) die 2 "$1 $2 is inside the repository $root (resolved: $p)" ;;
	esac
	CV_PHYS=$p
}

# cv_label <label>: validates it, then (before anything is written) the
# export path's length and that neither the output dir nor the export base is
# inside the repository; then creates $OUT.
cv_label() {
	local label=$1
	[[ $label =~ ^[A-Za-z0-9._-]{1,40}$ ]] ||
		die 2 "label '$label' must match ^[A-Za-z0-9._-]{1,40}\$"
	[[ $label =~ ^\.+$ ]] && die 2 "label '$label' is only dots"
	CV_LABEL=$label
	local base=${CRAZE_VERIFY_TMP:-/tmp}
	cv_export_len_ok "$base/cv-$label.XXXX"
	cv_outside_repo "export dir (CRAZE_VERIFY_TMP)" "$base"
	# The export keeps the logical base: the 40-character budget is for the
	# path the tests see ($PWD), and /tmp is /private/tmp on macOS.
	CV_EXPORT_BASE=$base
	local root_out=${CRAZE_VERIFY_OUT:-${XDG_CACHE_HOME:-$HOME/.cache}/craze-verify}
	cv_outside_repo "output dir" "$root_out/$label"
	OUT=$CV_PHYS
	mkdir -p "$OUT" || die 2 "cannot create output dir $OUT"
}

# cv_export_len_ok <pattern>: dies (2) when the export path is over 40 characters.
cv_export_len_ok() {
	((${#1} <= 40)) ||
		die 2 "export path $1 is ${#1} characters, over the 40-character limit (SF-b): use a shorter label or CRAZE_VERIFY_TMP"
}

# cv_resolve <rev>: the commit to verify, as CV_SHA_FULL / CV_SHA.
cv_resolve() {
	CV_SHA_FULL=$(git -C "$ROOT" rev-parse --verify --quiet "$1^{commit}") ||
		die 2 "not a commit in $ROOT: $1"
	CV_SHA=$(git -C "$ROOT" rev-parse --short "$CV_SHA_FULL")
}

# cv_export_dir: a fresh unique short directory, removed on exit, in CV_LAST_EXPORT.
cv_export_dir() {
	[[ -n $CV_EXPORT_BASE ]] || die 2 "cv_export_dir before cv_label"
	local pattern=$CV_EXPORT_BASE/cv-$CV_LABEL.XXXX
	cv_export_len_ok "$pattern"
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
# stdin from /dev/null, stdout and stderr appended to <log>, fd 9 (v1's tui
# lock) not inherited: a test's detached descendant must never hold the lock
# (v1's keeper holds it while the group lives). A shell function is fine as
# <cmd>.
cv_start() {
	local log=$1
	shift
	set -m
	"$@" </dev/null >>"$log" 2>&1 9>&- &
	CV_CHILD=$!
	set +m
	echo "$CV_NAME running pgid=$CV_CHILD log=$log"
}

# cv_wait: wait for CV_CHILD, then end whatever is left in its group. Returns
# its rc; dies (2) when the group survives SIGKILL, so nothing it guards (v1's
# lock) is released and no success is reported.
# Accepted residual: the leader is reaped before the group is signalled, so the
# pgid could be reused in between (a pid wrap within microseconds).
cv_wait() {
	local rc=0 g=$CV_CHILD
	wait "$g" || rc=$?
	if ! cv_stop_group "$g"; then
		CV_CHILD= CV_STOP_FAILED=1
		die 2 "process group $g survived SIGKILL: diagnose it before running again"
	fi
	CV_CHILD=
	return "$rc"
}

# cv_run <log> <cmd…>: cv_start + cv_wait.
cv_run() {
	cv_start "$@"
	cv_wait
}

# cv_stop_group <pgid>: TERM the group, give it 10 s, then KILL; returns 0
# once no process is left in it, 1 when one survives SIGKILL.
# Accepted residual: a descendant a test detached with setsid (a shell's
# sleeper, a host, an agent) is in no group of ours and can outlive an
# interrupted run, exactly as with a bare `go test`; it ends on its own timeout.
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
