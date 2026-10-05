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
  a failed export (2); a failing test (1); a malformed or too-long label (2,
  nothing written); an output or export dir that reaches the repository
  through `..` or a symlink (2, nothing written); two concurrent runs with
  one label (two unique export dirs); starve.sh and contend.sh without
  systemd-run/taskset (3); a failed build (2: cpu1, v2, and starve/contend
  where the tools exist); a panic before any test in contend.sh (and
  starve.sh where a systemd user session works) (1); SIGINT to a running
  script (130) and SIGTERM to one launched with a plain & (143): its child
  process group gone, its export dir removed, the summary line printed; a v1
  tui job with a shortened watchdog (INCOMPLETE, its group killed, the tui
  lock released) and v1 --summary; two v1 tui jobs with different labels
  (the second refused, 4); a SIGKILLed v1 script whose tui job lives on (a
  second tui job refused until the watchdog ends the group); v1 results bound
  to their manifest (a re-plan makes them STALE; --job --sha off the
  manifest refused); gate.sh's steps on a scratch Makefile (both lint passes
  through the wrapper, a failing step stops it); v8.sh --selftest; and the
  craze-live-smoke skill's smoke.sh start/type/wait/key/snap/stop on a
  dedicated tmux server running cat (also through the perl runner), its
  refusal without a bounded runner (3), a hung tmux bounded (124, its process
  group gone) through timeout and perl, a stop that cannot confirm the end
  (1), a stop of an absent server (0), and its --host mac refusal (3) when
  the mac-mini runbook's scripts are missing.
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
BG=()  # process groups this selftest started
BGP=() # plain pids it started (a `&` launch with job control off)
STUB_PIDS=$S/stub.pids
cv_cleanup_hook() {
	local p
	for p in "${BGP[@]+"${BGP[@]}"}"; do kill -TERM "$p" 2>/dev/null; done
	for p in "${BG[@]+"${BG[@]}"}"; do cv_stop_group "$p"; done
	if [[ -f $STUB_PIDS ]]; then
		while read -r p; do kill -KILL "$p" 2>/dev/null; done <"$STUB_PIDS"
	fi
	if command -v tmux >/dev/null; then
		tmux -L "$SOCK" kill-server 2>/dev/null
		tmux -L "$SOCK-p" kill-server 2>/dev/null
	fi
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
w internal/boom/boom_test.go $'package boom\n\nimport "testing"\n\nfunc init() { panic("deliberate init panic") }\n\nfunc TestBoom(t *testing.T) {}'
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
		kill -KILL "$1" 2>/dev/null
		wait "$1" 2>/dev/null
		RC=124
		return
	fi
	wait "$1"
	RC=$?
	BG=() BGP=()
}
# farm <dir> <name…>: a PATH directory of links to every command in the usual
# bin directories (and tmux's) except the names given.
farm() {
	local dir=$1 d f b x skip tdir=''
	shift
	mkdir -p "$dir"
	command -v tmux >/dev/null && tdir=$(dirname "$(command -v tmux)")
	for d in /usr/local/bin /usr/bin /bin /usr/sbin /sbin ${tdir:+"$tdir"}; do
		[[ -d $d ]] || continue
		for f in "$d"/*; do
			b=${f##*/}
			[[ -e $dir/$b ]] && continue
			skip=0
			for x in "$@"; do [[ $b == "$x" ]] && skip=1; done
			((skip)) || ln -s "$f" "$dir/$b" 2>/dev/null
		done
	done
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
[[ -e $S/o/$(printf 'b%.0s' {1..30}) ]] && bad "export path over 40 characters" "refused only after creating its output dir"

