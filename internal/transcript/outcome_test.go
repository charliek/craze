package transcript

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
)

// The outcome notes (plan 032 §3.2 C4, P14; SF-61): an answered question's
// or plan's ending draws its note into the main transcript, worded from the
// opening the fold holds, so every client — and every restore — shows it.

// notesOf is the main transcript's notes, oldest first.
func notesOf(m *Model) []string { return factTexts(m.Main, "note") }

// ending is an ask's ending of kind, answered unless edit says otherwise.
func ending(id string, kind agent.AskKind, edit func(*agent.AskUpdate)) agent.Event {
	u := &agent.AskUpdate{ID: id, Kind: kind, Outcome: agent.AskAnswered, By: agent.AskByClient}
	if edit != nil {
		edit(u)
	}
	return agent.Event{Type: agent.EventAsk, Ask: u, At: at(50)}
}

func questionOpens(q *agent.QuestionEvent) agent.Event {
	return agent.Event{Type: agent.EventQuestion, Question: q, At: at(10)}
}

func planOpens(p *agent.PlanEvent) agent.Event {
	return agent.Event{Type: agent.EventPlan, Plan: p, At: at(10)}
}

// oneQuestionAsk is a request that asks one thing.
func oneQuestionAsk() *agent.QuestionEvent {
	q := stubQuestion()
	q.Questions = q.Questions[:1]
	return q
}

func TestAnAnsweredQuestionDrawsANotePerQuestion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		q       *agent.QuestionEvent
		answers map[string][]string
		want    []string
	}{
		{"one question", oneQuestionAsk(), map[string][]string{"q1": {"opt-b"}}, []string{"? Pick one → B"}},
		{"two questions, one of them multi-select", stubQuestion(),
			map[string][]string{"q1": {"opt-a"}, "q2": {"opt-x", "opt-z"}},
			[]string{"? Pick one → A", "? Pick any → X, Z"}},
		{"a question left unanswered", stubQuestion(), map[string][]string{"q1": {"opt-a"}},
			[]string{"? Pick one → A", "? Pick any → nothing"}},
		{"an option the question does not offer", oneQuestionAsk(), map[string][]string{"q1": {"opt-nope"}},
			[]string{"? Pick one → nothing"}},
		{"no answers at all", stubQuestion(), nil, []string{"? Pick one → nothing", "? Pick any → nothing"}},
		{"a request that asks nothing", &agent.QuestionEvent{ID: "ask-1", Title: "Anything?"}, nil, nil},
		{"text folded onto one line", &agent.QuestionEvent{ID: "ask-1", Questions: []agent.Question{{
			ID: "q1", Prompt: "Pick\n  \x1b[1mone\x1b[0m", Options: []agent.Option{{ID: "o", Label: "the\tfirst\x07"}},
		}}}, map[string][]string{"q1": {"o"}}, []string{"? Pick one → the first"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, sequenced([]agent.Event{
				questionOpens(tc.q),
				ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Answers = tc.answers }),
			})...)
			if got := notesOf(m); !slices.Equal(got, tc.want) {
				t.Fatalf("notes %q, want %q", got, tc.want)
			}
			for i, f := range factsOf(m.Main, "note") {
				if !f.At.Equal(at(50)) {
					t.Fatalf("note %d is stamped %v, want the ending's %v", i, f.At, at(50))
				}
			}
			for i, e := range m.Main.live() {
				if e.ID != (EntryID{Seq: 2, N: uint32(i)}) {
					t.Fatalf("entry %d is %v: the notes are the ending's (seq 2), in order", i, e.ID)
				}
			}
			if len(m.State().Asks) != 0 || len(m.EndedAsks()) != 1 {
				t.Fatalf("the ask is still open (%+v) or unrecorded (%+v)", m.State().Asks, m.EndedAsks())
			}
		})
	}
}

