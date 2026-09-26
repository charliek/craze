package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// sessionIn writes one turn of a session with id in workspace cwd under home,
// closes it, and returns its store.
func sessionIn(t *testing.T, home, cwd, id string) *Store {
	t.Helper()
	opts := testOptions(t)
	opts.Home, opts.Workspace, opts.SessionID = home, cwd, id
	s := newStore(t, opts)
	turn(t, s, "q", "a", kimi)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestFindMatchesTheHeaderCwd (A4): /work/a-b and /work/a/b share a slug, so
// their sessions share a directory, and Find tells them apart by the cwd in
// each file's header: a session is found from its own workspace, and asked
// for from the other it is refused as belonging elsewhere — never taken for
// that workspace's, and never "not there", which would open an empty one
// under its id.
func TestFindMatchesTheHeaderCwd(t *testing.T) {
	home := filepath.Join(t.TempDir(), "native")
	const dash, slash = "/work/a-b", "/work/a/b"
	if Slug(dash) != Slug(slash) {
		t.Fatalf("control: %s and %s have different slugs", dash, slash)
	}
	s1 := sessionIn(t, home, dash, "00000000-0000-4000-8000-00000000000a")
	s2 := sessionIn(t, home, slash, "00000000-0000-4000-8000-00000000000b")
	if filepath.Dir(s1.Path()) != filepath.Dir(s2.Path()) {
		t.Fatal("control: the two sessions are not in one directory")
	}
	for _, tc := range []struct {
		cwd string
		s   *Store
	}{{dash, s1}, {slash, s2}, {slash + "/", s2}} {
		got, err := Find(home, tc.cwd, tc.s.ID())
		if err != nil || got != tc.s.Path() {
			t.Fatalf("Find(%s, %s) = %q, %v; want %s", tc.cwd, tc.s.ID(), got, err, tc.s.Path())
		}
	}
	_, err := Find(home, slash, s1.ID())
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "session "+s1.ID()+"'s file belongs to "+dash) {
		t.Fatalf("Find of %s's session from %s = %v, want ErrCorrupt naming %s", dash, slash, err, dash)
	}
	// What Find returns, Open opens, given the same id and workspace; and
	// Open makes the same check Find does.
	opts := testOptions(t)
	opts.Workspace, opts.SessionID = slash, s2.ID()
	r, err := Open(opts, s2.Path())
	if err != nil {
		t.Fatalf("Open of what Find found: %v", err)
	}
	_ = r.Close()
	opts.Workspace = dash
	if _, err := Open(opts, s2.Path()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open of %s's session as %s's = %v, want ErrCorrupt", slash, dash, err)
	}
}

// TestFindRejectsABadID (A4): an id that is empty, would move the name it is
// matched in (a separator), holds a control character, or would mean
// something to a glob is ErrBadSessionID, before any directory is read. The
// control: a good id that is not there is ErrNoTranscript.
func TestFindRejectsABadID(t *testing.T) {
	home := filepath.Join(t.TempDir(), "native")
	sessionIn(t, home, testWorkspace, "00000000-0000-4000-8000-000000000001")
	for _, id := range []string{"", "../x", "a/b", `a\b`, "*", "0000*", "a?", "[ab]", "a\nb", "a\x00b", "a\tb"} {
		if _, err := Find(home, testWorkspace, id); !errors.Is(err, ErrBadSessionID) {
			t.Errorf("Find(%q) = %v, want ErrBadSessionID", id, err)
		}
	}
	if _, err := Find(home, testWorkspace, "00000000-0000-4000-8000-000000000002"); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("control: Find of an absent good id = %v, want ErrNoTranscript", err)
	}
}

// TestAChildIsNotResumable (A4): a sub-agent's transcript is never reopened:
// Find and Open both refuse it, and Open leaves it as it was.
func TestAChildIsNotResumable(t *testing.T) {
	opts := testOptions(t)
	opts.ParentSession, opts.ParentToolCall, opts.SubagentType = "00000000-0000-4000-8000-0000000000ff", "t1.1.1", "explore"
	s := newStore(t, opts)
	turn(t, s, "look around", "done", kimi)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(s.Path())
	if _, err := Find(opts.Home, opts.Workspace, s.ID()); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("Find of a child = %v, want ErrNotResumable", err)
	}
	if _, err := Open(opts, s.Path()); !errors.Is(err, ErrNotResumable) {
		t.Fatalf("Open of a child = %v, want ErrNotResumable", err)
	}
	if after, _ := os.ReadFile(s.Path()); string(after) != string(before) {
		t.Fatal("a refused Open changed the child's transcript")
	}
}

