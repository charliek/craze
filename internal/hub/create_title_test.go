package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/fakehost"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/rundir"
)

// session.create's title (plan 043 D8, P1, P9, P11): normalized once, hashed
// as normalized and only when present, handed to the host it spawns, and
// served only by a hub whose hosts take one — which its hello says.

// titleCreates is creates("cursor") for a hub whose hosts take a title: every
// craze hub's (internal/cli's hubCreates).
func titleCreates() *Creates {
	c := creates("cursor")
	c.Title = true
	return c
}

// legacyCreateHash is createHash exactly as every hub before plan 043 had it,
// copied from e5eb4ca (plan 043 C3, before this change), frozen: what a host
// an older hub spawned carries as its registry entry's requestHash.
func legacyCreateHash(q protocol.CreateParams) string {
	key := struct {
		Cwd            string                  `json:"cwd"`
		Prompt         string                  `json:"prompt"`
		Provider       string                  `json:"provider"`
		Model          string                  `json:"model"`
		Effort         string                  `json:"effort"`
		Fast           *bool                   `json:"fast"`
		PermissionMode protocol.PermissionMode `json:"permissionMode"`
	}{q.Cwd, q.Prompt, q.Provider, q.Model, q.Effort, q.Fast, q.PermissionMode}
	b, _ := json.Marshal(key)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestCreateHashOfAnUntitledCreateIsTheLegacyHash is P1: an untitled create
// hashes byte for byte as it did before titles, so a create retried across a
// hub's upgrade is not request_conflict. The vectors were computed by HEAD
// e5eb4ca's createHash before this change was made, and are pinned here as
// literals; legacyCreateHash, the frozen copy, agrees with them and with
// createHash on random params. A title moves the hash, and two titles that
// normalize the same are hashed the same — the hub hashes what parse made of
// it.
//
// The negative control: the same key with the title written even when empty
// (no omitempty) does NOT hash to the pinned vector — so the pin would catch
// the one easy way to break it.
func TestCreateHashOfAnUntitledCreateIsTheLegacyHash(t *testing.T) {
	fast := true
	for _, tc := range []struct {
		q    protocol.CreateParams
		want string
	}{
		{protocol.CreateParams{Cwd: "/w", PermissionMode: protocol.PermissionBypass},
			"sha256:4944e026921eadbc21a91fea5227b9f46c727521d0f8c6579c33841f0f95ff34"},
		{protocol.CreateParams{Cwd: "/home/me/projects/lumen", Prompt: "hello from the hub", Provider: "grok", Model: "grok-4",
			Effort: "high", Fast: &fast, PermissionMode: protocol.PermissionPrompt},
			"sha256:a94028f745efd3ebc1e32bd986fb9229c4fbf2b7210815123f3fb9c6e98d597c"},
	} {
		if got := createHash(tc.q); got != tc.want {
			t.Fatalf("createHash(%+v) = %s, want the pinned legacy %s", tc.q, got, tc.want)
		}
		if got := legacyCreateHash(tc.q); got != tc.want {
			t.Fatalf("the frozen legacy hash of %+v = %s, want %s", tc.q, got, tc.want)
		}
		titled := tc.q
		titled.Title = "a title"
		if createHash(titled) == tc.want {
			t.Fatalf("a titled create hashes as its untitled twin: %+v", titled)
		}
		// The negative control: a key that writes the title whatever it is.
		key := struct {
			Cwd            string                  `json:"cwd"`
			Prompt         string                  `json:"prompt"`
			Provider       string                  `json:"provider"`
			Model          string                  `json:"model"`
			Effort         string                  `json:"effort"`
			Fast           *bool                   `json:"fast"`
			PermissionMode protocol.PermissionMode `json:"permissionMode"`
			Title          string                  `json:"title"`
		}{tc.q.Cwd, tc.q.Prompt, tc.q.Provider, tc.q.Model, tc.q.Effort, tc.q.Fast, tc.q.PermissionMode, ""}
		b, _ := json.Marshal(key)
		sum := sha256.Sum256(b)
		if "sha256:"+hex.EncodeToString(sum[:]) == tc.want {
			t.Fatal("a key that always writes the title hashes as the legacy one: the pin cannot tell them apart")
		}
	}
	cr := newCreator(&hub{}, *titleCreates())
	a, perr := cr.parse(json.RawMessage(`{"cwd":"/w","title":"  Fix  the\tbuild "}`))
	b, perr2 := cr.parse(json.RawMessage(`{"cwd":"/w","title":"Fix the build"}`))
	if perr != nil || perr2 != nil || a.hash != b.hash || a.p.Title != "Fix the build" {
		t.Fatalf("two titles that normalize the same: %+v (%v), %+v (%v)", a, perr, b, perr2)
	}
	untitled, perr := cr.parse(json.RawMessage(`{"cwd":"/w"}`))
	if perr != nil || untitled.hash != legacyCreateHash(untitled.p) || untitled.hash == a.hash {
		t.Fatalf("an untitled create through parse: %+v (%v)", untitled, perr)
	}
}

// TestCreateTitleNormalization is plan 043 §5 C4's normalization table
// through the hub's parse — engine's TestNormalizeTitle's very inputs and
// outputs (titleCases there), so the hub, craze serve --title and craze new
// --title (internal/cli's TestTitleFlagsNormalizeAsTheHubDoes) make the same
// title of each: kept as normalized, or refused bad_request where it comes to
// nothing, nothing spawned. The negative control is "plain", kept as given.
func TestCreateTitleNormalization(t *testing.T) {
	cr := newCreator(&hub{}, *titleCreates())
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "Fix the login bug", "Fix the login bug"},
		{"empty", "", ""},
		{"whitespace only", " \t\n  ", ""},
		{"control characters only", "\x00\x07\x1b", ""},
		{"padded", "  padded title  ", "padded title"},
		{"inner runs collapse", "a  b\t\tc\n\nd", "a b c d"},
		{"control characters dropped", "bell\x07 and \x00nul", "bell and nul"},
		{"an escape sequence dropped", "\x1b[31mred\x1b[0m title", "red title"},
		{"an OSC title sequence dropped", "\x1b]0;evil\x07kept", "kept"},
		{"unicode kept", "日本語のタイトル — café ✓", "日本語のタイトル — café ✓"},
		{"120 runes kept", strings.Repeat("é", 120), strings.Repeat("é", 120)},
		{"121 runes cut to 120", strings.Repeat("日", 121), strings.Repeat("日", 120)},
		{"a cut after a space trimmed", strings.Repeat("a b ", 31), strings.TrimSpace(strings.Repeat("a b ", 30))},
		{"a zero-width space only", "\u200b", ""},
		{"a zero-width space inside", "A\u200bB", "AB"},
		{"a BOM and a word joiner", "\ufeffTi\u2060tle", "Title"},
		{"bidi overrides and isolates", "\u202eevil\u202c and \u2066isolate\u2069", "evil and isolate"},
		{"invalid UTF-8 replaced", "bad\xffbyte", "bad\ufffdbyte"},
		{"only stripped runes", "\u200b\u202e\ufeff\u2066\u2069\u2060", ""},
		{"a carriage return dropped", "a\rb", "ab"},
		{"a C1 CSI dropped with its sequence", "\u009b31mred", "red"},
		{"ZWJ and ZWNJ kept", "a\u200db\u200cc", "a\u200db\u200cc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := engine.NormalizeTitle(tc.in); got != tc.want {
				t.Fatalf("the table is engine's: NormalizeTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
			raw, _ := json.Marshal(map[string]any{"cwd": "/w", "title": tc.in})
			p, perr := cr.parse(raw)
			if tc.want == "" {
				if perr == nil || perr.Data.Code != protocol.CodeBadRequest || perr.Data.Reason != protocol.ReasonBadRequest {
					t.Fatalf("a title of %q parsed as %+v (%v), want bad_request", tc.in, p, perr)
				}
				return
			}
			if perr != nil || p.p.Title != tc.want {
				t.Fatalf("a title of %q parsed as %q (%v), want %q", tc.in, p.p.Title, perr, tc.want)
			}
		})
	}
	// A title of another type is refused as any other member of the wrong
	// type is.
	for _, raw := range []string{`{"cwd":"/w","title":null}`, `{"cwd":"/w","title":7}`} {
		if _, perr := cr.parse(json.RawMessage(raw)); perr == nil || perr.Data.Code != protocol.CodeBadRequest {
			t.Fatalf("%s parsed, %v", raw, perr)
		}
	}
}

