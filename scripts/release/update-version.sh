#!/usr/bin/env bash
# update-version.sh — mechanical bump of craze's version manifest.
#
# internal/version/version.go IS the version manifest (see RELEASING.md).
# This script is run by the release-workflows:release skill, locally, from
# the repo root, before the version-bump commit is created. It must be
# idempotent, do no network I/O, and never `git add` — the skill stages and
# commits what it modified itself, and aborts the release if the resulting
# commit subject isn't the version bump.
set -euo pipefail

usage() {
  echo "Usage: $0 X.Y.Z[-prerelease]" >&2
}

if [ "$#" -ne 1 ]; then
  usage
  exit 2
fi

version="$1"

if ! [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.-]+)?$ ]]; then
  usage
  exit 2
fi

version_file="internal/version/version.go"

if [ ! -f "$version_file" ]; then
  echo "error: ${version_file} not found — run this from the repo root" >&2
  exit 1
fi

# Exactly one declaration, or the anchored substitution below would rewrite
# several lines and leave the file half-bumped before the readback caught it.
declarations="$(grep -cE '^var Version = "[^"]*"$' "$version_file" || true)"
if [ "$declarations" != "1" ]; then
  echo "error: expected exactly one 'var Version = \"...\"' line in ${version_file}, found ${declarations}" >&2
  exit 1
fi

# Write through a temp file rather than `sed -i`: the -i flag takes an
# argument on BSD sed and none on GNU sed, and craze is a macOS repo now.
tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
sed -E "s/^var Version = \"[^\"]*\"\$/var Version = \"${version}\"/" "$version_file" >"$tmp"
cat "$tmp" >"$version_file"

expected="var Version = \"${version}\""
# `|| true` so a missing line reaches the comparison below with its
# diagnostic, instead of set -e killing the script inside the assignment.
actual="$(grep -E '^var Version = ' "$version_file" || true)"

if [ "$actual" != "$expected" ]; then
  echo "error: ${version_file} does not contain '${expected}' after bump (got: ${actual:-<no match>})" >&2
  exit 1
fi

echo "$version_file"
