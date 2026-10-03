package chatgptauth_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/signinlog"
)

// The sign-in log is value-free (plan 034 A11, Q7). Every emission point —
// the A10 table (chatgptauth.RunEventScenarios) — runs with its events
// recorded in a sign-in log as they happen, and again, replayed with events
// built by hand from the same fixture values, into a log small enough to
// rotate; then every file is scanned, byte for byte, for every fixture value
// and its percent-encodings. The hand-built events put a value in every field
// an Event has, and each such field must be written "invalid".

// TestSignInLogIsValueFree is A11's Go half (tests/cli's test_auth.py is the
// other, over the craze binary).
func TestSignInLogIsValueFree(t *testing.T) {
	live := filepath.Join(t.TempDir(), "native")
	lg, err := signinlog.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []chatgptauth.Event
	values := chatgptauth.RunEventScenarios(t, func(ev chatgptauth.Event) {
		lg.Record(ev, signinlog.SurfaceCLI)
		mu.Lock()
		seen = append(seen, ev)
		mu.Unlock()
	})
	if err := lg.Close(); err != nil {
		t.Fatal(err)
	}
	if t.Failed() {
		return
	}
	values = distinct(values)
	if len(values) < 50 {
		t.Fatalf("the scenarios used %d fixture values; the scan would prove little", len(values))
	}

	// Events built by hand, each field holding a fixture value, or a number
	// out of its range.
	var forged []chatgptauth.Event
	for _, v := range values[:12] {
		forged = append(forged, chatgptauth.Event{
			Kind: chatgptauth.EventKind(v), Attempt: v, Step: chatgptauth.Step(v), Status: 99999, Code: v,
			Class: chatgptauth.Class(v), Mode: chatgptauth.Mode(v), Reason: chatgptauth.Reason(v), Port: 70000,
			Refusal: chatgptauth.Refusal(v), Part: chatgptauth.Part(v), Check: chatgptauth.Check(v), Via: chatgptauth.Via(v),
			Usage: chatgptauth.Usage(v), Registration: chatgptauth.Registration(v), Models: -1, ClientVersion: v,
			Elapsed: -time.Hour, Count: -3,
		})
	}
	all := append(slices.Clone(seen), forged...)
	sizing := filepath.Join(t.TempDir(), "native")
	replay(t, sizing, all, signinlog.Options{})
	size := fileSize(t, logFile(sizing, signinlog.FileName))
	rotated := filepath.Join(t.TempDir(), "native")
	replay(t, rotated, all, signinlog.Options{Max: size * 2 / 3})

	files := []string{
		logFile(live, signinlog.FileName),
		logFile(rotated, signinlog.FileName), logFile(rotated, signinlog.FileName+".1"),
	}
	lines := 0
	for _, f := range files[1:] {
		lines += len(readLines(t, f))
	}
	if lines != len(all) {
		t.Fatalf("the rotated log holds %d records of %d: a record was lost across the rotation", lines, len(all))
	}
	if n := len(readLines(t, files[0])); n != len(seen) {
		t.Fatalf("the live log holds %d records; the scenarios reported %d", n, len(seen))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range values {
			for _, form := range []string{v, url.QueryEscape(v), url.PathEscape(v)} {
				if bytes.Contains(b, []byte(form)) {
					t.Fatalf("%s holds fixture value %d (%d bytes), or one of its percent-encodings", filepath.Base(f), i, len(v))
				}
			}
		}
	}

	// Every forged record names its fields, each "invalid".
	forgedLines := 0
	for _, f := range files[1:] {
		for _, line := range readLines(t, f) {
			var rec map[string]any
			if err := json.Unmarshal(line, &rec); err != nil {
				t.Fatalf("a record is not JSON: %v", err)
			}
			if rec["event"] != "invalid" {
				continue
			}
			forgedLines++
			for _, k := range []string{"attempt", "step", "status", "code", "class", "mode", "reason", "port", "refusal",
				"part", "check", "via", "usage", "registration", "models", "client_version", "elapsed_ms", "count"} {
				if rec[k] != "invalid" {
					t.Fatalf("a forged event's %s was written as %v; want invalid", k, rec[k])
				}
			}
		}
	}
	if forgedLines != len(forged) {
		t.Fatalf("%d forged records written as invalid; want %d", forgedLines, len(forged))
	}
}

// replay records evs into dir's sign-in log, in batches of one Log each — the
// way several processes write it — each closed (flushed) before the next, so
// no record is dropped for a full queue.
func replay(t *testing.T, dir string, evs []chatgptauth.Event, o signinlog.Options) {
	t.Helper()
	for i := 0; i < len(evs); i += 40 {
		lg, err := signinlog.OpenWith(dir, o)
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs[i:min(i+40, len(evs))] {
			lg.Record(ev, signinlog.SurfaceTUI)
		}
		if err := lg.Close(); err != nil {
			t.Fatal(err)
		}
		if err := lg.Failure(); err != nil {
			t.Fatal(err)
		}
	}
}

func logFile(native, name string) string { return filepath.Join(native, paths.LogsName, name) }

func fileSize(t *testing.T, p string) int64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Size()
}

// readLines is p's lines, none of them carrying a dropped count.
func readLines(t *testing.T, p string) [][]byte {
	t.Helper()
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out [][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := slices.Clone(sc.Bytes())
		if bytes.Contains(line, []byte(`"dropped"`)) {
			t.Fatalf("%s holds a record after dropped ones", filepath.Base(p))
		}
		out = append(out, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// distinct is vs without repeats or values too short to scan for.
func distinct(vs []string) []string {
	var out []string
	for _, v := range vs {
		if len(v) >= 8 && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
