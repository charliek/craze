#!/usr/bin/env bash
# contend.sh <label> <sha> <pkg-dir> <cpus> <copies> <regex> [count] [race] —
# K copies of one test binary pinned to the same few cores, on a committed tree.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

usage() {
	cat <<EOF
usage: contend.sh <label> <sha> <pkg-dir> <cpus> <copies> <regex> [count] [race]

Builds <pkg-dir>'s test binary once from the COMMITTED tree at <sha> (with
-race when the last argument is "race") and runs <copies> copies of it at
once, each pinned with taskset -c <cpus> (e.g. 20-21 or 30,31), each running
the tests matching <regex> -test.count=<count> times (default 1), from the
package directory by absolute path. OS-level time-slicing preempts one thread
while the process's others run, which reproduced SF-118 (10 -race copies on 2
cores) and SF-135 (8 copies on 2 cores) where a CPU quota could not.
Linux only (exit 3 without taskset). CRAZE_GOLDEN_TRANSPORT defaults to both.

Prints CONTEND rcs=<each copy's exit> pass=<n> fail=<n> (top-level lines,
all copies) and the first failures. Logs: \$OUT/contend-<stamp>.<i>.log
$CV_HELP_COMMON
EOF
}

if cv_wants_help "$@"; then usage; exit 0; fi
cv_setup CONTEND
[[ $# -ge 6 && $# -le 8 ]] || { usage >&2; die 2 "want <label> <sha> <pkg-dir> <cpus> <copies> <regex> [count] [race]"; }
command -v taskset >/dev/null || die 3 "taskset not found: contend.sh needs Linux (util-linux)"
label=$1 rev=$2 pkg=${3#./} cpus=$4 copies=$5 re=$6 count=${7:-1} race=${8:-}
cv_label "$label"
[[ $cpus =~ ^[0-9]+([,-][0-9]+)*$ ]] || die 2 "cpus must be a taskset list such as 20-21 or 30,31: $cpus"
[[ $copies =~ ^[0-9]+$ && $copies -ge 1 ]] || die 2 "copies must be a positive integer: $copies"
[[ $count =~ ^[0-9]+$ && $count -ge 1 ]] || die 2 "count must be a positive integer: $count"
[[ -z $race || $race == race ]] || die 2 "the last argument must be 'race' or nothing: $race"
cv_resolve "$rev"
stamp=$(cv_stamp)
base=$OUT/contend-$stamp
CV_LOG=$base.log
: >"$CV_LOG"
export_tree "$CV_SHA_FULL"
tree=$CV_LAST_EXPORT
[[ -d $tree/$pkg ]] || die 2 "no package directory $pkg at $CV_SHA"

# The test binary lives in a directory of its own, outside the module tree: a
# test that walks the repository (TestRemovedConfigEnvAppearsNowhereElse) must
# not find it there.
cv_export_dir
bin=$CV_LAST_EXPORT/pkg.test
flags=()
[[ $race == race ]] && flags=(-race)
build() { cd "$tree" && go test "${flags[@]+"${flags[@]}"}" -c -o "$bin" "./$pkg"; }
cv_run "$CV_LOG" build || { tail -20 "$CV_LOG"; die 2 "build failed (see $CV_LOG)"; }
[[ -x $bin ]] || die 2 "no test files in $pkg"

# All copies run inside one process group (this function's), so a signal
# ends every one of them.
contend() {
	set +m
	cd "$tree/$pkg" || return 2
	local i pids=()
	for ((i = 1; i <= copies; i++)); do
		CRAZE_GOLDEN_TRANSPORT=${CRAZE_GOLDEN_TRANSPORT:-both} taskset -c "$cpus" "$bin" \
			"-test.run=$re" "-test.count=$count" -test.timeout=2h -test.v >"$base.$i.log" 2>&1 &
		pids+=($!)
	done
	for ((i = 1; i <= copies; i++)); do
		wait "${pids[i - 1]}"
		echo "$?" >"$base.$i.rc"
	done
}
s0=$(date +%s)
cv_run "$CV_LOG" contend
rcs=() pass=0 fail=0 bad=0
for ((i = 1; i <= copies; i++)); do
	r=$(cat "$base.$i.rc" 2>/dev/null || echo '?')
	rcs+=("$r")
	[[ $r == 0 ]] || bad=1
	n=$(grep -c '^--- PASS' "$base.$i.log" 2>/dev/null)
	pass=$((pass + ${n:-0}))
	n=$(grep -c '^--- FAIL' "$base.$i.log" 2>/dev/null)
	fail=$((fail + ${n:-0}))
done
echo "CONTEND label=$CV_LABEL sha=$CV_SHA pkg=$pkg cpus=$cpus copies=$copies count=$count race=${race:-no} rcs=${rcs[*]} pass=$pass fail=$fail elapsed=$(($(date +%s) - s0))s logs=$base.<i>.log"
grep -hE '^ *--- FAIL|^ +[^ ]+\.go:[0-9]+: |^panic:' "$base".*.log 2>/dev/null | head -20
if ((bad == 0)); then exit 0; fi
if ((pass == 0 && fail == 0)); then die 2 "no copy produced a result (see $base.1.log)"; fi
exit 1
