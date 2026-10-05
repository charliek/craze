#!/usr/bin/env bash
# v1.sh — the -race soak (V1), as a manifest of jobs that each fit one
# background command: --plan, then one --job per command, then --summary.
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

usage() {
	cat <<EOF
usage: v1.sh <label> --plan [--sha S] [--base REF] [--pkgs "P…" | --changed | --makefile-set] [--count N]
       v1.sh <label> --job <id> [--sha S]
       v1.sh <label> --summary

The -race soak of a COMMITTED tree, one job per background command (never
groups), each well under the 2 h background cap.

--plan prints (and writes to \$OUT/v1-manifest.tsv) a deterministic manifest,
"id pkg count est-min":
  - the packages: --pkgs (go patterns, as given), --makefile-set (the
    Makefile's test-race list, expanded with go list) or --changed (default:
    the .go files changed between merge-base(--base, sha) and sha, plus every
    package that imports them, restricted to the test-race list). --base
    defaults to origin/main, --sha to HEAD, --count to 20. Packages without
    test files are left out.
  - each package is one job of -race -count=<count>, except ./internal/tui,
    which is ceil(count/5) jobs of -count=5 (tui-s1…; the remainder slice takes
    the rest), and any package whose job is estimated over 100 min, which is
    split by count the same way.
  - est-min = ceil(count x per-pass seconds / 60) + 5 (build and export). The
    per-pass table is below in the script; an unknown package is 20 s a pass.
--job <id> runs that one job from the manifest:
    CRAZE_GOLDEN_TRANSPORT=both go test -race -count=N -timeout <min(2.5 x est, 105)>m <pkg>
  in its own process group, with a watchdog that kills the whole group at
  110 min wall-clock (CRAZE_VERIFY_V1_WATCHDOG_S overrides, in seconds) and
  reports INCOMPLETE. A tui job first takes a non-blocking flock on the
  per-user lock \${XDG_CACHE_HOME:-~/.cache}/craze-verify/locks/v1-tui.lock and
  is refused at once (exit 4) while another tui job holds it: two tui -race
  jobs never run together. A keeper process in a session of its own holds
  the lock too and lets it go only once the job's group is gone (or a minute
  past the watchdog), so it outlives a SIGKILL of this script while the
  group lives; the job's processes never hold it, so a test's detached
  descendant cannot keep it. --sha, when given, must be the manifest's sha
  (else exit 2).
  Writes \$OUT/v1-<id>.log and \$OUT/v1-<id>.result, bound to the manifest
  (its generation, sha, package and count).
--summary prints JOB PKG COUNT RC OK FAIL DATA_RACE ELAPSED STATUS (+ the
  first 3 failing tests) for every manifest job, then
  V1_TOTAL jobs= ok= fail= data_race= incomplete= not_run= stale=
  A result from another manifest (an earlier --plan under this label, even an
  identical one) is STALE: it counts for nothing, and the job must be run.
A failed job is never retried: diagnose, do not re-run.
Job exit: 0 PASS, 1 FAIL, 2 INCOMPLETE or BUILD_FAILED, 4 refused.
$CV_HELP_COMMON
EOF
}

# Per -race pass, seconds, with CRAZE_GOLDEN_TRANSPORT=both.
#   tui, hub, cli: measured 2026-10-04 at a3aa101, one -count=1 pass each on
#     the 32-core Linux box with one other implementer running (load 1.5-2):
#     tui 307.2 s, hub 83.5 s, cli 53.3 s (the `ok` line's time).
#   the rest: Plan 036's V1 logs at 69fb29b (-count=20, divided by 20).
pass_seconds() {
	case $1 in
	./internal/tui) echo 308 ;;
	./internal/hub) echo 84 ;;
	./internal/cli) echo 54 ;;
	./internal/control) echo 48 ;;
	./internal/control/wiretest) echo 1 ;;
	./internal/engine) echo 41 ;;
	./internal/fakehost) echo 5 ;;
	./internal/harness/tool/opencode) echo 10 ;;
	./internal/protocol) echo 5 ;;
	./internal/rundir) echo 1 ;;
	*) echo 20 ;;
	esac
}
OVERHEAD_MIN=5
BUDGET_MIN=100
WATCHDOG_S=${CRAZE_VERIFY_V1_WATCHDOG_S:-6600}
LOCK=${XDG_CACHE_HOME:-$HOME/.cache}/craze-verify/locks/v1-tui.lock
KEEPER=