// TestACreateWithATitle (A12's titled half, plan 043 T5's hub half): a hub
// whose hosts take a title says createTitle in its hello; a titled create
// hands its host the normalized title (--title), and the roster row it
// answers already carries it, read fresh from the host — which seeded it
// before it served. A repeat whose title normalizes the same joins it, one
// spawn in all; another title, or none, under the same requestId is
// request_conflict — and so is a title on the repeat of an untitled create.
// The negative control is the untitled create beside it, whose row has no
// title and whose command line no --title.
func TestACreateWithATitle(t *testing.T) {
	env := testEnv(t)
	s := hostsAsChildren(t, env, always("ok"))
	rn := runWith(t, env, nil, titleCreates())
	sock := rn.line(t).Socket
	rn.serving(t)
	c := dial(t, sock)
	if res := c.hello(t); !res.Capabilities.SessionCreate || !res.Capabilities.CreateTitle {
		t.Fatalf("a hub whose hosts take a title says %+v", res.Capabilities)
	}
	work := t.TempDir()
	// A zero-width space and a bidi isolate the session's scrub would drop:
	// dropped here, before the hash, so the plain spelling below is the same
	// create (review C7).
	first := createOn(t, c, map[string]any{"cwd": work, "requestId": "r-t", "title": "  Fix the\u200b\tlogin  bug\u2066 "})
	res := result(t, first)
	srow, ok, err := res.Session.SessionRow()
	if !ok || err != nil || srow.Title != "Fix the login bug" {
		t.Fatalf("the titled session's first row: %+v, %v, %v", srow, ok, err)
	}
	_, argv := s.cmd(0)
	if !slices.Contains(argv, "--title=Fix the login bug") || fakehost.ParseSpawnArgs(argv).Title != "Fix the login bug" {
		t.Fatalf("the host's command line %q lacks the normalized --title", argv)
	}
	again := createOn(t, c, map[string]any{"cwd": work, "requestId": "r-t", "title": "Fix the login bug"})
	if string(again.Result) != string(first.Result) || s.count() != 1 {
		t.Fatalf("the same title, spaced otherwise, answered %s (first %s) after %d spawns", again.Result, first.Result, s.count())
	}
	refusedAs(t, createOn(t, c, map[string]any{"cwd": work, "requestId": "r-t", "title": "Another title"}),
		protocol.CodeBadRequest, protocol.ReasonRequestConflict)
	refusedAs(t, createOn(t, c, map[string]any{"cwd": work, "requestId": "r-t"}),
		protocol.CodeBadRequest, protocol.ReasonRequestConflict)

	// The negative control: untitled, and a title on its repeat.
	plain := result(t, createOn(t, c, map[string]any{"cwd": work, "requestId": "r-u"}))
	prow, ok, err := plain.Session.SessionRow()
	if !ok || err != nil || prow.Title != "" {
		t.Fatalf("the untitled session's row: %+v, %v, %v", prow, ok, err)
	}
	if _, argv := s.cmd(1); slices.ContainsFunc(argv, func(a string) bool { return strings.HasPrefix(a, "--title") }) {
		t.Fatalf("an untitled create passes a title: %q", argv)
	}
	refusedAs(t, createOn(t, c, map[string]any{"cwd": work, "requestId": "r-u", "title": "Late title"}),
		protocol.CodeBadRequest, protocol.ReasonRequestConflict)
	if n := s.count(); n != 2 {
		t.Fatalf("%d spawns, want the two creates'", n)
	}
}

