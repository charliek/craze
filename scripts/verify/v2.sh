#!/usr/bin/env bash
# v2.sh — `craze prompt --json` parity (V2) of a committed tree against the
# baseline commit 6581e0a, driven by run_v2.py beside this script.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

BASE_SHA=6581e0a
OLD_PAIR_DIR=${CRAZE_VERIFY_V2_OLD_PAIR:-$HOME/.claude/plans/craze/021-session-control-s1b-engine/v2}

usage() {
	cat <<EOF
usage: v2.sh <label> <sha> [--only S] [--repeat N] [--calibrate | --equivalence]

Compare mode (the default): builds craze and craze-fake-agent from the
COMMITTED tree at <sha> (the candidate) and runs run_v2.py's 103-scenario
matrix against the baseline pair built from $BASE_SHA: exit status and ordered
stdout (the seq value stripped) must match; stderr is not compared. Every
scenario uses the baseline's fake agent, except the sigint-hold one, which
uses the candidate's on both sides (it is the only fake that knows that
script; Plan 036 X3). Prints V2: <n> SAME / <m> DIFF.

The baseline pair is built once from $BASE_SHA (-trimpath, the current Go
toolchain) into \${XDG_CACHE_HOME:-~/.cache}/craze-verify/baseline-$BASE_SHA-<goos>-<goarch>-<go version>/,
in a temporary directory published with an atomic rename, so an interrupted or
concurrent build never leaves a partial pair. A shallow clone cannot build it
(run git fetch --unshallow).

--equivalence proves the rebuilt baseline equals the one Plan 021 pinned:
  the full matrix in compare mode with Plan 021's saved pair
  ($OLD_PAIR_DIR/craze-baseline-$BASE_SHA and
  craze-fake-agent-baseline-$BASE_SHA; CRAZE_VERIFY_V2_OLD_PAIR overrides the
  directory) as the baseline and the rebuilt pair as the candidate
  (--candidate-fake-agent). <sha> only supplies the sigint-hold fake agent.
  It must report 103 SAME.
--calibrate runs the rebuilt baseline against itself (--repeat, default 3)
  and reports every field that varies (V2: <n> stable / <m> VARIES).
--only S runs the scenarios whose name contains S; --repeat N repeats the
  matrix N times.

Output: \$OUT/v2-<stamp>/ (per-scenario stdout/stderr/meta, diff.txt for a
DIFF, bin/ with the candidate pair). Log: \$OUT/v2-<stamp>.log
$CV_HELP_COMMON
EOF
}

