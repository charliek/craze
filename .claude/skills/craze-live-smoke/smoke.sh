#!/usr/bin/env bash
# smoke.sh — drive a TUI in a dedicated tmux server for a live smoke.
#
#   smoke.sh [--host local|mac] start <name> <cmd…>   a detached session at 120x36
#   smoke.sh [--host …] type <name> <text>            bracketed paste, no Enter
#   smoke.sh [--host …] key  <name> <keys…>           named keys: Enter Escape C-d BSpace …
#   smoke.sh [--host …] wait <name> <regex> <secs>    poll the pane until <regex> matches
#   smoke.sh [--host …] snap <name> <file> [--ansi]   capture the pane (--ansi keeps SGR codes)
#   smoke.sh [--host …] stop <name>                   Escape, C-d, end it, confirm it is gone
#
# local (the default): one tmux server per smoke, `tmux -L craze-<name>`
# (CRAZE_SMOKE_SOCK overrides the socket name), started with -f /dev/null so
# no user config applies; `stop` kills that whole server and confirms it is
# gone. Pane size: CRAZE_SMOKE_COLS x CRAZE_SMOKE_ROWS (default 120x36).
# mac: the mac-mini's smoke server through ~/.claude/plans/craze/mac-mini/mac-tmux.sh
# (`tmux -L smoke`, the only server whose panes reach the login keychain); a
# session of your own in it, and `stop` kills only that session, never the
# server, and confirms the session is gone.
#
# Every tmux call, local or remote, is bounded (the privacy-prompt trap) at
# CRAZE_SMOKE_BOUND_S seconds (default 20) by timeout(1), else gtimeout
# (Homebrew coreutils), else a perl fork + alarm that TERMs, then KILLs, the
# call's process group; with none of the three, smoke.sh refuses (exit 3)
# rather than make an unbounded call.
#
# Text goes in as a bracketed paste: `send-keys -l` drops words that are key
# names (end, home, up, tab…) in craze's TUI. Exit: 0 ok, 1 a wait timed out,
# tmux failed or `stop` could not confirm the end, 2 usage, 3 no bounded
# runner or the mac-mini runbook's scripts are missing, 124 a call hit its
# bound (every command, `wait`, `snap` and `stop` included).
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

# The bounded runner: `<runner> <secs> <cmd…>` exits 124 when the call
# outlives <secs>, after ending its whole process group (an ssh under
# mac-tmux.sh included). The perl fallback mirrors `timeout -k 2`.
# Accepted residual: GNU timeout signals the leader's group, but when the
#   leader dies on TERM it exits at once, so a TERM-resistant descendant gets
#   no KILL (a tmux client spawns none; mac-tmux.sh's ssh dies on TERM).
# Accepted residual: the perl fallback's final waitpid can block on a child
#   stuck in uninterruptible I/O, which no signal ends.
BOUND_PERL='
my $s = shift;
my $pid = fork;
defined $pid or die "smoke.sh: fork: $!\n";
if ($pid == 0) { setpgrp(0, 0); exec { $ARGV[0] } @ARGV or die "smoke.sh: exec $ARGV[0]: $!\n"; }
setpgrp($pid, $pid);
$SIG{ALRM} = sub { kill "TERM", -$pid; sleep 2; kill "KILL", -$pid; waitpid $pid, 0; exit 124 };
alarm $s;
waitpid $pid, 0;
exit($? & 127 ? 128 + ($? & 127) : $? >> 8);
'
BOUND_GNU=0
if command -v timeout >/dev/null; then
	BOUND=(timeout -k 2) BOUND_GNU=1
elif command -v gtimeout >/dev/null; then
	BOUND=(gtimeout -k 2) BOUND_GNU=1
elif command -v perl >/dev/null; then
	BOUND=(perl -e "$BOUND_PERL")
else
	die 3 "no bounded runner (timeout, gtimeout or perl): refusing to make an unbounded tmux call"
fi
BOUND_S=${CRAZE_SMOKE_BOUND_S:-20}
[[ $BOUND_S =~ ^[1-9][0-9]*$ ]] || die 2 "CRAZE_SMOKE_BOUND_S must be a positive whole number: $BOUND_S"