// TestATitledCreateWhoseStartFails: a titled create whose session's start
// fails is start_failed with its cause, as an untitled one is
// (TestCreateStartFailureLeavesNoHost) — the title changes nothing of it — and
// its host is gone.
func TestATitledCreateWhoseStartFails(t *testing.T) {
	env := testEnv(t)
	s := hostsAsChildren(t, env, always("fail"))
	rn := runWith(t, env, nil, titleCreates())
	sock := rn.line(t).Socket
	rn.serving(t)
	c := dial(t, sock)
	c.hello(t)
	e := refusedAs(t, createOn(t, c, map[string]any{"cwd": t.TempDir(), "title": "Never started"}),
		protocol.CodeNotAccepting, protocol.ReasonStartFailed)
	if e.Data.Cause != failCause {
		t.Fatalf("the start failure's cause is %q", e.Data.Cause)
	}
	if _, argv := s.cmd(0); !slices.Contains(argv, "--title=Never started") {
		t.Fatalf("the host's command line %q lacks its title", argv)
	}
	s.gone(t, env, 0)
}

// TestARetryAcrossAnUpgradeJoinsItsSession is P1 through the registry: a
// session an older hub created — one whose hosts take no title, as every hub
// before plan 043 — under a requestId, its host's entry carrying the legacy
// hash (legacyCreateHash, the frozen pre-change code), is joined by an
// upgraded hub retrying the same untitled create after a restart: its row,
// prompt unknown, nothing spawned again. The same id with a title is
// request_conflict there (the negative control: the registry's hash is
// compared, and a title moves it).
func TestARetryAcrossAnUpgradeJoinsItsSession(t *testing.T) {
	env := testEnv(t)
	s := hostsAsChildren(t, env, always("ok"))
	older := runWith(t, env, nil, creates("cursor"))
	c := dial(t, older.line(t).Socket)
	if res := c.hello(t); res.Capabilities.CreateTitle {
		t.Fatalf("a hub whose hosts take no title says createTitle: %+v", res.Capabilities)
	}
	work := t.TempDir()
	p := map[string]any{"cwd": work, "prompt": "hi", "requestId": "r-up"}
	res := result(t, createOn(t, c, p))
	e, listed := listedEntry(t, env, res.Session.HostID)
	want := legacyCreateHash(protocol.CreateParams{Cwd: work, Prompt: "hi", PermissionMode: protocol.PermissionBypass})
	if !listed || e.RequestID != "r-up" || e.RequestHash != want {
		t.Fatalf("the older hub's host is listed %v with request %q %q, want the legacy hash %s", listed, e.RequestID, e.RequestHash, want)
	}
	older.sigs <- syscall.SIGTERM
	if err := older.stopped(t); err != nil {
		t.Fatal(err)
	}

	upgraded := runWith(t, env, nil, titleCreates())
	c2 := dial(t, upgraded.line(t).Socket)
	if res := c2.hello(t); !res.Capabilities.CreateTitle {
		t.Fatalf("the upgraded hub says %+v", res.Capabilities)
	}
	refusedAs(t, createOn(t, c2, map[string]any{"cwd": work, "prompt": "hi", "requestId": "r-up", "title": "Named late"}),
		protocol.CodeBadRequest, protocol.ReasonRequestConflict)
	joined := result(t, createOn(t, c2, p))
	if joined.Session.HostID != res.Session.HostID || joined.Prompt != protocol.CreatePromptUnknown || s.count() != 1 {
		t.Fatalf("the retry across the upgrade answered %+v (first %+v) after %d spawns", joined, res, s.count())
	}
}