# 5b. The never-under-repo guard resolves paths physically (`..`, a symlink
# into the repo), and refuses before anything is written.
ln -s "$R" "$S/lnk"
expect "output dir through ..: refused" CPU1 2 env CRAZE_VERIFY_OUT="$S/o/../r/vo" "$V/cpu1.sh" pg good ./internal/ok
has "$OUTF" 'is inside the repository' && [[ ! -e $R/vo ]] || bad "output dir through .." "no refusal message, or $R/vo was written"
expect "output dir through a symlink: refused" CPU1 2 env CRAZE_VERIFY_OUT="$S/lnk/vo" "$V/cpu1.sh" pg good ./internal/ok
has "$OUTF" 'is inside the repository' && [[ ! -e $R/vo ]] || bad "output dir through a symlink" "no refusal message, or $R/vo was written"
expect "export dir through a symlink: refused" CPU1 2 env CRAZE_VERIFY_TMP="$S/lnk" "$V/cpu1.sh" pg good ./internal/ok
if has "$OUTF" 'is inside the repository' && [[ ! -e $S/o/pg ]] && ! compgen -G "$R/cv-*" >/dev/null; then :; else
	bad "export dir through a symlink" "no refusal message, or something was written"
fi
[[ -z $(git -C "$R" status --porcelain 2>&1) ]] || bad "never under the repo" "the scratch repo is dirty: $(git -C "$R" status --porcelain 2>&1 | head -3)"

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

# 9b. SIGTERM to a script launched with a plain & (job control off, so it
# inherits SIGINT ignored: SIGTERM is the documented stop signal).
f=$S/sigterm.out
CVSELF_SLEEP_S=300 "$V/cpu1.sh" sigt good ./internal/sleep >"$f" 2>&1 </dev/null &
p=$!
BGP+=("$p")
if wait_for "$f" '^CPU1 running pgid=' 120; then
	g=$(sed -n 's/^CPU1 running pgid=\([0-9]*\).*/\1/p' "$f")
	d=$(sed -n 's/^CPU1 export=\([^ ]*\).*/\1/p' "$f")
	for ((i = 0; i < 240; i++)); do pgrep -g "$g" -f 'sleep\.test' >/dev/null && break; sleep 0.25; done
	kill -TERM "$p"
	finish_bg "$p" 30
	cat "$f" >>"$CV_LOG"
	if [[ $RC == 143 ]] && summary_ok "$f" CPU1 143 && group_gone "$g" && [[ -n $d && ! -e $d ]]; then
		ok "SIGTERM to a plain & launch: exit 143, group $g gone, $d removed, summary printed"
	else
		bad "SIGTERM (plain &)" "rc $RC, group $g $(group_gone "$g" && echo gone || echo ALIVE), dir '$d' $([[ -e $d ]] && echo LEFT || echo removed), last: $(tail -1 "$f")"
	fi
else
	bad "SIGTERM (plain &)" "the script never started its child: $(tail -3 "$f")"
	kill -TERM "$p" 2>/dev/null
fi

# 9c. A panic before any test (in init) is a test failure (1), not a harness one.
if command -v taskset >/dev/null; then
	expect "contend: a panic before any test" CONTEND 1 "$V/contend.sh" cp good internal/boom 0 2 .
	has "$OUTF" '^panic: deliberate init panic' && has "$OUTF" ' crashed=2 ' ||
		bad "contend: a panic before any test" "no panic line or crashed=2"
fi
if command -v systemd-run >/dev/null && systemd-run --user --scope --quiet true >/dev/null 2>&1; then
	expect "starve: a panic before any test" STARVE 1 "$V/starve.sh" sp good internal/boom 50 .
	has "$OUTF" '^panic: deliberate init panic' || bad "starve: a panic before any test" "no panic line"
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
has "$OUTF" '^V1_TOTAL jobs=1 ok=0 fail=0 data_race=0 incomplete=1 not_run=0 stale=0$' || bad "v1 --summary" "wrong V1_TOTAL"

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