if cv_wants_help "$@"; then usage; exit 0; fi
cv_setup V2
[[ $# -ge 2 ]] || { usage >&2; die 2 "want <label> <sha> [options]"; }
cv_label "$1"
rev=$2
shift 2
mode=compare only='' repeat=''
while (($#)); do
	case $1 in
	--only) (($# >= 2)) || die 2 "--only needs a value"; only=$2; shift 2 ;;
	--repeat) (($# >= 2)) || die 2 "--repeat needs a value"; repeat=$2; shift 2 ;;
	--calibrate) [[ $mode == compare ]] || die 2 "--calibrate and --equivalence exclude each other"; mode=calibrate; shift ;;
	--equivalence) [[ $mode == compare ]] || die 2 "--calibrate and --equivalence exclude each other"; mode=equivalence; shift ;;
	*) usage >&2; die 2 "unknown option: $1" ;;
	esac
done
[[ -z $repeat || ($repeat =~ ^[0-9]+$ && $repeat -ge 1) ]] || die 2 "--repeat must be a positive integer: $repeat"
command -v python3 >/dev/null || die 3 "python3 not found"
cv_resolve "$rev"
stamp=$(cv_stamp)
run=$OUT/v2-$stamp
CV_LOG=$run.log
mkdir -p "$run/bin" || die 2 "cannot create $run"
: >"$CV_LOG"

# The candidate pair (also the sigint-hold fake agent).
export_tree "$CV_SHA_FULL"
ctree=$CV_LAST_EXPORT
build_cand() {
	cd "$ctree" &&
		go build -trimpath -o "$run/bin/craze" ./cmd/craze &&
		go build -trimpath -o "$run/bin/craze-fake-agent" ./cmd/craze-fake-agent
}
cv_run "$CV_LOG" build_cand || { tail -20 "$CV_LOG"; die 2 "candidate build failed at $CV_SHA (see $CV_LOG)"; }

# The baseline pair, built once per toolchain.
read -r goos goarch gover < <(cd "$ROOT" && go env GOOS GOARCH GOVERSION | paste -sd' ' -) ||
	die 2 "go env failed in $ROOT"
[[ -n ${gover:-} ]] || die 2 "go env reported no GOVERSION"
cache=${XDG_CACHE_HOME:-$HOME/.cache}/craze-verify
bdir=$cache/baseline-$BASE_SHA-$goos-$goarch-$gover
if [[ -x $bdir/craze && -x $bdir/craze-fake-agent ]]; then
	echo "V2 baseline: $bdir (cached)"
else
	if ! git -C "$ROOT" cat-file -e "$BASE_SHA^{commit}" 2>/dev/null; then
		if [[ $(git -C "$ROOT" rev-parse --is-shallow-repository 2>/dev/null) == true ]]; then
			die 2 "baseline commit $BASE_SHA is missing from this shallow clone: run git fetch --unshallow"
		fi
		die 2 "baseline commit $BASE_SHA is not in $ROOT"
	fi
	mkdir -p "$cache" || die 2 "cannot create $cache"
	full=$(git -C "$ROOT" rev-parse "$BASE_SHA^{commit}")
	export_tree "$full"
	btree=$CV_LAST_EXPORT
	btmp=$(mktemp -d "$cache/.baseline-tmp.XXXX") || die 2 "cannot create a temp dir in $cache"
	CV_EXPORTS+=("$btmp")
	# The current toolchain, whatever the old tree's own pins say.
	gobin=$(cd "$ROOT" && go env GOROOT)/bin/go
	build_base() {
		cd "$btree" &&
			GOTOOLCHAIN=local "$gobin" build -trimpath -o "$btmp/craze" ./cmd/craze &&
			GOTOOLCHAIN=local "$gobin" build -trimpath -o "$btmp/craze-fake-agent" ./cmd/craze-fake-agent
	}
	cv_run "$CV_LOG" build_base || { tail -20 "$CV_LOG"; die 2 "baseline build of $BASE_SHA failed (see $CV_LOG)"; }
	got=$("$gobin" version "$btmp/craze" | awk '{print $2}')
	[[ $got == "$gover" ]] || die 2 "the baseline was built with $got, not $gover"
	if python3 -c 'import os, sys; os.rename(sys.argv[1], sys.argv[2])' "$btmp" "$bdir" 2>/dev/null; then
		echo "V2 baseline: $bdir (built)"
	elif [[ -x $bdir/craze && -x $bdir/craze-fake-agent ]]; then
		echo "V2 baseline: $bdir (built concurrently by another run)"
	else
		die 2 "cannot publish the baseline to $bdir"
	fi
fi

args=()
case $mode in
compare)
	args=(--baseline "$bdir/craze" --fake-agent "$bdir/craze-fake-agent"
		--candidate "$run/bin/craze" --hold-fake-agent "$run/bin/craze-fake-agent")
	;;
calibrate)
	args=(--calibrate --baseline "$bdir/craze" --fake-agent "$bdir/craze-fake-agent"
		--hold-fake-agent "$run/bin/craze-fake-agent")
	;;
equivalence)
	ob=$OLD_PAIR_DIR/craze-baseline-$BASE_SHA
	of=$OLD_PAIR_DIR/craze-fake-agent-baseline-$BASE_SHA
	[[ -x $ob && -x $of ]] || die 2 "Plan 021's saved pair is not at $OLD_PAIR_DIR (set CRAZE_VERIFY_V2_OLD_PAIR)"
	args=(--baseline "$ob" --fake-agent "$of"
		--candidate "$bdir/craze" --candidate-fake-agent "$bdir/craze-fake-agent"
		--hold-fake-agent "$run/bin/craze-fake-agent")
	;;
esac
[[ -n $only ]] && args+=(--only "$only")
[[ -n $repeat ]] && args+=(--repeat "$repeat")
args+=(--out "$run")
echo "V2 mode=$mode candidate=$CV_SHA baseline=$BASE_SHA run=$run"
cv_run "$CV_LOG" python3 "$CV_DIR/run_v2.py" "${args[@]}"
rc=$?

if [[ $mode == calibrate ]]; then
	stable=$(grep -c '^\[stable\]' "$CV_LOG")
	varies=$(grep -c '^\[VARIES\]' "$CV_LOG")
	grep -A4 '^\[VARIES\]' "$CV_LOG" | head -40
	echo "V2: $stable stable / $varies VARIES"
else
	# A result row: name, base_rc, cand_rc, base_lines, cand_lines, result.
	row='^[^ ]+ +[^ ]+ +[^ ]+ +[0-9]+ +[0-9]+ +'
	same=$(grep -cE "${row}SAME( +\(flaky_ok=False\))?\$" "$CV_LOG")
	diff=$(grep -cE "${row}DIFF( +\(flaky_ok=False\))?\$" "$CV_LOG")
	grep -E -A1 "${row}DIFF( +\(flaky_ok=False\))?\$" "$CV_LOG" | head -40
	echo "V2: $same SAME / $diff DIFF"
fi
case $rc in
0) exit 0 ;;
1) exit 1 ;;
*) tail -5 "$CV_LOG"; die 2 "run_v2.py exited $rc (see $CV_LOG)" ;;
esac