# bounded <secs> <cmd…>: run cmd with that time limit; 124 when it hit it.
# GNU timeout -k exits 137 when the KILL after the grace ended the call: that
# is the bound too.
bounded() {
	local s=$1 rc
	shift
	"${BOUND[@]}" "$s" "$@"
	rc=$?
	((rc == 137 && BOUND_GNU)) && rc=124
	return "$rc"
}

# failed <rc> <message…>: exit 124 for a call that hit its bound, else 1.
failed() {
	local rc=$1
	shift
	((rc == 124)) && die 124 "$* (a tmux call hit its ${BOUND_S}s bound)"
	die 1 "$*"
}

if [[ $host == mac ]]; then
	[[ -x $MAC_TMUX ]] ||
		die 3 "--host mac needs $MAC_TMUX: see the mac-mini runbook ($MAC_DIR/README.md); if it is missing, ask the owner"
	T() { bounded "$BOUND_S" "$MAC_TMUX" "$@"; }
	paste_text() { bounded "$BOUND_S" "$MAC_TMUX" type "$name" "$1"; }
else
	command -v tmux >/dev/null || die 1 "tmux not found"
	sock=${CRAZE_SMOKE_SOCK:-craze-$name}
	T() { bounded "$BOUND_S" tmux -L "$sock" "$@"; }
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
		pane=$(T capture-pane -p -t "$name") || failed $? "capture-pane failed (is session $name running?)"
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
	T capture-pane "${flags[@]}" -t "$name" >"$file" || failed $? "capture-pane failed"
	echo "saved $file"
	;;
stop)
	(($# == 0)) || die 2 "stop takes no arguments"
	T send-keys -t "$name" Escape 2>/dev/null
	sleep 0.3
	T send-keys -t "$name" C-d 2>/dev/null
	sleep 0.5
	# End it (it may already be gone: C-d can end the last pane), then confirm
	# with a probe whose error establishes absence: no server on the socket
	# ("no server running", or the socket file missing), or no such session.
	# Anything else (a Permission denied, another connection error, a probe
	# that hit its bound) is a failure.
	if [[ $host == mac ]]; then
		# ssh to the mac-mini does not carry the remote exit status (it is 0
		# whatever tmux returned), so the mac probe is judged by what it
		# prints, and only an explicit absence counts: has-session on the
		# exact name ("=name", never a prefix such as the owner's "smoke")
		# says "can't find session" or "no server running" when it is gone,
		# and nothing when it is there. Silence, a truncated answer or any
		# other error is not proof of absence.
		T kill-session -t "=$name" 2>/dev/null
		krc=$?
		out=$(T has-session -t "=$name" 2>&1)
		prc=$?
		((prc == 124)) && failed 124 "cannot confirm session $name is gone after kill-session (rc $krc): has-session"
		case $out in
		*"can't find session"* | *"no server running"*) ;;
		*) die 1 "session $name is still running after kill-session, or its absence cannot be confirmed${out:+: $out}" ;;
		esac
		echo "stopped $name"
		exit 0
	else
		what="tmux server $sock" killcmd=kill-server probe=(list-sessions)
		T kill-server 2>/dev/null
	fi
	krc=$?
	err=$(T "${probe[@]}" 2>&1 >/dev/null)
	prc=$?
	((prc == 0)) && die 1 "$what is still running after $killcmd (rc $krc)"
	# A probe that hit its bound proves nothing, whatever it printed first.
	((prc == 124)) && failed 124 "cannot confirm $what is gone after $killcmd (rc $krc): ${probe[0]}"
	case $err in
	*"no server running"* | *"can't find session"* | *"error connecting to "*"(No such file or directory)"*) ;;
	*) failed "$prc" "cannot confirm $what is gone after $killcmd (rc $krc): ${probe[0]} exited $prc${err:+: $err}" ;;
	esac
	echo "stopped $name"
	;;
*) die 2 "unknown command: $cmd (start, type, key, wait, snap, stop)" ;;
esac