func TestASkippedQuestionDrawsItsNote(t *testing.T) {
	untitled := stubQuestion()
	untitled.Title = " \n"
	for _, tc := range []struct {
		name string
		q    *agent.QuestionEvent
		want string
	}{
		{"by its title", stubQuestion(), "? Question → skipped"},
		{"by its first prompt when it has no title", untitled, "? Pick one → skipped"},
		{"as a question when it has neither", &agent.QuestionEvent{ID: "ask-1"}, "? question → skipped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, sequenced([]agent.Event{
				questionOpens(tc.q),
				// A skip carries no answers, and any it did carry are not drawn.
				ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) {
					u.Skip, u.Answers = true, map[string][]string{"q1": {"opt-a"}}
				}),
			})...)
			if got := notesOf(m); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("notes %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAnAnsweredPlanDrawsItsVerb(t *testing.T) {
	unnamed := stubPlanEvent()
	unnamed.Name = "\t"
	for _, tc := range []struct {
		name     string
		p        *agent.PlanEvent
		accepted bool
		want     string
	}{
		{"accepted", stubPlanEvent(), true, "plan Fake Plan → accepted"},
		{"rejected", stubPlanEvent(), false, "plan Fake Plan → rejected"},
		{"with no name", unnamed, true, "plan plan → accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, sequenced([]agent.Event{
				planOpens(tc.p),
				ending("plan-1", agent.AskPlan, func(u *agent.AskUpdate) { u.Accepted = tc.accepted }),
			})...)
			if got := factTexts(m.Main, "plan"); len(got) != 1 {
				t.Fatalf("the plan entry: %v", facts(m.Main))
			}
			if got := notesOf(m); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("notes %q, want %q", got, tc.want)
			}
		})
	}
}

// An ending that decided nothing a user chose draws nothing, nor does a
// permission's answer, which never drew a row; each still ends its ask.
func TestAnEndingThatDecidesNothingDrawsNoNote(t *testing.T) {
	outcome := func(o agent.AskOutcome, by string) func(*agent.AskUpdate) {
		return func(u *agent.AskUpdate) { u.Outcome, u.By = o, by }
	}
	for _, tc := range []struct {
		name string
		open agent.Event
		end  agent.Event
	}{
		{"a permission answered", agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)},
			ending("perm-1", agent.AskPermission, func(u *agent.AskUpdate) { u.OptionID, u.Label = "opt-once", "Allow once" })},
		{"a question cancelled", questionOpens(stubQuestion()), ending("ask-1", agent.AskQuestion, outcome(agent.AskCancelled, agent.AskByCancel))},
		{"a question whose call ended", questionOpens(stubQuestion()), ending("ask-1", agent.AskQuestion, outcome(agent.AskCancelled, agent.AskByCall))},
		{"a question whose turn ended", questionOpens(stubQuestion()), ending("ask-1", agent.AskQuestion, outcome(agent.AskTurnEnded, agent.AskByTurn))},
		{"a question at the close", questionOpens(stubQuestion()), ending("ask-1", agent.AskQuestion, outcome(agent.AskClosing, agent.AskByClose))},
		{"a question craze's policy answered", questionOpens(stubQuestion()), ending("ask-1", agent.AskQuestion, outcome(agent.AskAutomatic, agent.AskByPolicy))},
		{"a plan cancelled", planOpens(stubPlanEvent()), ending("plan-1", agent.AskPlan, func(u *agent.AskUpdate) {
			u.Outcome, u.By, u.Accepted = agent.AskCancelled, agent.AskByCancel, true
		})},
		{"a plan whose turn ended", planOpens(stubPlanEvent()), ending("plan-1", agent.AskPlan, outcome(agent.AskTurnEnded, agent.AskByTurn))},
		{"a plan at the close", planOpens(stubPlanEvent()), ending("plan-1", agent.AskPlan, outcome(agent.AskClosing, agent.AskByClose))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, sequenced([]agent.Event{tc.open, tc.end})...)
			if got := notesOf(m); len(got) != 0 {
				t.Fatalf("an ending that decided nothing drew %q", got)
			}
			if len(m.State().Asks) != 0 || len(m.EndedAsks()) != 1 {
				t.Fatalf("the ask is still open (%+v) or unrecorded (%+v)", m.State().Asks, m.EndedAsks())
			}
		})
	}
}

