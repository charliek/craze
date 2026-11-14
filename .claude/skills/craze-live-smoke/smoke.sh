#!/usr/bin/env bash
# smoke.sh — drive a TUI in a dedicated tmux server for a live smoke.
#
#   smoke.sh [--host local|mac] start <name> [-c <dir>] <cmd…>
#                                                     a detached session at 120x36
#   smoke.sh [--host …] type <name> <text>            bracketed paste, no Enter
#   smoke.sh [--host …] key  <name> <keys…>           named keys: Enter Escape C-d BSpace …
#   smoke.sh [--host …] wait <name> <regex> <secs>    poll the pane until <regex> matches
#   smoke.sh [--host …] snap <name> <file> [--ansi]   capture the pane (--ansi keeps SGR codes)
#   smoke.sh [--host …] stop <name>                   Escape, C-d, end it, confirm it is gone
#
# start's -c <dir> (or --cwd <dir>) is the pane's working directory, which is
# the workspace a craze started without --workspace works in; without it the
# pane starts in the caller's own directory. A local <dir> must exist.
#
# local (the default): one tmux server per smoke, `tmux -L craze-<name>`
# (CRAZE_SMOKE_SOCK overrides the socket name; smokes sharing one share a
# server, and stopping any of them ends them all), started with -f /dev/null
# so no user config applies, and under a UTF-8 locale (LC_ALL: C.UTF-8 on
# Linux, en_US.UTF-8 on macOS, from `locale -a`), which the server and every
# pane inherit: under C, tmux draws non-ASCII as `_`. `stop` kills that whole
# server, confirms it is gone and removes a socket file it left behind.
# Pane size: CRAZE_SMOKE_COLS x CRAZE_SMOKE_ROWS (default 120x36).
# mac: the mac-mini's smoke server through mac-tmux.sh in the owner's
# ~/.claude/plans/craze/mac-mini (CRAZE_SMOKE_MAC_DIR overrides the folder;
# the owner's home comes from the password database, not $HOME, so an
# exported scratch HOME does not move it) (`tmux -L smoke`, the only server
# whose panes reach the login keychain); a session of your own in it, and
# `stop` kills only that session, never the server, and confirms the session
# is gone.
#
# Every tmux call, local or remote, is bounded (the privacy-prompt trap) at
# CRAZE_SMOKE_BOUND_S seconds (default 20) by timeout(1), else gtimeout
# (Homebrew coreutils), else a perl fork + alarm that TERMs, then KILLs, the
# call's process group; with none of the three, smoke.sh refuses (exit 3)
# rather than make an unbounded call.
#
# `wait` polls every 0.25 s for at least <secs> seconds (bash 5 times it to
# the microsecond; an older bash rounds the ceiling up to a whole second).
#
# `type` is a bracketed paste through the buffer `smoke`: one paste message,
# the default for text. `key <name> -l 'text'` types it as keys instead (a
# burst or typing check; craze splits a burst into its keys, SF-40), and
# `key <name> -N 80 BSpace` clears a draft. Exit: 0 ok, 1 a wait timed out,
# tmux failed or `stop` could not confirm the end, 2 usage (no arguments
# included), 3 no bounded runner or the mac-mini runbook's scripts are
# missing, 124 a call hit its bound (every command, `wait`, `snap` and `stop`
# included).
set -u -o pipefail

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
'') usage >&2; exit 2 ;;
-h | --help) usage; exit 0 ;;
esac
[[ $host == local || $host == mac ]] || die 2 "--host must be local or mac, not '$host'"
cmd=$1
name=${2:-}
[[ -n $name ]] || die 2 "$cmd needs a session name"
[[ $name =~ ^[A-Za-z0-9_-]+$ ]] || die 2 "a session name is letters, digits, _ and - only: $name"
shift 2
# Every pane target names the session exactly ("=name:"), never by prefix:
# on the mac-mini an absent name must not reach the owner's own session.
tgt="=$name:"

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

# owner_home is the invoking user's home from the password database: a smoke
# often exports a scratch HOME, which must not move the mac-mini runbook.
owner_home() {
	local h=
	command -v getent >/dev/null && h=$(getent passwd "$(id -u)" | cut -d: -f6)
	[[ -n $h ]] || h=$(perl -e 'print((getpwuid($<))[7] // "")' 2>/dev/null)
	printf '%s' "${h:-$HOME}"
}

