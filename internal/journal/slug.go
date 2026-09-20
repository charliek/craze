package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// This file duplicates internal/harness/store's slug on purpose. depguard
// denies the harness every craze import, so the one algorithm cannot live in
// a helper both packages share, and .golangci.yml is not loosened for it.
// Two copies can drift, so slug_parity_test.go runs both over the fixtures in
// testdata/slug_fixtures.json and requires the same bytes from each: a
// journal directory and the native store's directory for one workspace
// always carry the same name, so the two rails pair by eye (roadmap SQ1).

// maxSlugBytes bounds a workspace directory's name well under NAME_MAX (255
// bytes on Linux and macOS), whatever the workspace path's length.
const maxSlugBytes = 200

// slugSeparators are the characters the rule turns into '-': both path
// separators, so a Windows-style path slugs the same way on any host, and
// the drive colon.
var slugSeparators = strings.NewReplacer("/", "-", `\`, "-", ":", "-")

// Slug is the directory name a workspace's journals live under, the harness
// store's rule exactly: strip one leading separator, turn every '/', '\' and
// ':' into '-', and wrap the result in "--…--", so /home/me/src/app becomes
// --home-me-src-app--.
//
// Slugs are not unique: /a/b-c and /a/b/c both become --a-b-c--, so a reader
// looking for one workspace's journals must also match the header's cwd. A
// slug longer than 200 bytes keeps its "--" wrapping, is cut at a UTF-8
// boundary, and gains '-' and the first 8 hex chars of the full slug's
// SHA-256, so two long workspaces that share a prefix still differ.
func Slug(cwd string) string {
	body := strings.TrimPrefix(cwd, "/")
	if body == cwd {
		body = strings.TrimPrefix(cwd, `\`)
	}
	body = slugSeparators.Replace(body)
	slug := "--" + body + "--"
	if len(slug) <= maxSlugBytes {
		return slug
	}
	sum := sha256.Sum256([]byte(slug))
	suffix := "-" + hex.EncodeToString(sum[:4]) + "--"
	keep := maxSlugBytes - len("--") - len(suffix)
	for keep > 0 && !utf8.RuneStart(body[keep]) {
		keep--
	}
	return "--" + body[:keep] + suffix
}

// fileStampLayout is the file name's UTC timestamp, yyyymmddThhmmssZ, the
// store's layout: it sorts by time and has no ':' for filesystems that
// refuse one.
const fileStampLayout = "20060102T150405Z"

// journalPath is where one incarnation's journal lives:
// <dir>/<cwd slug>/<UTC stamp>_<incarnation>.jsonl (plan 020 §3.4).
func journalPath(dir, cwd, incarnation string, started time.Time) string {
	return filepath.Join(dir, Slug(cwd), started.UTC().Format(fileStampLayout)+"_"+incarnation+".jsonl")
}
