#!/usr/bin/env bash
# smoke.sh — drive a TUI in a dedicated tmux server for a live smoke.
#
#   smoke.sh [--host local|mac] start <name> <cmd…>   a detached session at 120x36
#   smoke.sh [--host …] type <name> <text>            bracketed paste, no Enter
#   smoke.sh [--host …] key  <name> <keys…>           named keys: Enter Escape C-d BSpace …
#   smoke.sh [--host …] wait <name> <regex> <secs>    poll the pane until <regex> matches
#   smoke.sh [--host …] snap <name> <file> [--ansi]   capture the pane (--ansi keeps SGR codes)
#   smoke.sh [--host …] stop <name>                   Escape, C-d, then end the session
#
# local (the default): one tmux server per smoke, `tmux -L craze-<name>`
# (CRAZE_SMOKE_SOCK overrides the socket name), started with -f /dev/null so
# no user config applies; `stop` kills that whole server. Pane size:
# CRAZE_SMOKE_COLS x CRAZE_SMOKE_ROWS (default 120x36).
# mac: the mac-mini's smoke server through ~/.claude/plans/craze/mac-mini/mac-tmux.sh
# (`tmux -L smoke`, the only server whose panes reach the login keychain); a
# session of your own in it, and `stop` kills only that session, never the
# server. Every remote call is bounded (the privacy-prompt trap).
#
# Text goes in as a bracketed paste: `send-keys -l` drops words that are key
# names (end, home, up, tab…) in craze's TUI. Exit: 0 ok, 1 a wait timed out
# or tmux failed, 2 usage, 3 the mac-mini runbook's scripts are missing.
set -u -o pipefail

MAC_DIR=$HOME/.claude/plans/craze/mac-mini
MAC_TMUX=$MAC_DIR/mac-tmux.sh

usage() { awk 'NR > 1 && /^#/ { sub(/^# ?/, ""); print; next } NR > 1 { exit }' "$0"; }
die() {
	echo "smoke.sh: $2" >&2
	exit "$1"
}

host=local
if [[ ${1:-} == --host ]]; then
	host=${2:-}
	shift 2 || true
fi
case ${1:-} in
'' | -h | --help) usage; exit 0 ;;
esac
[[ $host == local || $host == mac ]] || die 2 "--host must be local or mac, not '$host'"
cmd=$1
name=${2:-}
[[ -n $name ]] || die 2 "$cmd needs a session name"
[[ $name =~ ^[A-Za-z0-9_-]+$ ]] || die 2 "a session name is letters, digits, _ and - only: $name"
shift 2

# bounded <secs> <cmd…>: run with a time limit where timeout(1) exists.
bounded() {
	local s=$1
	shift
	if command -v timeout >/dev/null; then timeout "$s" "$@"; else "$@"; fi
}

if [[ $host == mac ]]; then
	[[ -x $MAC_TMUX ]] ||
		die 3 "--host mac needs $MAC_TMUX: see the mac-mini runbook ($MAC_DIR/README.md); if it is missing, ask the owner"
	T() { bounded 20 "$MAC_TMUX" "$@"; }
	paste_text() { bounded 20 "$MAC_TMUX" type "$name" "$1"; }
else
	command -v tmux >/dev/null || die 1 "tmux not found"
	sock=${CRAZE_SMOKE_SOCK:-craze-$name}
	T() { bounded 20 tmux -L "$sock" "$@"; }
	paste_text() { printf '%s' "$1" | T load-buffer -b smoke - && T paste-buffer -p -d -b smoke -t "$name"; }
fi

case $cmd in
start)
	(($# >= 1)) || die 2 "start needs a command"
	if [[ $host == mac ]]; then
		T new-session -d -s "$name" -x "${CRAZE_SMOKE_COLS:-120}" -y "${CRAZE_SMOKE_ROWS:-36}" "$@"
	else
		T -f /dev/null new-session -d -s "$name" -x "${CRAZE_SMOKE_COLS:-120}" -y "${CRAZE_SMOKE_ROWS:-36}" "$@"
	fi
	;;
type)
	(($# == 1)) || die 2 "type needs exactly one text argument"
	paste_text "$1"
	;;
key)
	(($# >= 1)) || die 2 "key needs at least one key"
	T send-keys -t "$name" "$@"
	;;
wait)
	(($# == 2)) || die 2 "wait needs <regex> <secs>"
	re=$1 secs=$2
	[[ $secs =~ ^[0-9]+$ ]] || die 2 "secs must be a whole number: $secs"
	end=$(($(date +%s) + secs))
	while :; do
		pane=$(T capture-pane -p -t "$name") || die 1 "capture-pane failed (is session $name running?)"
		if grep -Eq -- "$re" <<<"$pane"; then
			echo "matched: $re"
			exit 0
		fi
		(($(date +%s) >= end)) && break
		sleep 0.25
	done
	echo "no match for: $re within ${secs}s; the pane:" >&2
	printf '%s\n' "$pane" >&2
	exit 1
	;;
snap)
	(($# >= 1 && $# <= 2)) || die 2 "snap needs <file> [--ansi]"
	file=$1
	flags=(-p)
	[[ ${2:-} == --ansi ]] && flags+=(-e)
	[[ -z ${2:-} || ${2:-} == --ansi ]] || die 2 "snap's second argument must be --ansi"
	T capture-pane "${flags[@]}" -t "$name" >"$file" || die 1 "capture-pane failed"
	echo "saved $file"
	;;
stop)
	(($# == 0)) || die 2 "stop takes no arguments"
	T send-keys -t "$name" Escape 2>/dev/null
	sleep 0.3
	T send-keys -t "$name" C-d 2>/dev/null
	sleep 0.5
	if [[ $host == mac ]]; then
		T kill-session -t "$name" 2>/dev/null
	else
		T kill-server 2>/dev/null
	fi
	echo "stopped $name"
	;;
*) die 2 "unknown command: $cmd (start, type, key, wait, snap, stop)" ;;
esac
