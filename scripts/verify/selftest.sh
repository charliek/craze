#!/usr/bin/env bash
# selftest.sh — exercises scripts/verify's own failure paths in a scratch module.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

usage() {
	cat <<EOF
usage: selftest.sh

Checks that each of scripts/verify's failure paths ends with the summary line
and the right exit code, against a throwaway scratch module (never the real
repo's tests), in a few minutes:
  --help on every script; an invalid sha (2) on every script that takes one;
  a failed export (2); a failing test (1); a malformed or too-long label (2);
  two concurrent runs with one label (two unique export dirs); starve.sh and
  contend.sh without systemd-run/taskset (3); a failed build (2: cpu1, v2,
  and starve/contend where the tools exist); SIGINT to a running script (130:
  its child process group gone, its export dir removed, the summary line
  printed); a v1 tui job with a shortened watchdog (INCOMPLETE, its group
  killed, the tui lock released) and v1 --summary; two v1 tui jobs with
  different labels (the second refused, 4); gate.sh's steps on a scratch
  Makefile (both lint passes through the wrapper, a failing step stops it);
  v8.sh --selftest; and the craze-live-smoke skill's smoke.sh
  start/type/wait/key/snap/stop on a dedicated tmux server running cat, and
  its --host mac refusal (3) when the mac-mini runbook's scripts are missing.
Every case's output is appended to the log.

Log: \$OUT/selftest-<stamp>.log (label "selftest").
$CV_HELP_COMMON
EOF
}

if cv_wants_help "$@"; then usage; exit 0; fi
cv_setup SELFTEST
(($# == 0)) || { usage >&2; die 2 "selftest.sh takes no arguments"; }
cv_label selftest
CV_LOG=$OUT/selftest-$(cv_stamp).log
: >"$CV_LOG"
REAL_ROOT=$ROOT
SMOKE=$REAL_ROOT/.claude/skills/craze-live-smoke/smoke.sh
V=$CV_DIR

S=$(mktemp -d "${CRAZE_VERIFY_TMP:-/tmp}/cv-st.XXXX") || die 2 "mktemp failed"
CV_EXPORTS+=("$S")
((${#S} <= 22)) || die 2 "CRAZE_VERIFY_TMP is too long for the selftest's nested exports: $S"
R=$S/r
mkdir -p "$R" "$S/o" "$S/x" "$S/t" || die 2 "cannot create $S"
SOCK=cv-st-$$
BG=()
cv_cleanup_hook() {
	local p
	for p in "${BG[@]+"${BG[@]}"}"; do cv_stop_group "$p"; done
	if command -v tmux >/dev/null; then tmux -L "$SOCK" kill-server 2>/dev/null; fi
	return 0
}

# Every child script runs against the scratch repo, with scratch output,
# exports and (for v1's lock) cache; the Go build cache stays the real one.
GOCACHE=$(cd "$REAL_ROOT" && go env GOCACHE) || die 2 "go env GOCACHE failed"
export GOCACHE
export CRAZE_VERIFY_REPO=$R CRAZE_VERIFY_OUT=$S/o CRAZE_VERIFY_TMP=$S/t
XC=$S/x
LOCK=$XC/craze-verify/locks/v1-tui.lock

# ---- the scratch module -----------------------------------------------------
w() { mkdir -p "$(dirname "$R/$1")" && printf '%s\n' "$2" >"$R/$1"; }
[[ -f $REAL_ROOT/.mise.toml ]] && cp "$REAL_ROOT/.mise.toml" "$R/.mise.toml"
w go.mod $'module example.com/cvself\n\ngo 1.22'
w internal/ok/ok.go $'package ok\n\n// OK is what the scratch gate builds and lints.\nfunc OK() bool { return true }'
w internal/ok/ok_test.go $'package ok\n\nimport "testing"\n\nfunc TestOK(t *testing.T) {\n\tif !OK() {\n\t\tt.Fatal("not ok")\n\t}\n}'
w internal/fail/fail_test.go $'package fail\n\nimport "testing"\n\nfunc TestFail(t *testing.T) { t.Fatal("deliberate failure") }'
w internal/broken/broken.go $'package broken\n\nfunc F() int { return "not an int" }'
w internal/broken/broken_test.go $'package broken\n\nimport "testing"\n\nfunc TestF(t *testing.T) { F() }'
w internal/sleep/sleep_test.go $'package sleep\n\nimport (\n\t"os"\n\t"strconv"\n\t"testing"\n\t"time"\n)\n\nfunc TestSleep(t *testing.T) {\n\tn, _ := strconv.Atoi(os.Getenv("CVSELF_SLEEP_S"))\n\ttime.Sleep(time.Duration(n) * time.Second)\n}'
w internal/tui/tui_test.go $'package tui\n\nimport (\n\t"testing"\n\t"time"\n)\n\nfunc TestSlow(t *testing.T) { time.Sleep(300 * time.Second) }'
printf '%s\n' \
	'export PATH := $(HOME)/.local/share/mise/shims:$(PATH)' \
	'CVSELF_TEST_PKG ?= ./internal/ok' \
	'lint:' $'\tgolangci-lint run ./internal/ok' $'\tGOOS=darwin golangci-lint run ./internal/ok' \
	'test:' $'\tgo test -count=1 $(CVSELF_TEST_PKG)' \
	'test-race:' $'\tgo test -count=1 -race ./internal/ok' \
	'build:' $'\tgo build ./internal/ok' \
	'test-cli:' $'\t@echo no cli suite here' >"$R/Makefile"
G() { git -C "$R" -c user.name=selftest -c user.email=selftest@example.invalid -c commit.gpgsign=false "$@" >/dev/null 2>&1; }
G init -q && G add -A && G commit -q -m good && G tag good || die 2 "cannot build the scratch repo"
w internal/ok/blob.txt "a blob this test deletes: $$ $(date +%s%N)"
G add -A && G commit -q -m broken-export && G tag broken-export || die 2 "cannot build the scratch repo"
blob=$(git -C "$R" rev-parse broken-export:internal/ok/blob.txt) &&
	rm -f "$R/.git/objects/${blob:0:2}/${blob:2}" || die 2 "cannot remove the blob"
BAD=0000000000000000000000000000000000000bad

# ---- checks ------------------------------------------------------------------
pass=0 fail=0 n=0
ok() { pass=$((pass + 1)); echo "SELFTEST ok   $1"; }
bad() {
	fail=$((fail + 1))
	echo "SELFTEST FAIL $1: $2"
	echo "SELFTEST FAIL $1: $2" >>"$CV_LOG"
}
# summary_ok <file> <NAME> <rc>: the last line is that script's summary with that rc.
summary_ok() {
	tail -1 "$1" | grep -qE "^$2_EXIT=$3 elapsed=[0-9]+s sha=[^ ]+ log=[^ ]+\$"
}
# run <file-var> <cmd…>: run cmd in the foreground, output to a fresh file; sets RC and OUTF.
run() {
	n=$((n + 1))
	OUTF=$S/case-$n.out
	"$@" >"$OUTF" 2>&1
	RC=$?
	{
		echo "### case $n: $*"
		cat "$OUTF"
		echo "### rc=$RC"
	} >>"$CV_LOG"
}
# expect <case> <NAME> <rc> <cmd…>: run, then check the rc and the summary line.
expect() {
	local c=$1 name=$2 want=$3
	shift 3
	run "$@"
	if [[ $RC != "$want" ]]; then
		bad "$c" "exit $RC, want $want ($(tail -1 "$OUTF"))"
	elif ! summary_ok "$OUTF" "$name" "$want"; then
		bad "$c" "the last line is not the summary: $(tail -1 "$OUTF")"
	else
		ok "$c"
	fi
}
has() { grep -qE -- "$2" "$1"; }
group_gone() { ! kill -0 -- "-$1" 2>/dev/null; }
# wait_for <file> <ere> <secs>: until the file has a matching line.
wait_for() {
	local i
	for ((i = 0; i < $3 * 4; i++)); do
		grep -qE -- "$2" "$1" 2>/dev/null && return 0
		sleep 0.25
	done
	return 1
}
# start_bg <outfile> <cmd…>: start a script in its own process group (job
# control on, so SIGINT reaches it); its pid in BGPID.
start_bg() {
	local f=$1
	shift
	set -m
	"$@" >"$f" 2>&1 </dev/null &
	BGPID=$!
	set +m
	BG+=("$BGPID")
}
# finish_bg <pid> <secs>: wait for it with a ceiling; its rc in RC (124 on a hang).
finish_bg() {
	local i
	for ((i = 0; i < $2 * 4; i++)); do
		kill -0 "$1" 2>/dev/null || break
		sleep 0.25
	done
	if kill -0 "$1" 2>/dev/null; then
		cv_stop_group "$1"
		wait "$1" 2>/dev/null
		RC=124
		return
	fi
	wait "$1"
	RC=$?
	BG=()
}

echo "SELFTEST scratch=$S log=$CV_LOG"

# 1. --help on every script.
for s in common gate cpu1 starve contend v1 v2 v8 selftest; do
	run "$V/$s.sh" --help
	if [[ $RC == 0 ]] && has "$OUTF" '(usage|sourced by every)'; then ok "--help $s.sh"; else bad "--help $s.sh" "exit $RC"; fi
done
run "$SMOKE" --help
[[ $RC == 0 ]] && has "$OUTF" 'smoke.sh' && ok "--help smoke.sh" || bad "--help smoke.sh" "exit $RC"
run python3 "$REAL_ROOT/.claude/skills/craze-live-smoke/dump.py" --help
[[ $RC == 0 ]] && has "$OUTF" 'dump.py' && ok "--help dump.py" || bad "--help dump.py" "exit $RC"
run python3 "$V/run_v2.py" --help
[[ $RC == 0 ]] && has "$OUTF" 'usage' && ok "--help run_v2.py" || bad "--help run_v2.py" "exit $RC"

# 2. An invalid sha.
expect "invalid sha: gate" GATE 2 "$V/gate.sh" inv "$BAD"
expect "invalid sha: cpu1" CPU1 2 "$V/cpu1.sh" inv "$BAD" ./internal/ok
if command -v systemd-run >/dev/null; then expect "invalid sha: starve" STARVE 2 "$V/starve.sh" inv "$BAD" internal/ok 50 .; fi
if command -v taskset >/dev/null; then expect "invalid sha: contend" CONTEND 2 "$V/contend.sh" inv "$BAD" internal/ok 0 1 .; fi
expect "invalid sha: v1 --plan" V1 2 "$V/v1.sh" inv --plan --sha "$BAD" --pkgs ./internal/ok
expect "invalid sha: v2" V2 2 "$V/v2.sh" inv "$BAD"

# 3. A failed export (the commit exists; one of its blobs does not).
expect "failed export" CPU1 2 "$V/cpu1.sh" exp broken-export ./internal/ok
has "$OUTF" 'export failed' || bad "failed export" "no 'export failed' message"

# 4. A failing test.
expect "failing test" CPU1 1 "$V/cpu1.sh" ft good ./internal/fail
has "$OUTF" '^--- FAIL: TestFail' || bad "failing test" "no --- FAIL line printed"

# 5. Labels.
expect "malformed label" CPU1 2 "$V/cpu1.sh" 'bad label' good ./internal/ok
expect "41-character label" CPU1 2 "$V/cpu1.sh" "$(printf 'a%.0s' {1..41})" good ./internal/ok
expect "export path over 40 characters" CPU1 2 "$V/cpu1.sh" "$(printf 'b%.0s' {1..30})" good ./internal/ok
has "$OUTF" '40-character' || bad "export path over 40 characters" "no 40-character message"

# 6. Two concurrent runs with one label.
a=$S/conc-a.out b=$S/conc-b.out
CVSELF_SLEEP_S=3 "$V/cpu1.sh" same good ./internal/sleep >"$a" 2>&1 &
pa=$!
CVSELF_SLEEP_S=3 "$V/cpu1.sh" same good ./internal/sleep >"$b" 2>&1 &
pb=$!
wait "$pa"
ra=$?
wait "$pb"
rb=$?
cat "$a" "$b" >>"$CV_LOG"
da=$(sed -n 's/^CPU1 export=\([^ ]*\).*/\1/p' "$a")
db=$(sed -n 's/^CPU1 export=\([^ ]*\).*/\1/p' "$b")
if [[ $ra == 0 && $rb == 0 && -n $da && -n $db && $da != "$db" && ! -e $da && ! -e $db ]] &&
	summary_ok "$a" CPU1 0 && summary_ok "$b" CPU1 0; then
	ok "two concurrent runs, one label: $da and $db, both removed"
else
	bad "two concurrent runs, one label" "rc $ra/$rb dirs '$da' '$db'"
fi

# 7. starve/contend without systemd-run/taskset: a PATH that lacks both.
nb=$S/nobin
mkdir -p "$nb"
for d in /usr/local/bin /usr/bin /bin /usr/sbin /sbin; do
	[[ -d $d ]] || continue
	for f in "$d"/*; do
		b=${f##*/}
		[[ $b == systemd-run || $b == taskset || -e $nb/$b ]] && continue
		ln -s "$f" "$nb/$b" 2>/dev/null
	done
done
expect "starve without systemd-run" STARVE 3 env PATH="$nb" "$V/starve.sh" plat good internal/ok 50 .
expect "contend without taskset" CONTEND 3 env PATH="$nb" "$V/contend.sh" plat good internal/ok 0 1 .

# 8. A failed build.
expect "failed build: cpu1" CPU1 2 "$V/cpu1.sh" fb good ./internal/broken
expect "failed build: v2 (no cmd/craze)" V2 2 "$V/v2.sh" fb good --only sweep-echo
if command -v systemd-run >/dev/null; then expect "failed build: starve" STARVE 2 "$V/starve.sh" fb good internal/broken 50 .; fi
if command -v taskset >/dev/null; then expect "failed build: contend" CONTEND 2 "$V/contend.sh" fb good internal/broken 0 1 .; fi

# 9. SIGINT to a running script.
f=$S/sigint.out
CVSELF_SLEEP_S=300 start_bg "$f" "$V/cpu1.sh" sig good ./internal/sleep
p=$BGPID
if wait_for "$f" '^CPU1 running pgid=' 120; then
	g=$(sed -n 's/^CPU1 running pgid=\([0-9]*\).*/\1/p' "$f")
	d=$(sed -n 's/^CPU1 export=\([^ ]*\).*/\1/p' "$f")
	for ((i = 0; i < 240; i++)); do pgrep -g "$g" -f 'sleep\.test' >/dev/null && break; sleep 0.25; done
	kill -INT "$p"
	finish_bg "$p" 30
	cat "$f" >>"$CV_LOG"
	if [[ $RC == 130 ]] && summary_ok "$f" CPU1 130 && group_gone "$g" && [[ -n $d && ! -e $d ]]; then
		ok "SIGINT: exit 130, group $g gone, $d removed, summary printed"
	else
		bad "SIGINT" "rc $RC, group $g $(group_gone "$g" && echo gone || echo ALIVE), dir '$d' $([[ -e $d ]] && echo LEFT || echo removed), last: $(tail -1 "$f")"
	fi
else
	bad "SIGINT" "the script never started its child: $(tail -3 "$f")"
	cv_stop_group "$p"
fi

# 10. v1: a tui job past a shortened watchdog, then --summary.
expect "v1 --plan (watchdog label)" V1 0 env XDG_CACHE_HOME="$XC" "$V/v1.sh" wd --plan --sha good --pkgs ./internal/tui --count 5
has "$OUTF" '^tui-s1 +\./internal/tui +5 ' || bad "v1 --plan (watchdog label)" "no tui-s1 row of -count=5"
expect "v1 watchdog: INCOMPLETE" V1 2 env XDG_CACHE_HOME="$XC" CRAZE_VERIFY_V1_WATCHDOG_S=8 "$V/v1.sh" wd --job tui-s1
g=$(sed -n 's/^V1 running pgid=\([0-9]*\).*/\1/p' "$OUTF")
if has "$OUTF" 'status=INCOMPLETE' && [[ -n $g ]] && group_gone "$g" && has "$OUTF" 'V1 tui lock released' &&
	flock -n "$LOCK" true; then
	ok "v1 watchdog: group $g killed, tui lock released"
else
	bad "v1 watchdog" "status/group/lock wrong (group '$g')"
fi
expect "v1 --summary" V1 2 env XDG_CACHE_HOME="$XC" "$V/v1.sh" wd --summary
has "$OUTF" '^V1_TOTAL jobs=1 ok=0 fail=0 data_race=0 incomplete=1 not_run=0$' || bad "v1 --summary" "wrong V1_TOTAL"

# 11. v1: two tui jobs with different labels.
expect "v1 --plan (label la)" V1 0 env XDG_CACHE_HOME="$XC" "$V/v1.sh" la --plan --sha good --pkgs ./internal/tui --count 5
expect "v1 --plan (label lb)" V1 0 env XDG_CACHE_HOME="$XC" "$V/v1.sh" lb --plan --sha good --pkgs ./internal/tui --count 5
f=$S/v1-la.out
XDG_CACHE_HOME=$XC start_bg "$f" "$V/v1.sh" la --job tui-s1
p=$BGPID
if wait_for "$f" '^V1 running pgid=' 120; then
	g=$(sed -n 's/^V1 running pgid=\([0-9]*\).*/\1/p' "$f")
	expect "v1: a second tui job (another label) is refused" V1 4 env XDG_CACHE_HOME="$XC" "$V/v1.sh" lb --job tui-s1
	has "$OUTF" 'a tui job is running; launch this one after it' || bad "v1 tui lock refusal" "no refusal message"
	kill -INT "$p"
	finish_bg "$p" 30
	cat "$f" >>"$CV_LOG"
	if [[ $RC == 130 ]] && summary_ok "$f" V1 130 && group_gone "$g" && flock -n "$LOCK" true; then
		ok "v1: the first tui job ends on SIGINT, group $g gone, lock free"
	else
		bad "v1 tui job SIGINT" "rc $RC, last: $(tail -1 "$f")"
	fi
else
	bad "v1 tui lock" "the first tui job never started: $(tail -3 "$f")"
	cv_stop_group "$p"
fi

# 12. gate.sh's steps on the scratch Makefile.
expect "gate: green" GATE 0 "$V/gate.sh" gt good
if has "$OUTF" '^GATE_LINTWRAP runs=2 darwin=1$' &&
	[[ $(grep -cE '^GATE_STEP (lint|test|test-race|build|test-cli) rc=0 elapsed=[0-9]+$' "$OUTF") == 5 ]]; then
	ok "gate: five steps, both lint passes through the wrapper"
else
	bad "gate: steps" "missing GATE_STEP or GATE_LINTWRAP lines"
fi
expect "gate: a failing step stops it" GATE 1 env CVSELF_TEST_PKG=./internal/fail "$V/gate.sh" gt good
if has "$OUTF" '^GATE_STEP test rc=[1-9]' && ! has "$OUTF" '^GATE_STEP test-race'; then
	ok "gate: stops at the failing step"
else
	bad "gate: stop" "did not stop at test"
fi

# 13. v8.sh --selftest.
run "$V/v8.sh" --selftest
if [[ $RC == 0 ]] && has "$OUTF" '^V8_SELFTEST PASS$' && summary_ok "$OUTF" V8 0; then ok "v8.sh --selftest"; else bad "v8.sh --selftest" "exit $RC"; fi

# 14. The skill's smoke.sh on a dedicated tmux server running cat.
if command -v tmux >/dev/null; then
	export CRAZE_SMOKE_SOCK=$SOCK
	text='go up the tree and the end of it'
	run "$SMOKE" start st cat
	r1=$RC
	run "$SMOKE" type st "$text"
	r2=$RC
	run "$SMOKE" wait st "$text" 10
	r3=$RC
	run "$SMOKE" key st Enter
	r4=$RC
	run "$SMOKE" snap st "$S/snap.txt"
	r5=$RC
	run "$SMOKE" stop st
	r6=$RC
	if [[ "$r1$r2$r3$r4$r5$r6" == 000000 ]] && grep -qF "$text" "$S/snap.txt" &&
		! tmux -L "$SOCK" has-session 2>/dev/null; then
		ok "smoke.sh start/type/wait/key/snap/stop (every word survived; server gone)"
	else
		bad "smoke.sh" "rcs $r1 $r2 $r3 $r4 $r5 $r6"
	fi
	unset CRAZE_SMOKE_SOCK
else
	bad "smoke.sh" "tmux not found"
fi
# --host mac without the mac-mini runbook's scripts (a scratch HOME: no ssh).
run env HOME="$S/nohome" "$SMOKE" --host mac start st cat
if [[ $RC == 3 ]] && has "$OUTF" 'see the mac-mini runbook'; then ok "smoke.sh --host mac without the runbook: 3"; else bad "smoke.sh --host mac" "exit $RC"; fi

echo "SELFTEST passed=$pass failed=$fail"
((fail == 0)) || exit 1
exit 0