# utf8_locale is the UTF-8 locale `locale -a` lists, preferring C.UTF-8 on
# Linux and en_US.UTF-8 on macOS, else any UTF-8 one; empty when none is.
utf8_locale() {
	local all first second l
	all=$(locale -a 2>/dev/null) || all=
	if [[ $(uname -s) == Darwin ]]; then first=en_US second=C; else first=C second=en_US; fi
	for l in "$first" "$second"; do
		l=$(grep -Eix -m1 "$l\.utf-?8" <<<"$all") && { printf '%s' "$l"; return; }
	done
	grep -Ei -m1 '\.utf-?8$' <<<"$all"
}

if [[ $host == mac ]]; then
	MAC_DIR=${CRAZE_SMOKE_MAC_DIR:-$(owner_home)/.claude/plans/craze/mac-mini}
	MAC_TMUX=$MAC_DIR/mac-tmux.sh
	[[ -x $MAC_TMUX ]] ||
		die 3 "--host mac needs $MAC_TMUX: see the mac-mini runbook ($MAC_DIR/README.md); if it is missing, ask the owner"
	T() { bounded "$BOUND_S" "$MAC_TMUX" "$@"; }
	paste_text() { bounded "$BOUND_S" "$MAC_TMUX" type "$tgt" "$1"; }
else
	command -v tmux >/dev/null || die 1 "tmux not found"
	sock=${CRAZE_SMOKE_SOCK:-craze-$name}
	# The server takes its locale from the client that starts it, and the
	# panes from the server: a UTF-8 one for both (see the header).
	utf8=$(utf8_locale)
	if [[ -n $utf8 ]]; then
		export LC_ALL=$utf8
	elif [[ $cmd == start ]]; then
		echo "smoke.sh: no UTF-8 locale in \`locale -a\`: tmux may draw non-ASCII as _" >&2
	fi
	T() { bounded "$BOUND_S" tmux -L "$sock" "$@"; }
	paste_text() { printf '%s' "$1" | T load-buffer -b smoke - && T paste-buffer -p -d -b smoke -t "$tgt"; }
fi

