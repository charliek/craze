#!/usr/bin/env bash
# v8.sh — which goldens, wire fixtures, schema files and test fixtures a
# change adds, modifies, deletes or renames (V8).
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

usage() {
	cat <<EOF
usage: v8.sh [--base REF] [--head REF] [--expect-none-modified]
       v8.sh --selftest

Lists what changed between merge-base(--base, --head) and --head (defaults
origin/main and HEAD; commits, never the working tree) under
  git diff --name-status -M <merge-base> <head> -- \\
    ':(glob)**/*.golden' ':(glob)**/testdata/**' ':(glob)**/*.ndjson' \\
    ':(glob)**/*.jsonl' ':(glob)tests/cli/fixtures/**' \\
    ':(glob)**/protocol/schema/**' ':(exclude,glob)eval/**'
as ADDED, MODIFIED (a type change counts), DELETED and RENAMED, each path
classed wire (internal/fakehost/testdata/wire, internal/acp/testdata), schema
(…/protocol/schema/…), golden (*.golden*) or fixture, then
  V8 added= modified= deleted= renamed= wire_or_schema_modified=
A wire or schema path that is modified, deleted or renamed prints
WIRE-MOVED: needs owner approval.
--expect-none-modified exits 1 on any M, T, D or R (an existing path moved).
--selftest checks every class x {A, M, D, R, type change, a name that needs
quoting} in a scratch git repository.
V8 proves files are preserved, not that behaviour is: V2 and the tests do that.
$CV_HELP_COMMON
EOF
}

PATHSPECS=(
	':(glob)**/*.golden'
	':(glob)**/testdata/**'
	':(glob)**/*.ndjson'
	':(glob)**/*.jsonl'
	':(glob)tests/cli/fixtures/**'
	':(glob)**/protocol/schema/**'
	':(exclude,glob)eval/**'
)

