package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"charm.land/fantasy"
)

// These tests cover the reminder entry (plan 028 §3.15): the mode reminder a
// step's request carried, written at its place in the step's append as its
// variant alone, and rendered back into the context by the harness's
// Renderer.

// testRenderer renders every variant but "unknown" as a user message naming
// it, the way the harness renders one: the store never sees the text.
var testRenderer = Renderer{Reminder: func(v string) (fantasy.Message, bool) {
	if v == "unknown" {
		return fantasy.Message{}, false
	}
	return fantasy.NewUserMessage("<reminder " + v + ">"), true
}}

// reminded is opts with the test renderer.
func reminded(opts Options) Options {
	opts.Render = testRenderer
	return opts
}

// callA and resultA are how messageTexts shows calls(m, "call_a") and its
// results.
const (
	callA   = `assistant: [call call_a read {"filePath":"call_a.go"}]`
	resultA = "tool: [result call_a: body of call_a]"
)

// reminderLead is a lead that is a reminder of variant v.
func reminderLead(v string) Lead { return Lead{Reminder: v} }

// steerLead is a lead that is the steer text.
func steerLead(text string, m Model) Lead { return Lead{Message: user(text, m)} }

// TestReminderEntryRoundTrips: a reminder entry's line is the envelope and the
// variant, nothing else, reads back whole, and is a known type.
func TestReminderEntryRoundTrips(t *testing.T) {
	e := Entry{Type: TypeReminder, ID: "0000000a", ParentID: "00000009", Timestamp: fixedTime, Variant: "plan_full_empty"}
	line, err := encodeEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"type":"reminder","id":"0000000a","parentId":"00000009","timestamp":"2026-09-18T12:00:00.000Z","variant":"plan_full_empty"}`
	if string(line) != want {
		t.Fatalf("reminder line =\n%s\nwant\n%s", line, want)
	}
	back, err := decodeEntry(line)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, e) {
		t.Fatalf("reminder entry read back as %+v, want %+v", back, e)
	}
	if !knownType(TypeReminder) {
		t.Fatal("knownType does not know reminder")
	}
	if _, err := encodeEntry(Entry{Type: TypeReminder, ID: "0000000b", Timestamp: fixedTime, Variant: "Plan mode is active"}); err == nil {
		t.Fatal("a reminder whose variant is text encoded")
	}
}

// TestRemindersAreWrittenWhereTheRequestHadThem: a step's leads are written in
// the order given — after the held changes and user entries, ahead of the
// answer — and the context renders each reminder at its place, byte for byte
// the renderer's message. A variant the renderer does not know, and every
// reminder in a transcript with no renderer (Load's), is left out, as a newer
// craze's type is. The store's copy (Transcript) and a reopened store render
// as the store does, and an interrupted answer is led by its reminder too.
func TestRemindersAreWrittenWhereTheRequestHadThem(t *testing.T) {
	opts := reminded(testOptions(t))
	s := newStore(t, opts)
	if err := s.AppendModeChange("plan"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser(withTurn(user("q1", kimi), 1)); err != nil {
		t.Fatal(err)
	}
	ids, err := s.AppendStepLed([]Lead{reminderLead("plan_full_empty")}, calls(kimi, "call_a"), results(kimi, "call_a"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 5 {
		t.Fatalf("the first step wrote %d entries, want 5 (mode change, user, reminder, calls, results)", len(ids))
	}
	step := []Lead{reminderLead("ask"), steerLead("a steer", kimi), reminderLead("unknown")}
	if _, err := s.AppendStepLed(step, answer("", "a1", kimi), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendUser(withTurn(user("q2", kimi), 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendAnswerLed([]Lead{reminderLead("plan_sparse")}, MessageEntry{Message: assistantMsg("", "cut"), Model: kimi, Interrupted: true}); err != nil {
		t.Fatal(err)
	}
	if got, want := fileTypes(t, s.Path()), []string{
		"session", "mode_change", "message", "reminder", "message", "message",
		"reminder", "message", "reminder", "message",
		"message", "reminder", "message",
	}; !slices.Equal(got, want) {
		t.Fatalf("the file's lines are %q, want %q", got, want)
	}

	want := []string{
		"user: q1", "user: <reminder plan_full_empty>", callA, resultA,
		"user: <reminder ask>", "user: a steer", "assistant: a1",
		"user: q2", "user: <reminder plan_sparse>", "assistant: cut",
	}
	if got := messageTexts(s.Context(kimi)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the store's context = %q\nwant %q", got, want)
	}
	if got := messageTexts(s.Transcript().Context(kimi)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the copy's context = %q\nwant %q", got, want)
	}
	msgs, marks := s.ContextWithResults(kimi)
	if len(msgs) != len(marks) || slices.Contains(marks, true) {
		t.Fatalf("the context's marks are %v; a reminder is never an entry of results", marks)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	tr, err := Load(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var none []string
	for _, w := range want {
		if !strings.Contains(w, "<reminder") {
			none = append(none, w)
		}
	}
	if got := messageTexts(tr.Context(kimi)); !reflect.DeepEqual(got, none) {
		t.Fatalf("with no renderer the context = %q\nwant %q", got, none)
	}
	if n := len(tr.Entries); tr.Entries[n-2].Type != TypeReminder || tr.Entries[n-2].Variant != "plan_sparse" {
		t.Fatalf("the interrupted answer's lead read back as %+v", tr.Entries[n-2])
	}

	r := reopen(t, opts, s.Path())
	if got := messageTexts(r.Context(kimi)); !reflect.DeepEqual(got, want) {
		t.Fatalf("the reopened store's context = %q\nwant %q", got, want)
	}
}

// TestAReminderIsCheckedOnTheWayIn: a reminder's variant is a name — never a
// text, which could only be a caller's bug — and a lead is a reminder or a
// message, not both; a steer lead still opens no turn. Each is refused with
// nothing written. The control is the same step with good leads.
func TestAReminderIsCheckedOnTheWayIn(t *testing.T) {
	s := newStore(t, reminded(testOptions(t)))
	if err := s.AppendUser(withTurn(user("q", kimi), 1)); err != nil {
		t.Fatal(err)
	}
	for name, lead := range map[string]Lead{
		"a text":               reminderLead("Plan mode is active."),
		"a wrapped text":       reminderLead("<system-reminder>"),
		"upper case":           reminderLead("Plan_full"),
		"a dash":               reminderLead("plan-full"),
		"too long":             reminderLead(strings.Repeat("a", maxVariant+1)),
		"a message too":        {Reminder: "ask", Message: user("both", kimi)},
		"a steer with a turn":  {Message: withTurn(user("steer", kimi), 2)},
		"a lead with no role":  {},
		"an assistant message": {Message: answer("", "a", kimi)},
	} {
		if _, err := s.AppendStepLed([]Lead{lead}, answer("", "a", kimi), nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := s.AppendAnswerLed([]Lead{lead}, answer("", "a", kimi)); err == nil {
			t.Errorf("%s: accepted by AppendAnswerLed", name)
		}
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused step wrote the transcript (stat err %v)", err)
	}
	// The control.
	if _, err := s.AppendStepLed([]Lead{reminderLead(strings.Repeat("a", maxVariant)), steerLead("steer", kimi)}, answer("", "a", kimi), nil); err != nil {
		t.Fatal(err)
	}
}

// TestAWholeReminderLineWithABadVariantIsCorrupt (P14): a reminder whose
// variant is missing, not a string, or not a name can only be a whole line —
// a torn append leaves a prefix of one, which does not decode — so it is
// ErrCorrupt even as the last line, by Load and by Open alike, and Open
// leaves the file as it was. The control is the same line cut in half, a
// torn tail Load drops, and the line with a good variant, which loads.
func TestAWholeReminderLineWithABadVariantIsCorrupt(t *testing.T) {
	u := userLine(t, "00000001", "", "q1")
	envelope := `{"type":"reminder","id":"00000002","parentId":"00000001","timestamp":"2026-09-18T12:00:00.000Z"`
	if _, err := Load(writeFile(t, lines(headerText(t), u, envelope+`,"variant":"plan_sparse"}`))); err != nil {
		t.Fatalf("control: a good reminder line: %v", err)
	}
	for name, rest := range map[string]string{
		"no variant":        `}`,
		"a null variant":    `,"variant":null}`,
		"a number":          `,"variant":7}`,
		"an empty variant":  `,"variant":""}`,
		"a text":            `,"variant":"Plan mode is active."}`,
		"an escaped tag":    `,"variant":"<system-reminder>"}`,
		"a list":            `,"variant":["ask"]}`,
		"too long":          `,"variant":"` + strings.Repeat("a", maxVariant+1) + `"}`,
		"a control char":    `,"variant":"ask\n"}`,
		"an upper-case one": `,"variant":"ASK"}`,
	} {
		t.Run(name, func(t *testing.T) {
			last := envelope + rest
			raw := lines(headerText(t), u, last)
			path := writeFile(t, raw)
			if _, err := Load(path); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "invalid entry") {
				t.Fatalf("Load = %v, want ErrCorrupt for an invalid entry", err)
			}
			if _, err := Open(testOptions(t), path); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Open = %v, want ErrCorrupt", err)
			}
			if got, _ := os.ReadFile(path); string(got) != raw {
				t.Fatal("a refused Open changed the file")
			}
			if _, err := Load(writeFile(t, lines(headerText(t), u)+last[:len(last)/2])); err != nil {
				t.Fatalf("control: Load of the line torn in half = %v", err)
			}
		})
	}
}

// TestAReminderNeverSplitsACallFromItsResults: pairing holds for a reminder as
// for any entry — none may come between an assistant entry's calls and their
// results — so a file with one there is ErrCorrupt (ErrUnpaired), the last
// line included, and no append can put one there.
func TestAReminderNeverSplitsACallFromItsResults(t *testing.T) {
	raw := lines(
		headerText(t),
		userLine(t, "00000001", "", "q1"),
		callsLine(t, "00000002", "00000001", "call_a"),
		`{"type":"reminder","id":"00000003","parentId":"00000002","timestamp":"2026-09-18T12:00:00.000Z","variant":"ask"}`,
		resultsLine(t, "00000004", "00000003", "call_a"),
	)
	for _, cut := range []string{raw, strings.TrimSuffix(raw, resultsLine(t, "00000004", "00000003", "call_a")+"\n")} {
		if _, err := Load(writeFile(t, cut)); !errors.Is(err, ErrCorrupt) || !errors.Is(err, ErrUnpaired) {
			t.Fatalf("Load = %v, want ErrCorrupt wrapping ErrUnpaired", err)
		}
	}
}

// TestOpenRollsBackEveryCrashPrefixOfAReminderStep extends A3 to a step led by
// reminders: for every byte prefix of an append holding the turn's user
// entry, a reminder, a steer, a second reminder, an assistant entry with a
// call and its results, Open keeps exactly the complete steps — a reminder of
// a cut step goes with it — and a new step appends after it, paired, with
// the kept reminders rendered at their places in the context.
func TestOpenRollsBackEveryCrashPrefixOfAReminderStep(t *testing.T) {
	opts := reminded(testOptions(t))
	s := newStore(t, opts)
	turn(t, s, "q1", "a1", kimi)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	base, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	baseIDs := entryIDs(s.Transcript().Entries)

	full := reopen(t, opts, s.Path())
	if err := full.AppendUser(withTurn(user("q2", kimi), 2)); err != nil {
		t.Fatal(err)
	}
	lead := []Lead{reminderLead("plan_full_empty"), steerLead("a steer", kimi), reminderLead("ask")}
	if _, err := full.AppendStepLed(lead, calls(kimi, "call_a"), results(kimi, "call_a")); err != nil {
		t.Fatal(err)
	}
	if err := full.Close(); err != nil {
		t.Fatal(err)
	}
	whole, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	app := whole[len(base):]
	allIDs := entryIDs(full.Transcript().Entries)
	if n := strings.Count(string(app), "\n"); n != 7 || len(allIDs) != len(baseIDs)+7 {
		t.Fatalf("the append has %d lines and %d entries; want 7 (resume, user, reminder, steer, reminder, calls, results)", n, len(allIDs)-len(baseIDs))
	}

	dir := t.TempDir()
	for k := 0; k <= len(app); k++ {
		sub := filepath.Join(dir, fmt.Sprint(k))
		if err := os.Mkdir(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(sub, filepath.Base(s.Path()))
		if err := os.WriteFile(path, append(append([]byte(nil), base...), app[:k]...), 0o600); err != nil {
			t.Fatal(err)
		}
		ropts := opts
		ropts.SessionID = ""
		ropts.fsStep = (&steps{skipSyncs: true}).seam
		r, err := Open(ropts, path)
		if err != nil {
			t.Fatalf("prefix %d: Open: %v", k, err)
		}
		wantIDs, context := baseIDs, []string{"user: q1", "assistant: a1"}
		if k >= len(app)-1 { // the results line whole, its newline or not
			wantIDs = allIDs
			context = append(context, "user: q2", "user: <reminder plan_full_empty>", "user: a steer", "user: <reminder ask>",
				callA, resultA)
		}
		if got := entryIDs(r.Transcript().Entries); !slices.Equal(got, wantIDs) {
			t.Fatalf("prefix %d: kept %q, want %q", k, got, wantIDs)
		}
		if got := messageTexts(r.Context(kimi)); !slices.Equal(got, context) {
			t.Fatalf("prefix %d: context %q, want %q", k, got, context)
		}
		if err := r.AppendUser(withTurn(user("next", kimi), 3)); err != nil {
			t.Fatalf("prefix %d: AppendUser: %v", k, err)
		}
		if _, err := r.AppendStepLed([]Lead{reminderLead("ask")}, answer("", "done", kimi), nil); err != nil {
			t.Fatalf("prefix %d: AppendStepLed: %v", k, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		tr, err := Load(path)
		if err != nil {
			t.Fatalf("prefix %d: Load after the append: %v", k, err)
		}
		if n := len(tr.Entries); n != len(wantIDs)+4 {
			t.Fatalf("prefix %d: %d entries after the append, want %d (kept, resume, user, reminder, answer)", k, n, len(wantIDs)+4)
		}
		if err := unpairedIn(tr.Context(kimi)); err != nil {
			t.Fatalf("prefix %d: %v", k, err)
		}
	}
}
