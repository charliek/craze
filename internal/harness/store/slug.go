package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// maxSlugBytes bounds a workspace directory's name well under NAME_MAX (255
// bytes on Linux and macOS), whatever the workspace path's length.
const maxSlugBytes = 200

// slugSeparators are the characters pi's rule turns into '-': both path
// separators, so a Windows-style path slugs the same way on any host, and
// the drive colon.
var slugSeparators = strings.NewReplacer("/", "-", `\`, "-", ":", "-")

// Slug is the directory name a workspace's sessions live under, pi's rule
// (plan 018 §3.6): strip one leading separator, turn every '/', '\' and ':'
// into '-', and wrap the result in "--…--", so /home/me/src/app becomes
// --home-me-src-app--.
//
// Slugs are not unique: /a/b-c and /a/b/c both become --a-b-c--, so a reader
// looking for one workspace's sessions (H7's resume) must also match the
// header's cwd. A slug longer than 200 bytes keeps its "--" wrapping, is cut
// at a UTF-8 boundary, and gains '-' and the first 8 hex chars of the full
// slug's SHA-256, so two long workspaces that share a prefix still differ.
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

// fileStampLayout is the file name's UTC timestamp, yyyymmddThhmmssZ: sorts
// by time, and has no ':' for filesystems that refuse one.
const fileStampLayout = "20060102T150405Z"

// sessionPath is where a session's transcript lives:
// <home>/sessions/<slug>/<UTC stamp>_<session id>.jsonl (plan 018 §3.2).
func sessionPath(home, cwd, id string, started time.Time) string {
	return filepath.Join(home, "sessions", Slug(cwd), started.UTC().Format(fileStampLayout)+"_"+id+".jsonl")
}

// PlanPath is where a session's plan file lives: the transcript's sibling,
// <UTC stamp>_<session id>.plan.md (plan 023 §3.2). It is under the harness
// home rather than in the workspace, so plan mode leaves nothing in the
// repository and the file tools' confinement never sees it; the model is told
// the absolute path in every plan-mode reminder.
func PlanPath(sessionPath string) string {
	return strings.TrimSuffix(sessionPath, ".jsonl") + ".plan.md"
}

// CreatePlanFile creates path empty when nothing is there, with the
// transcript's own permissions — 0600 under 0700 directories, since a plan is
// written from the user's work — and does nothing at all when the file
// exists: it is never truncated, and craze never deletes it. The harness
// calls it when plan mode is first entered in a session, so that the model,
// the plan tool and the reminder all name a file that is there (plan 023
// §3.2).
func CreatePlanFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case errors.Is(err, fs.ErrExist):
		return nil
	case err != nil:
		return fmt.Errorf("store: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: %w", err)
	}
	return nil
}
