#!/usr/bin/env bash
# update-version.sh — mechanical bump of craze's version manifests.
#
# internal/version/version.go and .claude-plugin/plugin.json are the version
# manifests (see RELEASING.md).
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

# The Claude Code plugin's manifest carries the same version (plan 041 §3.6).
# jq rewrites it through a temp file and mv, as prox's bump does; the
# top-level .version is read back, so a manifest that lost the field fails the
# release here instead of shipping a stale version.
plugin_file=".claude-plugin/plugin.json"

if [ ! -f "$plugin_file" ]; then
  echo "error: ${plugin_file} not found — run this from the repo root" >&2
  exit 1
fi

plugin_tmp="$(mktemp)"
trap 'rm -f "$tmp" "$plugin_tmp"' EXIT
jq --arg v "$version" '.version = $v' "$plugin_file" >"$plugin_tmp"
# mktemp makes the file 0600; the manifest is a tracked 0644 file.
chmod 644 "$plugin_tmp"
mv "$plugin_tmp" "$plugin_file"

plugin_actual="$(jq -r '.version' "$plugin_file")"
if [ "$plugin_actual" != "$version" ]; then
  echo "error: ${plugin_file} top-level .version is '${plugin_actual}' after bump, expected '${version}'" >&2
  exit 1
fi

echo "$plugin_file"