case $cmd in
start)
	cwd=()
	if [[ ${1:-} == -c || ${1:-} == --cwd ]]; then
		(($# >= 2)) || die 2 "$1 needs a directory"
		dir=$2
		shift 2
		if [[ $host == local ]]; then
			[[ -d $dir ]] || die 2 "start's working directory is not a directory: $dir"
			dir=$(cd -- "$dir" && pwd -P) || die 2 "cannot resolve the working directory: $dir"
		fi
		cwd=(-c "$dir")
	fi
	(($# >= 1)) || die 2 "start needs a command"
	if [[ $host == mac ]]; then
		T new-session -d -s "$name" -x "${CRAZE_SMOKE_COLS:-120}" -y "${CRAZE_SMOKE_ROWS:-36}" ${cwd[@]+"${cwd[@]}"} "$@"
	else
		T -f /dev/null new-session -d -s "$name" -x "${CRAZE_SMOKE_COLS:-120}" -y "${CRAZE_SMOKE_ROWS:-36}" ${cwd[@]+"${cwd[@]}"} "$@"
	fi
	;;
type)
	(($# == 1)) || die 2 "type needs exactly one text argument"
	paste_text "$1"
	;;
key)
	(($# >= 1)) || die 2 "key needs at least one key"
	T send-keys -t "$tgt" "$@"
	;;
wait)
	(($# == 2)) || die 2 "wait needs <regex> <secs>"
	re=$1 secs=$2
	[[ $secs =~ ^[0-9]+$ ]] || die 2 "secs must be a whole number: $secs"
	# Capture into a file, never a pipe: a tmux client hands its stdout to
	# the server, so a hung server keeps a $(...) pipe open after the bound
	# has killed the client, and the read would block for ever.
	cap=$(mktemp "${TMPDIR:-/tmp}/smoke-wait.XXXXXX") || die 1 "mktemp failed"
	trap 'rm -f -- "$cap"' EXIT
	# now_us: microseconds from bash 5's EPOCHREALTIME (its six decimals,
	# whatever the decimal point); an older bash has whole seconds, so its
	# ceiling is rounded up a second rather than cut short.
	if [[ -n ${EPOCHREALTIME:-} ]]; then
		now_us() { echo "${EPOCHREALTIME//[!0-9]/}"; }
		end=$(($(now_us) + secs * 1000000))
	else
		now_us() { echo $(($(date +%s) * 1000000)); }
		end=$(($(now_us) + (secs + 1) * 1000000))
	fi
	while :; do
		T capture-pane -p -t "$tgt" >"$cap" || failed $? "capture-pane failed (is session $name running?)"
		pane=$(<"$cap")
		if grep -Eq -- "$re" <<<"$pane"; then
			echo "matched: $re"
			exit 0
		fi
		(($(now_us) >= end)) && break
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
	# Into <file>.part, moved into place only on success, so a failed
	# capture never truncates an earlier snap.
	T capture-pane "${flags[@]}" -t "$tgt" >"$file.part"
	rc=$?
	if ((rc != 0)); then
		rm -f -- "$file.part"
		failed "$rc" "capture-pane failed"
	fi
	mv -f -- "$file.part" "$file" || die 1 "cannot move $file.part into place"
	echo "saved $file"
	;;
stop)
	(($# == 0)) || die 2 "stop takes no arguments"
	T send-keys -t "$tgt" Escape 2>/dev/null
	sleep 0.3
	T send-keys -t "$tgt" C-d 2>/dev/null
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
		if ! grep -qxF "can't find session: $name" <<<"$out" && ! grep -q '^no server running' <<<"$out"; then
			die 1 "session $name is still running after kill-session, or its absence cannot be confirmed${out:+: $out}"
		fi
		echo "stopped $name"
		exit 0
	else
		what="tmux server $sock" killcmd=kill-server probe=(list-sessions)
		T kill-server 2>/dev/null
	fi
	krc=$?
	# A probe that reaches the server while kill-server is still tearing it
	# down says "server exited unexpectedly": ask again, for up to ~3 s.
	for _ in {1..15}; do
		err=$(T "${probe[@]}" 2>&1 >/dev/null)
		prc=$?
		if ((prc == 0)) || [[ $err != *"server exited unexpectedly"* ]]; then break; fi
		sleep 0.2
	done
	((prc == 0)) && die 1 "$what is still running after $killcmd (rc $krc)"
	# A probe that hit its bound proves nothing, whatever it printed first.
	((prc == 124)) && failed 124 "cannot confirm $what is gone after $killcmd (rc $krc): ${probe[0]}"
	case $err in
	*"no server running"* | *"can't find session"* | *"error connecting to "*"(No such file or directory)"*) ;;
	*) failed "$prc" "cannot confirm $what is gone after $killcmd (rc $krc): ${probe[0]} exited $prc${err:+: $err}" ;;
	esac
	# A refused connection ("no server running on <path>") can leave the
	# dead server's socket file behind: remove it, and only it. Another
	# start may bind a new server there at any moment (it unlinks the old
	# file and binds a new one), so the file is removed only when a second
	# probe is refused too and the inode read before that probe is still
	# the one there after it.
	# Accepted residual: a new server that binds between the last inode read
	#   and the rm (or binds before the first read and does not listen until
	#   after the second probe) loses its socket; it is microseconds wide.
	if [[ $err == *"no server running on "* ]]; then
		path=${err##*no server running on }
		path=${path%%$'\n'*}
		if [[ ${path##*/} == "$sock" && -S $path ]]; then
			read -r ino _ < <(ls -di -- "$path" 2>/dev/null) || ino=
			again=$(T "${probe[@]}" 2>&1 >/dev/null)
			arc=$?
			read -r ino2 _ < <(ls -di -- "$path" 2>/dev/null) || ino2=
			if ((arc != 0 && arc != 124)) && [[ $again == *"no server running on "* && -n $ino && $ino == "$ino2" ]]; then
				rm -f -- "$path"
			fi
		fi
	fi
	echo "stopped $name"
	;;
*) die 2 "unknown command: $cmd (start, type, key, wait, snap, stop)" ;;
esac
