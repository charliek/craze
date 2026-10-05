#!/usr/bin/env bash
# starve.sh <label> <sha> <pkg-dir> <quota%> <regex> [count] [race] — one
# package's tests under a CPU quota, on a committed tree.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

usage() {
	cat <<EOF
usage: starve.sh <label> <sha> <pkg-dir> <quota%> <regex> [count] [race]

Builds <pkg-dir>'s test binary from the COMMITTED tree at <sha> (with -race
when the last argument is "race") and runs the tests matching <regex>,
-test.count=<count> (default 1), from the package directory by absolute path
(a relative ./pkg.test breaks tests that re-run os.Args[0]; F-5), under
  systemd-run --user --scope -p CPUQuota=<quota>%
A quota freezes the whole process together: it reproduces starvation (5%
found the 028 frame flake and SF-133), not preemption between threads — for
that use contend.sh. Linux with a systemd user session only (exit 3 without
systemd-run). CRAZE_GOLDEN_TRANSPORT defaults to both, as in the gate.

Prints STARVE pass=<n> fail=<n> (top-level --- PASS/--- FAIL lines) and the
first failing lines. A binary that exits non-zero before any result (a panic
or an os.Exit in init or TestMain) is a test failure (1); exit 2 is kept for
the build, the export, or systemd-run's own failure ("Failed to …").
Log: \$OUT/starve-<stamp>.log
$CV_HELP_COMMON
EOF
}

if cv_wants_help "$@"; then usage; exit 0; fi
cv_setup STARVE
[[ $# -ge 5 && $# -le 7 ]] || { usage >&2; die 2 "want <label> <sha> <pkg-dir> <quota%> <regex> [count] [race]"; }
command -v systemd-run >/dev/null ||
	die 3 "systemd-run not found: starve.sh needs Linux with a systemd user session"
label=$1 rev=$2 pkg=${3#./} quota=${4%\%} re=$5 count=${6:-1} race=${7:-}
cv_label "$label"
[[ $quota =~ ^[0-9]+$ && $quota -ge 1 ]] || die 2 "quota must be a whole percentage: $4"
[[ $count =~ ^[0-9]+$ && $count -ge 1 ]] || die 2 "count must be a positive integer: $count"
[[ -z $race || $race == race ]] || die 2 "the last argument must be 'race' or nothing: $race"
cv_resolve "$rev"
stamp=$(cv_stamp)
CV_LOG=$OUT/starve-$stamp.log
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
blog=$OUT/starve-$stamp.build.log
cv_run "$blog" build || { tail -20 "$blog"; die 2 "build failed (see $blog)"; }
[[ -x $bin ]] || die 2 "no test files in $pkg"

starve() {
	cd "$tree/$pkg" &&
		CRAZE_GOLDEN_TRANSPORT=${CRAZE_GOLDEN_TRANSPORT:-both} systemd-run --user --scope --quiet -p CPUQuota="$quota%" \
			"$bin" "-test.run=$re" "-test.count=$count" -test.timeout=4h -test.v
}
s0=$(date +%s)
cv_run "$CV_LOG" starve
rc=$?
pass=$(grep -c '^--- PASS' "$CV_LOG")
fail=$(grep -c '^--- FAIL' "$CV_LOG")
echo "STARVE label=$CV_LABEL sha=$CV_SHA pkg=$pkg quota=$quota% count=$count race=${race:-no} exit=$rc pass=$pass fail=$fail elapsed=$(($(date +%s) - s0))s"
grep -E '^ *--- FAIL|^ +[^ ]+\.go:[0-9]+: |^panic:' "$CV_LOG" | head -20
if ((rc == 0)); then exit 0; fi
if ((fail == 0 && pass == 0)) && ! grep -qE '^(FAIL|panic:)' "$CV_LOG"; then
	tail -5 "$CV_LOG"
	if grep -qE '^(Failed to |systemd-run: )' "$CV_LOG"; then
		die 2 "systemd-run failed; the test binary never ran"
	fi
	echo "starve.sh: the test binary exited $rc before any test result (init or TestMain)" >&2
fi
exit 1