// TestFindErrors: ErrNoTranscript only when no file in the directory is named
// for the session — the directory missing included; a file named for it that
// is not its transcript is never "not there" (plan 028 R2-15). The files
// beside a transcript (its plan, a torn copy, a leftover temporary file) are
// not candidates, and another session's own file, whose id ends in _<id>, is
// passed over. Anything named for the session that is not a regular file —
// a directory, a named pipe (never opened for reading, which would block), a
// link to either — is ErrCorrupt, naming it.
func TestFindErrors(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	home := filepath.Join(t.TempDir(), "native")
	dir := filepath.Join(home, "sessions", Slug(testWorkspace))
	name := "20260918T120000Z_" + id + ".jsonl"
	put := func(t *testing.T, name, body string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clear := func(t *testing.T) {
		t.Helper()
		if err := os.RemoveAll(home); err != nil {
			t.Fatal(err)
		}
	}
	header := func(id, cwd string) string {
		b, err := encodeHeader(Header{Version: FormatVersion, ID: id, Timestamp: fixedTime, Cwd: cwd})
		if err != nil {
			t.Fatal(err)
		}
		return string(b) + "\n"
	}

	if _, err := Find(home, testWorkspace, id); !errors.Is(err, ErrNoTranscript) {
		t.Fatalf("no directory: Find = %v, want ErrNoTranscript", err)
	}
	for what, files := range map[string]map[string]string{
		"only its neighbours": {
			"20260918T120000Z_" + id + ".plan.md":                         "# plan\n",
			"20260918T120000Z_" + id + ".torn-20260918T130000Z":           "{\"type\":\"mess",
			"." + name + ".0badf00d.tmp":                                  header(id, testWorkspace),
			"20260918T120000Z_00000000-0000-4000-8000-000000000009.jsonl": header("00000000-0000-4000-8000-000000000009", testWorkspace),
		},
		"another session whose id ends in _<id>": {"20260918T120000Z_x_" + id + ".jsonl": header("x_"+id, testWorkspace)},
	} {
		t.Run(what, func(t *testing.T) {
			clear(t)
			for n, body := range files {
				put(t, n, body)
			}
			if _, err := Find(home, testWorkspace, id); !errors.Is(err, ErrNoTranscript) {
				t.Fatalf("Find = %v, want ErrNoTranscript", err)
			}
		})
	}
	for what, mk := range map[string]func(path string) error{
		"a directory named like one":  func(path string) error { return os.Mkdir(path, 0o700) },
		"a named pipe named like one": func(path string) error { return syscall.Mkfifo(path, 0o600) },
		"a link to a directory named like one": func(path string) error {
			target := filepath.Join(dir, "elsewhere")
			if err := os.Mkdir(target, 0o700); err != nil {
				return err
			}
			return os.Symlink(target, path)
		},
	} {
		t.Run(what, func(t *testing.T) {
			clear(t)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := mk(filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			_, err := Find(home, testWorkspace, id)
			if !errors.Is(err, ErrCorrupt) || errors.Is(err, ErrNoTranscript) || !strings.Contains(err.Error(), name) {
				t.Fatalf("Find = %v, want ErrCorrupt naming %s", err, name)
			}
		})
	}
	t.Run("a dangling link named like one", func(t *testing.T) {
		clear(t)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "gone"), filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := Find(home, testWorkspace, id); err == nil || errors.Is(err, ErrNoTranscript) {
			t.Fatalf("Find = %v, want an error that is not ErrNoTranscript", err)
		}
	})

	for what, tc := range map[string]struct {
		files map[string]string
		want  error
	}{
		"an empty file":          {map[string]string{name: ""}, ErrNoHeader},
		"a zero-filled file":     {map[string]string{name: "\x00\x00\x00\x00"}, ErrNoHeader},
		"a header that is not":   {map[string]string{name: "{}\n"}, ErrNoHeader},
		"a torn header":          {map[string]string{name: header(id, testWorkspace)[:30]}, ErrNoHeader},
		"another id's header":    {map[string]string{name: header("00000000-0000-4000-8000-000000000009", testWorkspace)}, ErrCorrupt},
		"another cwd's header":   {map[string]string{name: header(id, "/work/elsewhere")}, ErrCorrupt},
		"two files for the id":   {map[string]string{name: header(id, testWorkspace), "20260918T130000Z_" + id + ".jsonl": header(id, testWorkspace)}, ErrCorrupt},
		"a header with no lines": {map[string]string{name: strings.TrimSuffix(header(id, testWorkspace), "\n")}, nil},
	} {
		t.Run(what, func(t *testing.T) {
			clear(t)
			for n, body := range tc.files {
				put(t, n, body)
			}
			got, err := Find(home, testWorkspace, id)
			if !errors.Is(err, tc.want) || (tc.want != nil && errors.Is(err, ErrNoTranscript)) {
				t.Fatalf("Find = %q, %v; want %v", got, err, tc.want)
			}
			if tc.want == nil && got != filepath.Join(dir, name) {
				t.Fatalf("Find = %q, want %s", got, filepath.Join(dir, name))
			}
		})
	}

	for _, bad := range [][2]string{{"native", testWorkspace}, {home, "work/craze"}} {
		if _, err := Find(bad[0], bad[1], id); err == nil || errors.Is(err, ErrNoTranscript) {
			t.Errorf("Find(home %q, cwd %q) = %v, want a refusal of the relative path", bad[0], bad[1], err)
		}
	}
}

// TestReadHeaderReadsOnlyTheHeader: ReadHeader is the header the store wrote,
// read from the first line alone, so damage past it is Open's to find, not
// ReadHeader's; the control is Load of the same file, which refuses it.
func TestReadHeaderReadsOnlyTheHeader(t *testing.T) {
	opts := testOptions(t)
	opts.ToolProfile, opts.Tools = "opencode", []byte(`[{"name":"read"}]`)
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	turn(t, s, "q2", "a2", kimi)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	ls := strings.SplitAfter(string(b), "\n")
	ls[2] = "{not json\n"
	if err := os.WriteFile(s.Path(), []byte(strings.Join(ls, "")), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(s.Path()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("control: Load = %v, want ErrCorrupt", err)
	}
	h, err := ReadHeader(s.Path())
	if err != nil || h != s.Header() {
		t.Fatalf("ReadHeader = %+v, %v; want %+v", h, err, s.Header())
	}
}

// TestFindTakesTheSuffixRule (plan 028 §3.2, astra r1-c0c1): every file whose
// name ends in _<id>.jsonl is a candidate, whatever its stamp holds, and its
// header decides. The session's own header makes it the transcript; another
// session's own file — whose name also ends in _<its id>.jsonl, as a session
// x_<id>'s does — is passed over, so it never poisons the session's lookup;
// any other header is ErrCorrupt, and so are two files of the session's own.
func TestFindTakesTheSuffixRule(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	const other = "00000000-0000-4000-8000-000000000009"
	own := "20260918T120000Z_" + id + ".jsonl"
	odd := "20260918_extra_" + id + ".jsonl"    // a stamp with a '_' in it
	xs := "20260918T120000Z_x_" + id + ".jsonl" // session x_<id>'s own
	for what, tc := range map[string]struct {
		files map[string]string // file name → the id its header names
		want  string            // the file Find takes; "" when it refuses
		err   error
	}{
		"a stamp with an underscore":                   {map[string]string{odd: id}, odd, nil},
		"beside session x_<id>'s own":                  {map[string]string{own: id, xs: "x_" + id}, own, nil},
		"session x_<id>'s own alone":                   {map[string]string{xs: "x_" + id}, "", ErrNoTranscript},
		"named as x_<id>'s, holding the session's":     {map[string]string{xs: id}, xs, nil},
		"two of its own, one with an odd stamp":        {map[string]string{own: id, odd: id}, "", ErrCorrupt},
		"an odd stamp holding another session's":       {map[string]string{odd: other}, "", ErrCorrupt},
		"named as x_<id>'s, holding a third session's": {map[string]string{xs: other}, "", ErrCorrupt},
	} {
		t.Run(what, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "native")
			dir := filepath.Join(home, "sessions", Slug(testWorkspace))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			for name, hid := range tc.files {
				b, err := encodeHeader(Header{Version: FormatVersion, ID: hid, Timestamp: fixedTime, Cwd: testWorkspace})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Find(home, testWorkspace, id)
			if tc.want != "" {
				if err != nil || got != filepath.Join(dir, tc.want) {
					t.Fatalf("Find = %q, %v; want %s", got, err, tc.want)
				}
			} else if !errors.Is(err, tc.err) || (!errors.Is(tc.err, ErrNoTranscript) && errors.Is(err, ErrNoTranscript)) {
				t.Fatalf("Find = %q, %v; want %v", got, err, tc.err)
			}
			// Session x_<id> finds its own file whatever lies beside it.
			if hid, ok := tc.files[xs]; ok && hid == "x_"+id {
				if got, err := Find(home, testWorkspace, "x_"+id); err != nil || got != filepath.Join(dir, xs) {
					t.Fatalf("Find of x_<id> = %q, %v; want %s", got, err, xs)
				}
			}
		})
	}
}