# The tui lock's keeper: started with fd 9 (the locked file) in a session of
# its own, so neither a SIGKILL of v1.sh nor one of the job's group reaches
# it. It polls the job's group about once a second and exits (closing fd 9,
# releasing the lock) once the group is gone, or at its deadline.
# shellcheck disable=SC2016
KEEPER_SH='g=$1 end=$(($(date +%s) + $2))
while kill -0 -- "-$g" 2>/dev/null && (($(date +%s) < end)); do sleep 1; done'

# release_lock: the job's group is gone; drop the tui lock (flock -u frees it
# for the keeper too: one open file description), then end the keeper.
release_lock() {
	flock -u 9 2>/dev/null
	exec 9>&-
	if [[ -n $KEEPER ]]; then
		kill "$KEEPER" 2>/dev/null
		wait "$KEEPER" 2>/dev/null
		KEEPER=
	fi
}

est_min() { # est_min <pkg> <count>
	local s
	s=$(pass_seconds "$1")
	echo $(((($2 * s) + 59) / 60 + OVERHEAD_MIN))
}

job_base() { # ./internal/harness/tool/opencode -> harness-tool-opencode
	local p=${1#./}
	p=${p#internal/}
	[[ -z $p || $p == . ]] && p=root
	echo "${p//\//-}"
}

# go_pkgs <tree> <pattern…>: ./rel dirs of the packages with test files, sorted.
go_pkgs() {
	local tree=$1 real out
	shift
	real=$(cd "$tree" && pwd -P)
	out=$(cd "$tree" && go list -e -f '{{if or .TestGoFiles .XTestGoFiles}}{{.Dir}}{{end}}' "$@") ||
		die 2 "go list failed for: $*"
	local d
	while IFS= read -r d; do
		[[ -z $d ]] && continue
		if [[ $d == "$real" ]]; then echo .; else echo "./${d#"$real"/}"; fi
	done <<<"$out" | LC_ALL=C sort -u
}

# makefile_set <tree>: the patterns of the Makefile's test-race recipe.
makefile_set() {
	awk '/^test-race:/{r=1; next} r && /^[^\t]/{r=0} r && /go test/{for(i=1;i<=NF;i++) if ($i ~ /^\.\//) print $i}' "$1/Makefile"
}

# changed_pkgs <tree> <merge-base> <sha>: the test-race packages that changed
# or import (directly, transitively, or from their tests) a changed package.
changed_pkgs() {
	local tree=$1 mb=$2 sha=$3 mod files
	files=$(git -C "$ROOT" diff --name-only "$mb" "$sha" -- '*.go') || die 2 "git diff failed"
	[[ -z $files ]] && return 0
	mod=$(cd "$tree" && go list -m) || die 2 "go list -m failed"
	local changed
	changed=$(while IFS= read -r f; do
		d=$(dirname "$f")
		[[ $d == . ]] && echo "$mod" || echo "$mod/$d"
	done <<<"$files" | LC_ALL=C sort -u)
	local set
	set=$(makefile_set "$tree")
	[[ -n $set ]] || die 2 "no test-race packages found in $tree/Makefile"
	# shellcheck disable=SC2086
	(cd "$tree" && go list -e -test -f '{{.ImportPath}}|{{join .Deps " "}}' $set) >"$tree/.v1-deps" ||
		die 2 "go list -deps failed"
	local hits
	hits=$(CV_CHANGED=$changed awk -F'|' '
		BEGIN { n = split(ENVIRON["CV_CHANGED"], c, "\n"); for (i = 1; i <= n; i++) ch[c[i]] = 1 }
		{
			p = $1; sub(/ \[.*\]$/, "", p); sub(/\.test$/, "", p); sub(/_test$/, "", p)
			hit = (p in ch)
			m = split($2, d, " ")
			for (i = 1; i <= m && !hit; i++) if (d[i] in ch) hit = 1
			if (hit) print p
		}' "$tree/.v1-deps" | LC_ALL=C sort -u)
	[[ -z $hits ]] && return 0
	local inset
	inset=$(go_pkgs "$tree" $set) || exit $? # die in $(…) ends only the subshell
	local p rel
	while IFS= read -r p; do
		rel=${p#"$mod"}
		rel=.${rel}
		[[ $rel == . ]] || rel=./${rel#./}
		grep -qxF -- "$rel" <<<"$inset" && echo "$rel"
	done <<<"$hits" | LC_ALL=C sort -u
}

do_plan() {
	local sha=HEAD base=origin/main sel=changed pkgs='' count=20
	while (($#)); do
		case $1 in
		--sha) (($# >= 2)) && [[ -n $2 ]] || die 2 "--sha needs a non-empty value"; sha=$2; shift 2 ;;
		--base) (($# >= 2)) && [[ -n $2 ]] || die 2 "--base needs a non-empty value"; base=$2; shift 2 ;;
		--pkgs) (($# >= 2)) && [[ -n $2 ]] || die 2 "--pkgs needs a non-empty value"; sel=pkgs; pkgs=$2; shift 2 ;;
		--changed) sel=changed; shift ;;
		--makefile-set) sel=makefile-set; shift ;;
		--count) (($# >= 2)) && [[ -n $2 ]] || die 2 "--count needs a non-empty value"; count=$2; shift 2 ;;
		*) usage >&2; die 2 "unknown --plan option: $1" ;;
		esac
	done
	[[ $count =~ ^[0-9]+$ && $count -ge 1 ]] || die 2 "--count must be a positive integer: $count"
	cv_resolve "$sha"
	CV_LOG=$OUT/v1-manifest.tsv
	export_tree "$CV_SHA_FULL"
	local tree=$CV_LAST_EXPORT list
	case $sel in
	pkgs)
		# shellcheck disable=SC2086
		list=$(go_pkgs "$tree" $pkgs) || exit $? # die in $(…) ends only the subshell
		;;
	makefile-set)
		local set
		set=$(makefile_set "$tree")
		[[ -n $set ]] || die 2 "no test-race packages found in the Makefile at $CV_SHA"
		# shellcheck disable=SC2086
		list=$(go_pkgs "$tree" $set) || exit $? # die in $(…) ends only the subshell
		;;
	changed)
		local mb
		mb=$(git -C "$ROOT" merge-base "$base" "$CV_SHA_FULL") || die 2 "no merge base of $base and $CV_SHA"
		list=$(changed_pkgs "$tree" "$mb" "$CV_SHA_FULL") || exit $? # die in $(…) ends only the subshell
		sel="changed base=$base merge-base=$(git -C "$ROOT" rev-parse --short "$mb")"
		;;
	esac
	local tmp=$CV_LOG.tmp.$$
	# The generation binds every job result to this manifest: a re-plan under
	# the label (even an identical one) makes the old results STALE.
	{
		echo "# craze-verify v1 manifest"
		echo "# sha=$CV_SHA_FULL count=$count selection=$sel generated=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
		echo "# gen=$(date +%s)-$$-$RANDOM$RANDOM"
	} >"$tmp"
	local p n=0 max=0 total=0 slice e i left c id
	while IFS= read -r p; do
		[[ -z $p ]] && continue
		slice=$count
		if [[ $p == ./internal/tui ]]; then
			slice=5
		elif (($(est_min "$p" "$count") > BUDGET_MIN)); then
			slice=5
		fi
		while ((slice > 1 && $(est_min "$p" "$slice") > BUDGET_MIN)); do slice=$((slice - 1)); done
		(($(est_min "$p" 1) > BUDGET_MIN)) && die 2 "$p: one pass is estimated over $BUDGET_MIN min"
		((slice > count)) && slice=$count
		left=$count i=0
		while ((left > 0)); do
			i=$((i + 1))
			c=$((left < slice ? left : slice))
			left=$((left - c))
			id=$(job_base "$p")
			if [[ $p == ./internal/tui ]] || ((slice < count)); then id=$id-s$i; fi
			e=$(est_min "$p" "$c")
			printf '%s\t%s\t%s\t%s\n' "$id" "$p" "$c" "$e" >>"$tmp"
			n=$((n + 1)) total=$((total + e))
			((e > max)) && max=$e
		done
	done <<<"$list"
	mv -f "$tmp" "$CV_LOG"
	printf '%-28s %-36s %5s %7s\n' id pkg count est-min
	grep -v '^#' "$CV_LOG" | while IFS=$'\t' read -r id p c e; do
		printf '%-28s %-36s %5s %7s\n' "$id" "$p" "$c" "$e"
	done
	echo "V1_PLAN jobs=$n max_est=${max}m total_est=${total}m selection=$sel manifest=$CV_LOG"
	((n > 0)) || echo "v1.sh: the selection is empty" >&2
	return 0
}

manifest_sha() { sed -n 's/^# sha=\([0-9a-f]*\) .*/\1/p' "$1" | head -1; }
manifest_gen() { sed -n 's/^# gen=\([^ ]*\)$/\1/p' "$1" | head -1; }

do_job() {
	local id=${1:-} sha=''
	[[ -n $id ]] || die 2 "--job needs an id"
	shift
	while (($#)); do
		case $1 in
		--sha) (($# >= 2)) && [[ -n $2 ]] || die 2 "--sha needs a non-empty value"; sha=$2; shift 2 ;;
		*) usage >&2; die 2 "unknown --job option: $1" ;;
		esac
	done
	local manifest=$OUT/v1-manifest.tsv row pkg count est msha gen
	[[ -f $manifest ]] || die 2 "no manifest at $manifest: run v1.sh $CV_LABEL --plan first"
	row=$(awk -F'\t' -v id="$id" '$1 == id' "$manifest")
	[[ -n $row ]] || die 2 "no job '$id' in $manifest"
	IFS=$'\t' read -r _ pkg count est <<<"$row"
	msha=$(manifest_sha "$manifest") gen=$(manifest_gen "$manifest")
	[[ -n $msha && -n $gen ]] || die 2 "$manifest has no sha or gen header: run v1.sh $CV_LABEL --plan again"
	cv_resolve "${sha:-$msha}"
	[[ $CV_SHA_FULL == "$msha" ]] ||
		die 2 "--sha $sha is not the manifest's sha ($msha): its result would be stale; run --plan --sha $sha first"
	CV_LOG=$OUT/v1-$id.log
	local result=$OUT/v1-$id.result
	rm -f "$result"

	if [[ $pkg == ./internal/tui ]]; then
		command -v flock >/dev/null || die 3 "flock not found: a tui job needs util-linux flock"
		command -v setsid >/dev/null || die 3 "setsid not found: a tui job needs util-linux setsid"
		mkdir -p "$(dirname "$LOCK")" || die 2 "cannot create $(dirname "$LOCK")"
		exec 9>>"$LOCK" || die 2 "cannot open $LOCK"
		flock -n 9 || die 4 "a tui job is running; launch this one after it (lock $LOCK)"
		echo "V1 tui lock held: $LOCK"
		# On a handled exit (a signal, a die) cv_on_exit stops the group, then
		# this releases the lock, unless the group survived SIGKILL (then the
		# keeper holds it until the group is gone).
		cv_cleanup_hook() { ((CV_STOP_FAILED)) || release_lock; }
	fi

	: >"$CV_LOG"
	export_tree "$CV_SHA_FULL"
	local tree=$CV_LAST_EXPORT
	local timeout=$(((est * 5 + 1) / 2))
	((timeout > 105)) && timeout=105
	echo "V1 job $id: $pkg -race -count=$count -timeout ${timeout}m (est ${est}m) at $CV_SHA" | tee -a "$CV_LOG"

	run_job() { cd "$tree" && CRAZE_GOLDEN_TRANSPORT=both go test -race -count="$count" -timeout "${timeout}m" "$pkg"; }
	local s0 marker=$OUT/v1-$id.incomplete.$$
	rm -f "$marker"
	local left=$((WATCHDOG_S - ($(date +%s) - CV_START)))
	((left < 1)) && left=1
	s0=$(date +%s)
	cv_start "$CV_LOG" run_job
	local g=$CV_CHILD
	if [[ $pkg == ./internal/tui ]]; then
		# The job has no fd 9 (cv_start closes it); the keeper takes this
		# script's. Job control is off here, so the keeper is no group leader
		# and setsid execs it in place: $! is the keeper. A SIGKILL in the
		# instant between cv_start and this line frees the lock early (the
		# same microsecond class as the pgid-reuse residual).
		setsid bash -c "$KEEPER_SH" v1-tui-lock-keeper "$g" "$((left + 60))" </dev/null >/dev/null 2>&1 &
		KEEPER=$!
		echo "V1 tui lock keeper pid=$KEEPER holds it until group $g is gone"
	fi
	# The watchdog closes fd 9: only this script and the keeper hold the lock.
	set -m
	(
		trap - INT TERM HUP EXIT
		end=$(($(date +%s) + left))
		while (($(date +%s) < end)); do
			kill -0 -- "-$g" 2>/dev/null || exit 0
			sleep 1
		done
		: >"$marker"
		kill -TERM -- "-$g" 2>/dev/null
		sleep 10
		kill -KILL -- "-$g" 2>/dev/null
	) </dev/null >/dev/null 2>&1 9>&- &
	CV_WATCHDOG=$!
	set +m
	echo "V1 watchdog pgid=$CV_WATCHDOG at ${left}s"

	local rc=0
	cv_wait || rc=$? # dies (2) when the group survives SIGKILL: the lock stays held
	cv_stop_group "$CV_WATCHDOG" || {
		CV_STOP_FAILED=1
		die 2 "the watchdog (group $CV_WATCHDOG) survived SIGKILL"
	}
	CV_WATCHDOG=
	local elapsed=$(($(date +%s) - s0))
	# The group is gone (cv_wait ended what was left of it); only now may the
	# lock go, so a second tui job never overlaps this one's stragglers.
	if [[ $pkg == ./internal/tui ]]; then
		release_lock
		unset -f cv_cleanup_hook
		echo "V1 tui lock released"
	fi

	local ok fail dr names status
	ok=$(grep -cE '^ok[[:space:]]' "$CV_LOG")
	fail=$(grep -cE '^(--- FAIL|FAIL)' "$CV_LOG")
	dr=$(grep -c 'WARNING: DATA RACE' "$CV_LOG")
	names=$(grep -oE '^--- FAIL: [^ ]+' "$CV_LOG" | awk '{print $3}' | awk '!s[$0]++' | head -3 | paste -sd, -)
	if [[ -e $marker ]]; then
		status=INCOMPLETE
		rm -f "$marker"
	elif ((rc == 0 && dr == 0)); then
		status=PASS
	else
		cv_go_verdict "$rc" "$CV_LOG" 2>/dev/null
		if (($? == 2)); then status=BUILD_FAILED; else status=FAIL; fi
	fi
	printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
		"$id" "$gen" "$CV_SHA_FULL" "$pkg" "$count" "$rc" "$ok" "$fail" "$dr" "${elapsed}s" "$status" "${names:--}" >"$result"
	grep -E '^(--- FAIL|FAIL|ok|panic)' "$CV_LOG" | head -10
	echo "V1_JOB id=$id pkg=$pkg count=$count rc=$rc ok=$ok fail=$fail data_race=$dr elapsed=${elapsed}s status=$status${names:+ failing=$names}"
	case $status in
	PASS) return 0 ;;
	FAIL) echo "v1.sh: diagnose, do not re-run" >&2; return 1 ;;
	*) echo "v1.sh: $status: diagnose, do not re-run" >&2; return 2 ;;
	esac
}

do_summary() {
	(($# == 0)) || die 2 "--summary takes no options"
	local manifest=$OUT/v1-manifest.tsv
	[[ -f $manifest ]] || die 2 "no manifest at $manifest: run v1.sh $CV_LABEL --plan first"
	local msha gen
	msha=$(manifest_sha "$manifest") gen=$(manifest_gen "$manifest")
	[[ -n $msha && -n $gen ]] || die 2 "$manifest has no sha or gen header: run v1.sh $CV_LABEL --plan again"
	CV_SHA=$(git -C "$ROOT" rev-parse --short "$msha" 2>/dev/null || echo -)
	CV_LOG=$manifest
	local jobs=0 ok=0 fail=0 dr=0 inc=0 notrun=0 stale=0
	local fmt='%-24s %-34s %5s %4s %4s %5s %9s %8s %-12s %s\n'
	# shellcheck disable=SC2059
	printf "$fmt" JOB PKG COUNT RC OK FAIL DATA_RACE ELAPSED STATUS FAILING
	local id pkg count est rid rgen rsha rpkg rcount rrc rok rfail rdr rel rstatus rnames
	while IFS=$'\t' read -r id pkg count est; do
		[[ -z $id || $id == \#* ]] && continue
		jobs=$((jobs + 1))
		if [[ ! -f $OUT/v1-$id.result ]]; then
			notrun=$((notrun + 1))
			# shellcheck disable=SC2059
			printf "$fmt" "$id" "$pkg" "$count" - - - - - NOT_RUN -
			continue
		fi
		rid='' rgen='' rsha='' rpkg='' rcount=''
		IFS=$'\t' read -r rid rgen rsha rpkg rcount rrc rok rfail rdr rel rstatus rnames <"$OUT/v1-$id.result"
		if [[ $rid != "$id" || $rgen != "$gen" || $rsha != "$msha" || $rpkg != "$pkg" || $rcount != "$count" ]]; then
			stale=$((stale + 1))
			# shellcheck disable=SC2059
			printf "$fmt" "$id" "$pkg" "$count" - - - - - STALE "(a result from another manifest)"
			continue
		fi
		# shellcheck disable=SC2059
		printf "$fmt" "$id" "$pkg" "$count" "$rrc" "$rok" "$rfail" "$rdr" "$rel" "$rstatus" "$rnames"
		dr=$((dr + rdr))
		case $rstatus in
		PASS) ok=$((ok + 1)) ;;
		FAIL) fail=$((fail + 1)) ;;
		*) inc=$((inc + 1)) ;;
		esac
	done <"$manifest"
	echo "V1_TOTAL jobs=$jobs ok=$ok fail=$fail data_race=$dr incomplete=$inc not_run=$notrun stale=$stale"
	((fail > 0 || dr > 0)) && { echo "v1.sh: diagnose, do not re-run" >&2; return 1; }
	((inc > 0 || notrun > 0 || stale > 0)) && return 2
	return 0
}

if cv_wants_help "$@"; then usage; exit 0; fi
cv_setup V1
[[ $# -ge 2 ]] || { usage >&2; die 2 "want <label> --plan|--job <id>|--summary"; }
cv_label "$1"
mode=$2
shift 2
case $mode in
--plan) do_plan "$@" ;;
--job) do_job "$@" ;;
--summary) do_summary "$@" ;;
*) usage >&2; die 2 "unknown mode: $mode" ;;
esac
exit $?