# 11b. v1: SIGKILL the script (not its group) during a tui job. The job's own
# processes hold the lock, so a second tui job is refused until the watchdog
# (30 s from the script's start) has ended the group; then the lock is free.
expect "v1 --plan (label lk)" V1 0 env XDG_CACHE_HOME="$XC" "$V/v1.sh" lk --plan --sha good --pkgs ./internal/tui --count 5
f=$S/v1-lk.out
XDG_CACHE_HOME=$XC CRAZE_VERIFY_V1_WATCHDOG_S=30 start_bg "$f" "$V/v1.sh" lk --job tui-s1
p=$BGPID
if wait_for "$f" '^V1 watchdog pgid=' 120; then
	g=$(sed -n 's/^V1 running pgid=\([0-9]*\).*/\1/p' "$f")
	wd=$(sed -n 's/^V1 watchdog pgid=\([0-9]*\).*/\1/p' "$f")
	BG+=("$g" "$wd")
	for ((i = 0; i < 240; i++)); do pgrep -g "$g" -f 'tui\.test' >/dev/null && break; sleep 0.25; done
	kill -KILL "$p"
	wait "$p" 2>/dev/null
	expect "v1: SIGKILLed script, job alive: a second tui job is refused" V1 4 env XDG_CACHE_HOME="$XC" "$V/v1.sh" lb --job tui-s1
	then_alive=$(group_gone "$g" && echo gone || echo alive)
	for ((i = 0; i < 240; i++)); do group_gone "$g" && break; sleep 0.25; done
	if [[ $then_alive == alive ]] && group_gone "$g" && flock -n "$LOCK" true; then
		ok "v1: SIGKILLed script: refused while group $g lived, lock free once the watchdog ended it"
	else
		bad "v1 SIGKILL" "group $g was $then_alive at the refusal, now $(group_gone "$g" && echo gone || echo ALIVE); lock $(flock -n "$LOCK" true && echo free || echo HELD)"
	fi
	for ((i = 0; i < 80; i++)); do group_gone "$wd" && break; sleep 0.25; done
	group_gone "$wd" || bad "v1 SIGKILL" "the watchdog group $wd did not end"
	cat "$f" >>"$CV_LOG"
	BG=()
else
	bad "v1 SIGKILL" "the tui job never started: $(tail -3 "$f")"
	cv_stop_group "$p"
fi

# 11c. v1: results are bound to their manifest.
expect "v1 --plan (label stl)" V1 0 env XDG_CACHE_HOME="$XC" "$V/v1.sh" stl --plan --sha good --pkgs ./internal/ok --count 1
expect "v1 --job ok (label stl)" V1 0 env XDG_CACHE_HOME="$XC" "$V/v1.sh" stl --job ok
expect "v1 --summary (current result)" V1 0 env XDG_CACHE_HOME="$XC" "$V/v1.sh" stl --summary
has "$OUTF" '^V1_TOTAL jobs=1 ok=1 fail=0 data_race=0 incomplete=0 not_run=0 stale=0$' || bad "v1 --summary (current result)" "wrong V1_TOTAL"
expect "v1 --job --sha off the manifest: refused" V1 2 env XDG_CACHE_HOME="$XC" "$V/v1.sh" stl --job ok --sha broken-export
has "$OUTF" "is not the manifest's sha" || bad "v1 --job --sha off the manifest" "no refusal message"
expect "v1 --plan again, identical (label stl)" V1 0 env XDG_CACHE_HOME="$XC" "$V/v1.sh" stl --plan --sha good --pkgs ./internal/ok --count 1
expect "v1 --summary after a re-plan: STALE" V1 2 env XDG_CACHE_HOME="$XC" "$V/v1.sh" stl --summary
has "$OUTF" '^ok +\./internal/ok +1 .* STALE ' && has "$OUTF" '^V1_TOTAL jobs=1 ok=0 fail=0 data_race=0 incomplete=0 not_run=0 stale=1$' ||
	bad "v1 --summary after a re-plan" "the old result was not reported STALE"

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
# smoke.sh's bounded runner. A PATH without timeout/gtimeout takes the perl
# fallback; one without perl as well is refused (3).
farm "$S/noto" timeout gtimeout
farm "$S/norun" timeout gtimeout perl
if command -v tmux >/dev/null && command -v perl >/dev/null; then
	run env PATH="$S/noto" CRAZE_SMOKE_SOCK="$SOCK-p" "$SMOKE" start sp cat
	r1=$RC
	run env PATH="$S/noto" CRAZE_SMOKE_SOCK="$SOCK-p" "$SMOKE" type sp "$text"
	r2=$RC
	run env PATH="$S/noto" CRAZE_SMOKE_SOCK="$SOCK-p" "$SMOKE" wait sp "$text" 10
	r3=$RC
	run env PATH="$S/noto" CRAZE_SMOKE_SOCK="$SOCK-p" "$SMOKE" stop sp
	r4=$RC
	if [[ "$r1$r2$r3$r4" == 0000 ]] && ! tmux -L "$SOCK-p" has-session 2>/dev/null; then
		ok "smoke.sh through the perl runner: start/type/wait/stop"
	else
		bad "smoke.sh (perl runner)" "rcs $r1 $r2 $r3 $r4"
	fi