// An Auto question or plan is craze's own headless answer: no card, no plan
// entry, and no note, whatever its ending says.
func TestAnAutoAskDrawsNoNote(t *testing.T) {
	q, p := stubQuestion(), stubPlanEvent()
	q.Auto, p.Auto = true, true
	for _, tc := range []struct {
		name string
		open agent.Event
		end  agent.Event
	}{
		{"a question answered", questionOpens(q), ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) {
			u.Answers = map[string][]string{"q1": {"opt-a"}}
		})},
		{"a question skipped", questionOpens(q), ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Skip = true })},
		{"a plan accepted", planOpens(p), ending("plan-1", agent.AskPlan, func(u *agent.AskUpdate) { u.Accepted = true })},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, sequenced([]agent.Event{tc.open, tc.end})...)
			if es := facts(m.Main); len(es) != 0 {
				t.Fatalf("an Auto ask drew %v", es)
			}
		})
	}
}

// A kind the session's capabilities hide draws no note: every client answers
// it unseen — a question skipped, a plan rejected — and those answers are not
// the user's. The kind left visible still draws its own.
func TestAHiddenAskDrawsNoNote(t *testing.T) {
	answered := map[string][]string{"q1": {"opt-a"}}
	for _, tc := range []struct {
		name   string
		hidden HiddenAsks
		want   []string
	}{
		{"nothing hidden", HiddenAsks{}, []string{"? Question → skipped", "plan Fake Plan → rejected", "? Pick one → A", "? Pick any → nothing", "plan Fake Plan → accepted"}},
		{"questions hidden", HiddenAsks{Questions: true}, []string{"plan Fake Plan → rejected", "plan Fake Plan → accepted"}},
		{"plans hidden", HiddenAsks{Plans: true}, []string{"? Question → skipped", "? Pick one → A", "? Pick any → nothing"}},
		{"both hidden", HiddenAsks{Questions: true, Plans: true}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{Hidden: tc.hidden})
			if m.Hidden() != tc.hidden {
				t.Fatalf("the model reports %+v hidden, want %+v", m.Hidden(), tc.hidden)
			}
			foldAll(t, m, true, sequenced([]agent.Event{
				// What a client sends for a hidden ask: the skip, the reject…
				questionOpens(stubQuestion()),
				ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Skip = true }),
				planOpens(stubPlanEvent()),
				ending("plan-1", agent.AskPlan, nil),
				// …and any other answer to one, which no card offered either.
				questionOpens(stubQuestion()),
				ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Answers = answered }),
				planOpens(stubPlanEvent()),
				ending("plan-1", agent.AskPlan, func(u *agent.AskUpdate) { u.Accepted = true }),
			})...)
			if got := notesOf(m); !slices.Equal(got, tc.want) {
				t.Fatalf("notes %q, want %q", got, tc.want)
			}
		})
	}
}

// HiddenBy reads the two card capabilities, and every shipped provider shows
// both: the zero HiddenAsks, which a construction site with no capabilities
// to hand assumes, is every real session's.
func TestHiddenByReadsTheCardCapabilities(t *testing.T) {
	for _, tc := range []struct {
		caps agent.Capabilities
		want HiddenAsks
	}{
		{agent.Capabilities{AskCards: true, PlanCards: true}, HiddenAsks{}},
		{agent.Capabilities{AskCards: true}, HiddenAsks{Plans: true}},
		{agent.Capabilities{PlanCards: true}, HiddenAsks{Questions: true}},
		{agent.Capabilities{}, HiddenAsks{Questions: true, Plans: true}},
	} {
		if got := HiddenBy(tc.caps); got != tc.want {
			t.Errorf("HiddenBy(%+v) = %+v, want %+v", tc.caps, got, tc.want)
		}
	}
	for _, p := range agent.Providers() {
		if got := HiddenBy(p.Capabilities()); got != (HiddenAsks{}) {
			t.Errorf("provider %s hides %+v: the zero default no longer matches every shipped provider", p.Name(), got)
		}
	}
}

