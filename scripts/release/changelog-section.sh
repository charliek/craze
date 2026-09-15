#!/usr/bin/env bash
# changelog-section.sh — print one release's section of CHANGELOG.md.
#
# Used by release.yml to build GoReleaser's --release-notes file. Reads
# ./CHANGELOG.md relative to the current working directory (the repo root,
# when run from CI or from a release checkout). Prints nothing and exits 0
# if the tag's section isn't found, so a missing/incomplete CHANGELOG never
# blocks a release — GoReleaser falls back to its own generated notes.
set -euo pipefail

usage() {
  echo "Usage: $0 <tag>" >&2
}

if [ "$#" -ne 1 ]; then
  usage
  exit 2
fi

tag="$1"
changelog="CHANGELOG.md"

if [ ! -f "$changelog" ]; then
  exit 0
fi

awk -v tag="$tag" '
  $1=="##" && $2==tag {flag=1; next}
  /^## v/ {if(flag) exit}
  flag {print}
' "$changelog"