classify() {
	case $1 in
	internal/fakehost/testdata/wire/* | internal/acp/testdata/*) echo wire ;;
	protocol/schema/* | */protocol/schema/*) echo schema ;;
	*.golden | *.golden.*) echo golden ;;
	*) echo fixture ;;
	esac
}

show() { # a path as-is, or shell-quoted when it holds a control character
	if [[ $1 == *[[:cntrl:]]* ]]; then printf '%q' "$1"; else printf '%s' "$1"; fi
}

# v8_run <base> <head> <expect>: the listing; returns 0, or 1 under <expect>=1
# when an existing path moved.
v8_run() {
	local base=$1 head=$2 expect=$3 mb hb
	hb=$(git -C "$ROOT" rev-parse --verify --quiet "$head^{commit}") || die 2 "not a commit: $head"
	git -C "$ROOT" rev-parse --verify --quiet "$base^{commit}" >/dev/null || die 2 "not a commit: $base"
	mb=$(git -C "$ROOT" merge-base "$base" "$hb") || die 2 "no merge base of $base and $head"
	CV_SHA=$(git -C "$ROOT" rev-parse --short "$hb")
	local raw
	raw=$(mktemp "${TMPDIR:-/tmp}/cv-v8.XXXX") || die 2 "mktemp failed"
	CV_EXPORTS+=("$raw")
	git -C "$ROOT" diff --name-status -z -M "$mb" "$hb" -- "${PATHSPECS[@]}" >"$raw" ||
		die 2 "git diff failed"
	local -a lines_a=() lines_m=() lines_d=() lines_r=()
	local a=0 m=0 d=0 r=0 w=0 st p q cls
	while IFS= read -r -d '' st; do
		IFS= read -r -d '' p || break
		case $st in
		R*)
			IFS= read -r -d '' q || break
			cls=$(classify "$q")
			lines_r+=("$(printf '%-8s %-7s %s -> %s' RENAMED "$cls" "$(show "$p")" "$(show "$q")")")
			r=$((r + 1))
			[[ $cls == wire || $cls == schema || $(classify "$p") == wire || $(classify "$p") == schema ]] && {
				w=$((w + 1))
				lines_r+=("WIRE-MOVED: needs owner approval: $(show "$p")")
			}
			;;
		C*)
			IFS= read -r -d '' q || break
			cls=$(classify "$q")
			lines_a+=("$(printf '%-8s %-7s %s (copied from %s)' ADDED "$cls" "$(show "$q")" "$(show "$p")")")
			a=$((a + 1))
			;;
		A)
			cls=$(classify "$p")
			lines_a+=("$(printf '%-8s %-7s %s' ADDED "$cls" "$(show "$p")")")
			a=$((a + 1))
			;;
		M | T)
			cls=$(classify "$p")
			lines_m+=("$(printf '%-8s %-7s %s%s' MODIFIED "$cls" "$(show "$p")" "$([[ $st == T ]] && echo ' (type change)')")")
			m=$((m + 1))
			[[ $cls == wire || $cls == schema ]] && {
				w=$((w + 1))
				lines_m+=("WIRE-MOVED: needs owner approval: $(show "$p")")
			}
			;;
		D)
			cls=$(classify "$p")
			lines_d+=("$(printf '%-8s %-7s %s' DELETED "$cls" "$(show "$p")")")
			d=$((d + 1))
			[[ $cls == wire || $cls == schema ]] && {
				w=$((w + 1))
				lines_d+=("WIRE-MOVED: needs owner approval: $(show "$p")")
			}
			;;
		*)
			lines_m+=("$(printf '%-8s %-7s %s (status %s)' MODIFIED "$(classify "$p")" "$(show "$p")" "$st")")
			m=$((m + 1))
			;;
		esac
	done <"$raw"
	echo "V8 base=$base merge-base=$(git -C "$ROOT" rev-parse --short "$mb") head=$CV_SHA"
	local x
	for x in "${lines_a[@]+"${lines_a[@]}"}" "${lines_m[@]+"${lines_m[@]}"}" \
		"${lines_d[@]+"${lines_d[@]}"}" "${lines_r[@]+"${lines_r[@]}"}"; do
		echo "$x"
	done
	echo "V8 added=$a modified=$m deleted=$d renamed=$r wire_or_schema_modified=$w"
	if [[ $expect == 1 ]] && ((m + d + r > 0)); then
		echo "v8.sh: an existing golden, fixture, wire or schema file moved (--expect-none-modified)" >&2
		return 1
	fi
	return 0
}

selftest() {
	local dir repo out rc=0
	dir=$(mktemp -d "${CRAZE_VERIFY_TMP:-/tmp}/cv-v8self.XXXX") || die 2 "mktemp failed"
	CV_EXPORTS+=("$dir")
	repo=$dir/repo
	mkdir -p "$repo"
	g() { git -C "$repo" -c user.name=v8 -c user.email=v8@example.invalid -c commit.gpgsign=false "$@" >/dev/null 2>&1; }
	g init -q
	put() { mkdir -p "$repo/$(dirname "$1")" && printf '%s\n' "$2" >"$repo/$1"; }
	# One file per class x change; every content unique so renames are exact.
	local cls_paths=(
		"wire:internal/fakehost/testdata/wire/0"
		"wire:internal/acp/testdata/grok-queue/0"
		"schema:internal/protocol/schema/0"
		"schema:docs/reference/protocol/schema/0"
		"golden:internal/tui/testdata/0"
		"fixture:internal/agent/testdata/0"
		"fixture:tests/cli/fixtures/probe/0"
	)
	ext() { case $1 in wire:internal/fakehost*) echo .ndjson ;; wire:*) echo .jsonl ;; schema:*) echo .json ;; golden:*) echo .golden ;; *) echo .txt ;; esac; }
	local e cp base
	for e in "${cls_paths[@]}"; do
		cp=${e#*:}
		base=${cp%0}
		local x
		x=$(ext "$e")
		put "${base}m$x" "m $e"
		put "${base}d$x" "d $e"
		put "${base}r$x" "r $e rename me with enough content to be an exact rename"
		put "${base}t$x" "t $e"
	done
	put "eval/testdata/x.golden" "eval"
	put "internal/x/x.go" "package x"
	g add -A
	g commit -q -m base
	g tag v8-base
	for e in "${cls_paths[@]}"; do
		cp=${e#*:}
		base=${cp%0}
		x=$(ext "$e")
		put "${base}a$x" "a $e"
		put "${base}q \"é\"$x" "q $e"
		put "${base}m$x" "m $e changed"
		rm "$repo/${base}d$x"
		git -C "$repo" mv "${base}r$x" "${base}r2$x"
		rm "$repo/${base}t$x"
		ln -s "m$x" "$repo/${base}t$x"
	done
	put "eval/testdata/x.golden" "eval changed"
	put "internal/x/x.go" "package x // changed"
	g add -A
	g commit -q -m change
	g tag v8-head
	put "internal/tui/testdata/only-added.golden" "only added"
	g add -A
	g commit -q -m add-only
	g tag v8-addonly

	ck() { # ck <name> <condition…>
		local n=$1
		shift
		if "$@"; then echo "V8_SELFTEST ok $n"; else
			echo "V8_SELFTEST FAIL $n"
			rc=1
		fi
	}
	out=$(CRAZE_VERIFY_REPO=$repo "$CV_DIR/v8.sh" --base v8-base --head v8-head 2>&1)
	local orc=$?
	has() { grep -qF -- "$1" <<<"$out"; }
	ck "exit 0 without --expect-none-modified" test "$orc" -eq 0
	for e in "${cls_paths[@]}"; do
		local c=${e%%:*}
		cp=${e#*:}
		base=${cp%0}
		x=$(ext "$e")
		ck "$c A ${base}a$x" has "$(printf '%-8s %-7s %s' ADDED "$c" "${base}a$x")"
		ck "$c A quoted name" has "$(printf '%-8s %-7s %s' ADDED "$c" "${base}q \"é\"$x")"
		ck "$c M ${base}m$x" has "$(printf '%-8s %-7s %s' MODIFIED "$c" "${base}m$x")"
		ck "$c T ${base}t$x" has "$(printf '%-8s %-7s %s (type change)' MODIFIED "$c" "${base}t$x")"
		ck "$c D ${base}d$x" has "$(printf '%-8s %-7s %s' DELETED "$c" "${base}d$x")"
		ck "$c R ${base}r$x" has "$(printf '%-8s %-7s %s -> %s' RENAMED "$c" "${base}r$x" "${base}r2$x")"
		if [[ $c == wire || $c == schema ]]; then
			ck "$c WIRE-MOVED for M" has "WIRE-MOVED: needs owner approval: ${base}m$x"
		fi
	done
	ck "eval/ excluded" bash -c '! grep -q "eval/" <<<"$1"' _ "$out"
	ck "non-fixture .go excluded" bash -c '! grep -q "internal/x/x.go" <<<"$1"' _ "$out"
	# 7 paths x (a, q) added; x (m, t) modified; x d deleted; x r renamed; the
	# wire and schema rows (4 paths) each move m, t, d and r.
	ck "counts" has "V8 added=14 modified=14 deleted=7 renamed=7 wire_or_schema_modified=16"
	CRAZE_VERIFY_REPO=$repo "$CV_DIR/v8.sh" --base v8-base --head v8-head --expect-none-modified >/dev/null 2>&1
	ck "--expect-none-modified exits 1 on a move" test $? -eq 1
	out=$(CRAZE_VERIFY_REPO=$repo "$CV_DIR/v8.sh" --base v8-head --head v8-addonly --expect-none-modified 2>&1)
	orc=$?
	ck "--expect-none-modified exits 0 on additions only" test "$orc" -eq 0
	ck "additions-only counts" has "V8 added=1 modified=0 deleted=0 renamed=0 wire_or_schema_modified=0"
	out=$(CRAZE_VERIFY_REPO=$repo "$CV_DIR/v8.sh" --base v8-head --head v8-head --expect-none-modified 2>&1)
	ck "no change is 0" has "V8 added=0 modified=0 deleted=0 renamed=0 wire_or_schema_modified=0"
	echo "V8_SELFTEST $([[ $rc == 0 ]] && echo PASS || echo FAIL)"
	return "$rc"
}

if cv_wants_help "$@"; then usage; exit 0; fi
cv_setup V8
base=origin/main head=HEAD expect=0 self=0
while (($#)); do
	case $1 in
	--base) (($# >= 2)) || die 2 "--base needs a value"; base=$2; shift 2 ;;
	--head) (($# >= 2)) || die 2 "--head needs a value"; head=$2; shift 2 ;;
	--expect-none-modified) expect=1; shift ;;
	--selftest) self=1; shift ;;
	*) usage >&2; die 2 "unknown option: $1" ;;
	esac
done
if ((self)); then
	selftest
	exit $?
fi
v8_run "$base" "$head" "$expect"
exit $?