// An ending whose opening the fold never saw has nothing to word: no note,
// whether its id was never opened, it carries an opening of its own (an ask
// never published), or its kind is not the open ask's.
func TestAnEndingWithNoOpeningDrawsNoNote(t *testing.T) {
	for _, tc := range []struct {
		name string
		evs  []agent.Event
	}{
		{"an id never opened", []agent.Event{
			ending("ask-9", agent.AskQuestion, func(u *agent.AskUpdate) { u.Skip = true }),
		}},
		{"an ending carrying its own opening", []agent.Event{
			ending("ask-9", agent.AskQuestion, func(u *agent.AskUpdate) {
				u.Skip, u.Body = true, &agent.AskBody{Question: stubQuestion()}
			}),
		}},
		{"an ending of another kind", []agent.Event{
			questionOpens(stubQuestion()),
			ending("ask-1", agent.AskPlan, func(u *agent.AskUpdate) { u.Accepted = true }),
		}},
		{"an ending with no id", []agent.Event{
			questionOpens(stubQuestion()),
			ending("", agent.AskQuestion, func(u *agent.AskUpdate) { u.Skip = true }),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, sequenced(tc.evs)...)
			if got := notesOf(m); len(got) != 0 {
				t.Fatalf("an ending with no opening to word drew %q", got)
			}
		})
	}
}

// One opening, one note: the same ending folded again — re-delivered with its
// seq, unsequenced, or as a later event — finds the ask gone. An id opened
// again after its ending is a new ask, and its own ending draws its own note.
func TestADuplicateEndingDrawsOneNote(t *testing.T) {
	m := New(Options{})
	skip := ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Skip = true })
	evs := sequenced([]agent.Event{questionOpens(stubQuestion()), skip})
	foldAll(t, m, true, evs...)
	again := evs[1]
	foldAll(t, m, true, again)
	again.Seq = 0
	foldAll(t, m, true, again)
	again.Seq = 3
	foldAll(t, m, true, again)
	if got := notesOf(m); !slices.Equal(got, []string{"? Question → skipped"}) {
		t.Fatalf("a re-ended id drew %q, want the one note", got)
	}
	reopened := questionOpens(stubQuestion())
	reopened.Seq = 4
	skip.Seq = 5
	foldAll(t, m, true, reopened, skip)
	if got := notesOf(m); !slices.Equal(got, []string{"? Question → skipped", "? Question → skipped"}) {
		t.Fatalf("the id's second ask drew %q, want a note of its own", got)
	}
}

// Snapshot plus tail: at every cut of a session whose asks end in every way —
// before an opening, between an opening and its ending, after a note — a
// snapshot restored (in process and through the codec) and folded on is the
// model that folded everything, notes and all, for a session that hides
// nothing and for one that hides plans. Hidden is not in the snapshot: the
// restorer is given it, as the first model was.
func TestOutcomeNotesSurviveASnapshotAndItsTail(t *testing.T) {
	p2 := stubPlanEvent()
	p2.ID, p2.Name = "plan-2", "Second"
	evs := sequenced([]agent.Event{
		questionOpens(stubQuestion()),
		ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Answers = map[string][]string{"q1": {"opt-b"}, "q2": {"opt-y"}} }),
		planOpens(stubPlanEvent()),
		{Type: agent.EventPermission, Permission: stubPermissionEvent(false), At: at(11)},
		ending("plan-1", agent.AskPlan, func(u *agent.AskUpdate) { u.Accepted = true }),
		ending("perm-1", agent.AskPermission, func(u *agent.AskUpdate) { u.OptionID = "opt-once" }),
		questionOpens(oneQuestionAsk()),
		{Type: agent.EventText, Text: "while you think", At: at(12)},
		ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Skip = true }),
		planOpens(p2),
		ending("plan-2", agent.AskPlan, func(u *agent.AskUpdate) { u.Outcome, u.By = agent.AskTurnEnded, agent.AskByTurn }),
		{Type: agent.EventDone, StopReason: "end_turn", At: at(60)},
	})
	for _, tc := range []struct {
		name   string
		hidden HiddenAsks
		notes  []string
	}{
		{"nothing hidden", HiddenAsks{}, []string{"? Pick one → B", "? Pick any → Y", "plan Fake Plan → accepted", "? Question → skipped"}},
		{"plans hidden", HiddenAsks{Plans: true}, []string{"? Pick one → B", "? Pick any → Y", "? Question → skipped"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{Hidden: tc.hidden}
			whole := New(o)
			foldAll(t, whole, true, evs...)
			if got := notesOf(whole); !slices.Equal(got, tc.notes) {
				t.Fatalf("folded whole, the notes are %q, want %q", got, tc.notes)
			}
			for cut := 0; cut <= len(evs); cut++ {
				m := New(o)
				foldAll(t, m, false, evs[:cut]...)
				s, b := snapshotOf(t, m, 0)
				r1, r2 := restoredBoth(t, s, b, o)
				continueBoth(t, fmt.Sprintf("cut %d", cut), m, []*Model{r1, r2}, evs[cut:]...)
				assertSameModel(t, fmt.Sprintf("cut %d, in process", cut), whole, r1)
				assertSameModel(t, fmt.Sprintf("cut %d, through the codec", cut), whole, r2)
			}
		})
	}
}