// TestAHubWhoseHostsTakeNoTitleRefusesOne is the older hub a client meets
// (P11), on the real hub: Creates.Title false says no createTitle in its
// hello — the member absent, not false — and refuses a create naming a title
// unknown_field, spawning nothing, as a hub from before titles does; Create
// (the client) asked for a titled session there is LacksError's "set a title
// at create" without sending anything. The negative control: the same hub
// creates an untitled session, through Create too.
func TestAHubWhoseHostsTakeNoTitleRefusesOne(t *testing.T) {
	env := testEnv(t)
	s := hostsAsChildren(t, env, always("ok"))
	rn := runWith(t, env, nil, creates("cursor"))
	sock := rn.line(t).Socket
	rn.serving(t)
	c := dial(t, sock)
	resp := c.call(t, protocol.MethodHello, protocol.HelloParams{Protocols: []int{1}, Client: protocol.ClientInfo{Kind: "test"}})
	if resp.Error != nil || strings.Contains(string(resp.Result), "createTitle") {
		t.Fatalf("hello = %s (%v), want no createTitle member", resp.Result, resp.Error)
	}
	work := t.TempDir()
	refusedAs(t, createOn(t, c, map[string]any{"cwd": work, "title": "A title"}), protocol.CodeBadRequest, protocol.ReasonUnknownField)
	ctx, cancel := context.WithTimeout(context.Background(), step)
	defer cancel()
	_, err := Create(ctx, sock, protocol.ClientInfo{Kind: "test"}, protocol.CreateParams{Cwd: work, Title: "A title"})
	var lacks *LacksError
	if !errors.As(err, &lacks) || !slices.Equal(lacks.Missing, []string{"set a title at create"}) ||
		err.Error() != lacksWords(lacks.Version, "set a title at create", os.Getpid()) {
		t.Fatalf("Create of a titled session on a hub that takes none = %v; want LacksError", err)
	}
	if n := s.count(); n != 0 {
		t.Fatalf("a refused titled create spawned %d hosts", n)
	}
	raw, err := Create(ctx, sock, protocol.ClientInfo{Kind: "test"}, protocol.CreateParams{Cwd: work})
	if err != nil || len(raw) == 0 || s.count() != 1 {
		t.Fatalf("an untitled Create on the same hub: %s, %v, after %d spawns", raw, err, s.count())
	}
}