fi
run env PATH="$S/norun" "$SMOKE" start sn cat
if [[ $RC == 3 ]] && has "$OUTF" 'no bounded runner'; then ok "smoke.sh without a bounded runner: 3"; else bad "smoke.sh without a bounded runner" "exit $RC"; fi
# A hung tmux (a stand-in that never returns, nor does its child) is bounded:
# 124 within the bound plus the 2 s grace, its whole process group gone.
mkdir -p "$S/stub"
printf '%s\n' '#!/bin/sh' \
	'# A stand-in tmux: CVSTUB_MODE=hang never returns (nor does its child);' \
	'# alive claims every call worked (a server that will not die).' \
	'case ${CVSTUB_MODE:-} in' \
	'hang) echo $$ >>"$CVSTUB_PIDS"; sleep 1000 & echo $! >>"$CVSTUB_PIDS"; wait; exit 0 ;;' \
	'alive) exit 0 ;;' \
	'esac' \
	'exit 1' >"$S/stub/tmux"
chmod 755 "$S/stub/tmux"
for via in timeout perl; do
	if [[ $via == timeout ]]; then
		command -v timeout >/dev/null || command -v gtimeout >/dev/null || continue
		pth=$S/stub:$PATH
	else
		command -v perl >/dev/null || continue
		pth=$S/stub:$S/noto
	fi
	: >"$STUB_PIDS"
	t0=$(date +%s)
	run env PATH="$pth" CVSTUB_MODE=hang CVSTUB_PIDS="$STUB_PIDS" CRAZE_SMOKE_SOCK="$SOCK-h" CRAZE_SMOKE_BOUND_S=2 "$SMOKE" start sh1 cat
	el=$(($(date +%s) - t0))
	alive=''
	while read -r q; do kill -0 "$q" 2>/dev/null && alive+=" $q"; done <"$STUB_PIDS"
	if [[ $RC == 124 ]] && ((el <= 9)) && [[ -s $STUB_PIDS && -z $alive ]]; then
		ok "smoke.sh: a hung tmux is bounded through $via (124 in ${el}s, its group gone)"
	else
		bad "smoke.sh hang ($via)" "exit $RC in ${el}s, pids alive:${alive:- none}"
	fi
	while read -r q; do kill -KILL "$q" 2>/dev/null; done <"$STUB_PIDS"
done
# stop: a server that claims to die but stays is a failure (1); an absent one is success (0).
run env PATH="$S/stub:$PATH" CVSTUB_MODE=alive CRAZE_SMOKE_SOCK="$SOCK-a" "$SMOKE" stop sa
if [[ $RC == 1 ]] && has "$OUTF" 'still running after kill-server'; then ok "smoke.sh stop: a server that stays is reported (1)"; else bad "smoke.sh stop (server stays)" "exit $RC"; fi
if command -v tmux >/dev/null; then
	run env CRAZE_SMOKE_SOCK="$SOCK-gone" "$SMOKE" stop sg
	if [[ $RC == 0 ]] && has "$OUTF" '^stopped sg$'; then ok "smoke.sh stop: an absent server counts as stopped (0)"; else bad "smoke.sh stop (absent)" "exit $RC"; fi
fi
# --host mac without the mac-mini runbook's scripts (a scratch HOME: no ssh).
run env HOME="$S/nohome" "$SMOKE" --host mac start st cat
if [[ $RC == 3 ]] && has "$OUTF" 'see the mac-mini runbook'; then ok "smoke.sh --host mac without the runbook: 3"; else bad "smoke.sh --host mac" "exit $RC"; fi

echo "SELFTEST passed=$pass failed=$fail"
((fail == 0)) || exit 1
exit 0