// The note is worded from the ask as a snapshot carries it (plan 032 C4
// review r3, finding 1). A client restored from a snapshot taken while the ask
// was open holds every string of it at its ItemCap head; at every cut — the
// one between the opening and the ending above all — it draws, under the one
// shared entry id, the note the model that folded everything draws, byte for
// byte: an oversized prompt, label, title or plan name, and an option id the
// snapshot cut, alike. Each row pins its note; an ask a snapshot carries whole
// words exactly as it always did. While the ask is open the restored state
// holds its capped form (Ask.Truncated), so the models are compared once the
// ending has taken it out of the open set.
func TestOutcomeNotesWordTheAskAsASnapshotCarriesIt(t *testing.T) {
	pad := strings.Repeat(" ", ItemCap)
	longID := strings.Repeat("x", ItemCap) + "-yes"
	longLabel := strings.Repeat("a", ItemCap+10)
	proceed := func(prompt, yesID, yesLabel string) *agent.QuestionEvent {
		return &agent.QuestionEvent{ID: "ask-1", Questions: []agent.Question{{
			ID: "q1", Prompt: prompt,
			Options: []agent.Option{{ID: yesID, Label: yesLabel}, {ID: "no", Label: "No"}},
		}}}
	}
	answer := func(ids ...string) agent.Event {
		return ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Answers = map[string][]string{"q1": ids} })
	}
	skip := ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) { u.Skip = true })
	accept := ending("plan-1", agent.AskPlan, func(u *agent.AskUpdate) { u.Accepted = true })
	titled := func(title string) *agent.QuestionEvent {
		q := stubQuestion()
		q.Title = title
		return q
	}
	named := func(name string) *agent.PlanEvent {
		p := stubPlanEvent()
		p.Name = name
		return p
	}
	head := "? Proceed? → "
	for _, tc := range []struct {
		name string
		open agent.Event
		end  agent.Event
		want string
	}{
		{"a question a snapshot carries whole", questionOpens(proceed("Proceed?", "yes", "Yes")), answer("yes"), "? Proceed? → Yes"},
		{"a plan a snapshot carries whole", planOpens(stubPlanEvent()), accept, "plan Fake Plan → accepted"},
		{"a prompt whose words start past ItemCap", questionOpens(proceed(pad+"Proceed?", "yes", "Yes")), answer("yes"), "?  → Yes"},
		{"a prompt cut in a run of spaces", questionOpens(proceed("Proceed?"+pad+"really", "yes", "Yes")), answer("yes"), "? Proceed? → Yes"},
		{"a label cut in a run of spaces", questionOpens(proceed("Proceed?", "yes", "Yes"+pad+"!")), answer("yes"), "? Proceed? → Yes"},
		{"a label past the note's cap", questionOpens(proceed("Proceed?", "yes", longLabel)), answer("yes"),
			head + longLabel[:outcomeNoteCap-len(ellipsis)-len(head)] + ellipsis},
		{"an option id past ItemCap", questionOpens(proceed("Proceed?", longID, "Yes")), answer(longID), "? Proceed? → Yes"},
		{"a skipped question's title cut in a run of spaces", questionOpens(titled("Skip me" + pad + "now")), skip, "? Skip me → skipped"},
		{"a plan's name cut in a run of spaces", planOpens(named("Ship" + pad + "it")), accept, "plan Ship → accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evs := sequenced([]agent.Event{tc.open, tc.end, {Type: agent.EventDone, StopReason: "end_turn", At: at(60)}})
			whole := New(Options{})
			foldAll(t, whole, true, evs...)
			want := notesOf(whole)
			if !slices.Equal(want, []string{tc.want}) {
				t.Fatalf("folded whole, the notes are %s, want %s", clipNotes(want...), clipNotes(tc.want))
			}
			for cut := 0; cut <= len(evs); cut++ {
				m := New(Options{})
				foldAll(t, m, false, evs[:cut]...)
				s, b := snapshotOf(t, m, 0)
				r1, r2 := restoredBoth(t, s, b, Options{})
				for _, ev := range evs[cut:] {
					for _, x := range []*Model{m, r1, r2} {
						x.Fold(ev)
					}
				}
				for i, x := range []*Model{m, r1, r2} {
					what := fmt.Sprintf("cut %d, %s", cut, []string{"live", "restored in process", "restored through the codec"}[i])
					if got := notesOf(x); !slices.Equal(got, want) {
						t.Fatalf("%s: the notes are %s, want %s", what, clipNotes(got...), clipNotes(want...))
					}
					assertSameModel(t, what, whole, x)
					checkInvariants(t, x)
				}
			}
		})
	}
}