// TestEnsureReportsAHubThatTakesNoTitle is P11 at Ensure (§3.8 step 1): a
// running hub without createTitle, asked for by craze new --title
// (CreateNeed), is reported in LacksError's words — "cannot set a title at
// create" — not used, not replaced and not signalled; asked for an untitled
// create (the negative control) it is used.
func TestEnsureReportsAHubThatTakesNoTitle(t *testing.T) {
	env := processEnv(t)
	noCommand(t)
	pid := sleeper(t)
	hubID := rundir.NewHubID()
	d := newDecoy(t, decoyAnswer, hubResult(hubID, "0.9.1", protocol.HubCapabilities()))
	writeRecord(t, env, rundir.HubRecord{HubID: hubID, PID: pid, StartToken: token(t, pid), Socket: d.path})
	need := CreateNeed(protocol.CreateParams{Cwd: "/w", Title: "A title"})
	if !need.SessionCreate || !need.CreateTitle {
		t.Fatalf("a titled create needs %+v", need)
	}
	_, err := ensure(t, env, need)
	var lacks *LacksError
	if !errors.As(err, &lacks) || err.Error() != lacksWords("0.9.1", "set a title at create", 1) {
		t.Fatalf("Ensure for a titled create = %v; want the older hub reported", err)
	}
	if !alive(t, pid) {
		t.Fatal("Ensure signalled an older hub")
	}
	sock, err := ensure(t, env, CreateNeed(protocol.CreateParams{Cwd: "/w"}))
	if err != nil || sock != d.path {
		t.Fatalf("Ensure for an untitled create = %q, %v; want the older hub, which can", sock, err)
	}
}
