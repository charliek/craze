package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// A sub-agent's transcript and its header's parent link (plan 026 §3.2, §7
// A1).

// TestHeaderRecordsTheParentLink: a store opened with a caller-supplied id and
// the four parent keys names its file after that id, writes the keys into the
// header, and reads them back. The control is an ordinary session, whose
// header has none of them.
func TestHeaderRecordsTheParentLink(t *testing.T) {
	opts := testOptions(t)
	opts.SessionID = "child-0001"
	opts.ParentSession = "00000000-0000-4000-8000-00000000000a"
	opts.ParentToolCall = "t3.2.1"
	opts.SubagentType = "pr-review-toolkit:code-reviewer"
	opts.PersonaPath = "/plugins/pr-review-toolkit/agents/code-reviewer.md"
	s := newStore(t, opts)
	turn(t, s, "review it", "reviewed", kimi)

	if s.ID() != "child-0001" || !strings.HasSuffix(s.Path(), "_child-0001.jsonl") {
		t.Fatalf("ID %q at %s; want the caller's id in both", s.ID(), s.Path())
	}
	head := firstLine(t, s.Path())
	for _, kv := range []string{
		`"id":"child-0001"`,
		`"parent_session":"00000000-0000-4000-8000-00000000000a"`,
		`"parent_tool_call":"t3.2.1"`,
		`"subagent_type":"pr-review-toolkit:code-reviewer"`,
		`"persona_path":"/plugins/pr-review-toolkit/agents/code-reviewer.md"`,
	} {
		if !strings.Contains(head, kv) {
			t.Errorf("the header lacks %s: %s", kv, head)
		}
	}
	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if tr.Header != s.Header() {
		t.Fatalf("header read back as %+v, want %+v", tr.Header, s.Header())
	}
	if h := tr.Header; h.ParentSession != opts.ParentSession || h.ParentToolCall != opts.ParentToolCall ||
		h.SubagentType != opts.SubagentType || h.PersonaPath != opts.PersonaPath {
		t.Fatalf("the parent link read back as %+v", h)
	}

	plain := newStore(t, testOptions(t))
	turn(t, plain, "q", "a", kimi)
	head = firstLine(t, plain.Path())
	for _, key := range []string{"parent_session", "parent_tool_call", "subagent_type", "persona_path"} {
		if strings.Contains(head, key) {
			t.Errorf("an ordinary session's header has %s: %s", key, head)
		}
	}
}

// TestHeaderWithoutParentKeysLoads (A1): a header written before the parent
// keys existed — a whole transcript, byte for byte what an earlier craze left
// on disk — still loads, with every parent field empty and its entries whole.
func TestHeaderWithoutParentKeysLoads(t *testing.T) {
	old := `{"type":"session","version":1,"id":"5b1c0d8e-2f4a-4c6b-9d7e-1a2b3c4d5e6f",` +
		`"timestamp":"2026-09-18T12:00:00.000Z","cwd":"/work/craze","craze_version":"v0.0.1",` +
		`"system_prompt_sha256":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",` +
		`"tool_profile":"opencode","tools_sha256":"60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752"}`
	tr := mustLoad(t, lines(old, userLine(t, "u1", "", "hello"), replyLine(t, "a1", "u1", "", "hi", kimi)))

	h := tr.Header
	if h.ID != "5b1c0d8e-2f4a-4c6b-9d7e-1a2b3c4d5e6f" || h.ToolProfile != "opencode" || h.CrazeVersion != "v0.0.1" {
		t.Fatalf("the old header read back as %+v", h)
	}
	if h.ParentSession != "" || h.ParentToolCall != "" || h.SubagentType != "" || h.PersonaPath != "" {
		t.Fatalf("an old header grew a parent link: %+v", h)
	}
	if len(tr.Entries) != 2 {
		t.Fatalf("the old transcript loaded %d entries, want 2", len(tr.Entries))
	}
}

// TestSessionIDMustNameAFile: the caller's id is spliced into the file's
// name, so one that would move the file — a separator of either kind — or
// put a control character in it is refused before anything is named. The
// control is an id that names a file.
func TestSessionIDMustNameAFile(t *testing.T) {
	for _, id := range []string{"../escape", "a/b", `a\b`, "a\nb", "a\x00b"} {
		opts := testOptions(t)
		opts.SessionID = id
		if s, err := New(opts); err == nil {
			t.Errorf("New with session id %q = %s, want a refusal", id, s.Path())
		}
	}
	opts := testOptions(t)
	opts.SessionID = "child-0002"
	s := newStore(t, opts)
	if filepath.Dir(s.Path()) != filepath.Join(opts.Home, "sessions", "--work-craze--") {
		t.Fatalf("control: the session is filed at %s", s.Path())
	}
}