// An ask whose id a snapshot cuts draws no note, live or restored: a restored
// fold holds it under the id's head, which its ending's id can never find, so
// a live note would be one no restored client could draw. Only the notes are
// compared: the restored open set keeps the ask under that head, the
// snapshot's own cut and not this rule's.
func TestAnAskIDASnapshotCutsDrawsNoNote(t *testing.T) {
	q := stubQuestion()
	q.ID = strings.Repeat("k", ItemCap+1)
	evs := sequenced([]agent.Event{
		questionOpens(q),
		ending(q.ID, agent.AskQuestion, func(u *agent.AskUpdate) { u.Answers = map[string][]string{"q1": {"opt-a"}} }),
	})
	whole := New(Options{})
	foldAll(t, whole, true, evs...)
	m := New(Options{})
	foldAll(t, m, false, evs[0])
	s, b := snapshotOf(t, m, 0)
	r1, r2 := restoredBoth(t, s, b, Options{})
	for i, x := range []*Model{whole, r1, r2} {
		if i > 0 {
			x.Fold(evs[1])
		}
		if got := notesOf(x); len(got) != 0 {
			t.Fatalf("model %d drew %s for an ask whose id a snapshot cuts", i, clipNotes(got...))
		}
	}
}

// One long label picked over and over (plan 032 C4 review r3, finding 2):
// the agent's validator checks that each pick is offered, not that it is new,
// so 65 picks of one 64 KiB label would word a note of over 4 MiB — the
// newest entry once the turn ends, which every snapshot must carry, so no
// client could attach. The note is held to outcomeNoteCap: the ended session
// snapshots at the default budget and restores as itself.
func TestARepeatedLongLabelLeavesTheSessionAttachable(t *testing.T) {
	label := strings.Repeat("a", 64<<10)
	q := &agent.QuestionEvent{ID: "ask-1", Questions: []agent.Question{{
		ID: "q1", Prompt: "Pick", AllowMultiple: true, Options: []agent.Option{{ID: "x", Label: label}},
	}}}
	evs := sequenced([]agent.Event{
		questionOpens(q),
		ending("ask-1", agent.AskQuestion, func(u *agent.AskUpdate) {
			u.Answers = map[string][]string{"q1": slices.Repeat([]string{"x"}, 65)}
		}),
		{Type: agent.EventDone, StopReason: "end_turn", At: at(60)},
	})
	m := New(Options{})
	foldAll(t, m, true, evs...)
	s, b := snapshotOf(t, m, 0)
	r1, r2 := restoredBoth(t, s, b, Options{})
	assertSameModel(t, "restored in process", m, r1)
	assertSameModel(t, "restored through the codec", m, r2)
	head := "? Pick → "
	want := head + label[:outcomeNoteCap-len(ellipsis)-len(head)] + ellipsis
	if got := notesOf(m); !slices.Equal(got, []string{want}) {
		t.Fatalf("the notes are %s, want %s", clipNotes(got...), clipNotes(want))
	}
	if len(want) > outcomeNoteCap {
		t.Fatalf("the note is %d bytes, over its %d-byte cap", len(want), outcomeNoteCap)
	}
}

// capNote keeps a note that fits whole, and cuts a longer one back to a rune
// boundary under the ellipsis, outcomeNoteCap bytes at most with it.
func TestCapNoteCutsOnARuneBoundary(t *testing.T) {
	fits := strings.Repeat("a", outcomeNoteCap)
	if got := capNote(fits); got != fits {
		t.Fatalf("a note of exactly the cap was cut to %d bytes", len(got))
	}
	for _, unit := range []string{"a", "é", "⤷", "😀"} {
		for pad := range 4 {
			s := strings.Repeat("b", pad) + strings.Repeat(unit, outcomeNoteCap+1)
			got := capNote(s)
			body, ok := strings.CutSuffix(got, ellipsis)
			switch {
			case !ok || len(got) > outcomeNoteCap:
				t.Fatalf("%q after %d bytes: capped to %d bytes, ellipsis %v", unit, pad, len(got), ok)
			case !utf8.ValidString(got) || !strings.HasPrefix(s, body):
				t.Fatalf("%q after %d bytes: the head is not a rune-boundary prefix of the note", unit, pad)
			case len(body) < outcomeNoteCap-len(ellipsis)-utf8.UTFMax+1:
				t.Fatalf("%q after %d bytes: the head is %d bytes, cut back further than one rune", unit, pad, len(body))
			}
		}
	}
}

// answerNote stops naming once its note is past the cap, and what it builds
// caps to exactly the whole note's capping — the rule before the early stop,
// answerLabels' join, here as the reference — for every length of label and
// count of picks around the cap, a multi-byte label and an empty one among
// them; and what it builds is bounded by the cap plus one label, not by the
// count of picks — 3000 picks of a label three caps long build no more than
// four caps.
func TestAnswerNoteStopsPastTheCapAndCapsAsTheWholeNote(t *testing.T) {
	whole := func(qq agent.Question, ids []string) string {
		var labels []string
		for _, id := range ids {
			for _, o := range qq.Options {
				if o.ID == id {
					labels = append(labels, sanitizeLine(o.Label))
					break
				}
			}
		}
		named := "nothing"
		if len(labels) > 0 {
			named = strings.Join(labels, ", ")
		}
		return "? " + sanitizeLine(qq.Prompt) + " → " + named
	}
	for _, label := range []string{"", "a", "ab", "é", "😀x", strings.Repeat("c", 1000), strings.Repeat("d", outcomeNoteCap-20), strings.Repeat("e", 3*outcomeNoteCap)} {
		qq := agent.Question{ID: "q1", Prompt: "Pick", Options: []agent.Option{{ID: "x", Label: label}, {ID: "y", Label: "Y"}}}
		for _, picks := range []int{0, 1, 2, 3, 5, 100, 1000, 1500, 2000, 2100, 3000} {
			ids := slices.Repeat([]string{"x", "nope", "y"}, picks)
			built := answerNote(qq, ids)
			// The reference builds the whole note: past a MiB it proves no
			// more than the rows below it do, and only costs time.
			if len(label)*picks <= 1<<20 {
				if got, want := capNote(built), capNote(whole(qq, ids)); got != want {
					t.Fatalf("label of %d bytes, %d picks: capped to %s, the whole note caps to %s", len(label), picks, clipNotes(got), clipNotes(want))
				}
			}
			// The longest it can go past the cap: a separator and the longest
			// label ("Y" among them), or "nothing".
			if limit := outcomeNoteCap + len(", ") + max(len(label), len("nothing")); len(built) > limit {
				t.Fatalf("label of %d bytes, %d picks: built %d bytes, over the cap plus one label (%d)", len(label), picks, len(built), limit)
			}
		}
	}
}

// clip quotes each string as at most its first 40 bytes and its length.
func clipNotes(ss ...string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		h, _ := headOf(s, 40)
		out[i] = fmt.Sprintf("%q(%d bytes)", h, len(s))
	}
	return "[" + strings.Join(out, " ") + "]"
}
