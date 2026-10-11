package transcript

import (
	"bytes"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

// The slash catalog in the model (plan 043 C2): folded as a whole replacement
// (P6), carried by the snapshot with its action fields never cut (P7), every
// holder's own (P6), and invisible to a client that predates it (§2.6).

// slashDeltaAt is the event a slash-only change reaches a client as: the
// section alone, a skill scan's.
func slashDeltaAt(sec int, items []agent.SlashItem) agent.Event {
	return agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Slash: &agent.SlashCatalog{Items: items}}, At: at(sec)}
}

// TestASlashDeltaFoldsAndRestores (plan 043 P6): the slash section folds
// through the delta table as a whole replacement — the model's own copy — and
// a snapshot carries it in its settings, in the event codec's own section
// shape, and restores it, so a client attaching after a change folds the
// catalog the session offers now. An empty section clears it, and a model no
// delta gave a catalog to — a session that publishes none — writes no "slash"
// key at all, so no snapshot recorded before the section existed moves.
// Negative control: a fold row that ignores an empty section leaves the
// catalog in place after the clear.
func TestASlashDeltaFoldsAndRestores(t *testing.T) {
	m := New(Options{})
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Model: strp("grok")}, At: at(1)})
	raw, err := EncodeSnapshot(mustSnapshot(t, m))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"slash"`)) {
		t.Fatalf("a snapshot with no slash catalog writes the key: %s", raw)
	}

	first := scriptSlash()
	foldAll(t, m, true, slashDeltaAt(2, first))
	if got := m.State().Settings.Slash; !reflect.DeepEqual(got, scriptSlash()) {
		t.Fatalf("the settings' catalog is %+v, want %+v", got, scriptSlash())
	}
	if n := len(m.Main.live()); n != 0 {
		t.Fatalf("a slash delta draws nothing: %v", facts(m.Main))
	}
	later := scriptSlash(agent.Skill{Name: "deploy", Description: "found by a scan"})
	foldAll(t, m, true, slashDeltaAt(3, later))
	if got := m.State().Settings.Slash; !reflect.DeepEqual(got, later) {
		t.Fatalf("the second section did not replace the first: %+v", got)
	}

	s, b := snapshotOf(t, m, capBudget)
	wire, err := agent.EncodeSlashCatalog(&agent.SlashCatalog{Items: later})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, append([]byte(`"slash":`), wire...)) {
		t.Fatalf("the snapshot's settings do not carry the slash section %s: %s", wire, b)
	}
	r1, r2 := restoredBoth(t, s, b, Options{})
	for _, r := range []*Model{r1, r2} {
		if got := r.State().Settings; !reflect.DeepEqual(got.Slash, later) || got.Truncated.Slash {
			t.Fatalf("restored, the catalog is %+v (marked %v), want %+v", got.Slash, got.Truncated.Slash, later)
		}
	}

	foldAll(t, m, true, slashDeltaAt(4, nil))
	if got := m.State().Settings.Slash; got != nil {
		t.Fatalf("an empty section left the catalog %+v", got)
	}
	if raw, err := EncodeSnapshot(mustSnapshot(t, m)); err != nil || bytes.Contains(raw, []byte(`"slash"`)) {
		t.Fatalf("a cleared catalog's snapshot: %s, %v", raw, err)
	}
}

// TestTheSlashCatalogIsEveryHoldersOwn (plan 043 P6): the rows the model holds
// are its own and every holder's are theirs — what the delta it folded, a
// State, a Snapshot and a Mirror it handed out, and the snapshot a model was
// restored from, have written into them after the fact reaches neither the
// model nor the next projection. Negative control: the fold keeping the
// delta's slice (no clone) goes red at the write after the fold; the cut
// sharing the model's goes red at the State; Restore keeping the snapshot's
// goes red at the restored model.
func TestTheSlashCatalogIsEveryHoldersOwn(t *testing.T) {
	want := scriptSlash()
	delta := &agent.SlashCatalog{Items: scriptSlash()}
	m := New(Options{})
	foldAll(t, m, true, agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Slash: delta}, At: at(1)})
	check := func(what string, got []agent.SlashItem) {
		t.Helper()
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: the catalog is %+v, want %+v", what, got, want)
		}
	}
	delta.Items[0].Name = "written after the fold"
	check("the delta written after the fold", m.State().Settings.Slash)

	st := m.State()
	st.Settings.Slash[0].Description = "written into a State"
	check("a State written into", m.State().Settings.Slash)

	mr := m.Mirror()
	mr.Settings.Slash[0].Run.Kind = "written into a Mirror"
	check("a Mirror written into", m.State().Settings.Slash)

	snap := mustSnapshot(t, m)
	snap.Settings.Slash[0].Run.Setting.Value = "written into a Snapshot"
	check("a Snapshot written into", m.State().Settings.Slash)
	check("the next Snapshot", mustSnapshot(t, m).Settings.Slash)

	from := mustSnapshot(t, m)
	r := Restore(from, Options{})
	from.Settings.Slash[0].Requires = "written after the restore"
	check("the restored model, its snapshot written into", r.State().Settings.Slash)
}

// slashStringFields is the path, as field indices, of every string reachable
// from an agent.SlashItem, by reflection: what TestASlashSnapshotCuts…'s rows
// are filled through, so a string field SlashItem (or SlashRun, or
// SlashSetting) gains is filled too.
func slashStringFields() (paths [][]int, names []string) {
	var walk func(typ reflect.Type, prefix []int, name string)
	walk = func(typ reflect.Type, prefix []int, name string) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			path := append(slices.Clone(prefix), i)
			switch f.Type.Kind() {
			case reflect.String:
				paths, names = append(paths, path), append(names, name+f.Name)
			case reflect.Struct:
				walk(f.Type, path, name+f.Name+".")
			}
		}
	}
	walk(reflect.TypeFor[agent.SlashItem](), nil, "")
	return paths, names
}

// slashDisplay is the slash row's display text, the strings a snapshot
// carries as their ItemCap head (capSlash); every other string is an action
// field, whose row is dropped instead (plan 043 P7).
var slashDisplay = map[string]bool{"Description": true, "Run.ArgHint": true, "Run.Reason": true}

// slashRow is a row named name with every string of it set to a short value
// of its own.
func slashRow(name string) agent.SlashItem {
	paths, names := slashStringFields()
	var it agent.SlashItem
	v := reflect.ValueOf(&it).Elem()
	for i, p := range paths {
		v.FieldByIndex(p).SetString(name + "-" + strings.ToLower(names[i]))
	}
	it.Name = name
	return it
}

// TestASlashSnapshotCutsTextAndDropsActions (plan 043 P7): a snapshot never
// cuts what a client acts on. For every string a slash row carries — found by
// reflection, so one a later SlashItem gains is tried too — a catalog whose
// middle row holds 5 MiB of it fits a 4 MiB snapshot: a display string (the
// description, the argument hint, the unavailable reason) as its ItemCap head,
// the row kept; any other — the name, the source, the plugin and qualified
// spelling, the requires, every other key of the run — by dropping the row
// whole, the rows either side kept in order. Either way settings.truncated.slash
// says so, a restored model surfaces the mark, the model's own rows are
// untouched, and the next slash section clears the mark. Negative controls:
// capSlash cutting the name instead of dropping the row fails the action
// case for Name; slashActionOver forgetting Run.Local fails it for Run.Local.
func TestASlashSnapshotCutsTextAndDropsActions(t *testing.T) {
	paths, names := slashStringFields()
	if len(paths) < 16 {
		t.Fatalf("fixture: found %d strings in a slash row, want at least 16: %v", len(paths), names)
	}
	big := strings.Repeat("z", huge)
	for i, p := range paths {
		name := names[i]
		t.Run(name, func(t *testing.T) {
			before, after := slashRow("before"), slashRow("after")
			mid := slashRow("mid")
			reflect.ValueOf(&mid).Elem().FieldByIndex(p).SetString(big)
			m := New(Options{})
			foldAll(t, m, true, slashDeltaAt(1, []agent.SlashItem{before, mid, after}))
			s, b := snapshotOf(t, m, capBudget)
			got := s.Settings.Slash
			if !s.Settings.Truncated.Slash {
				t.Fatalf("a 5 MiB %s left no mark", name)
			}
			if slashDisplay[name] {
				cut := mid
				reflect.ValueOf(&cut).Elem().FieldByIndex(p).SetString(big[:ItemCap])
				if !reflect.DeepEqual(got, []agent.SlashItem{before, cut, after}) {
					t.Fatalf("display text %s: the snapshot's rows are not the three with it cut to its head (%d rows)", name, len(got))
				}
			} else if !reflect.DeepEqual(got, []agent.SlashItem{before, after}) {
				t.Fatalf("action field %s: the snapshot's rows are not the two either side of the dropped one (%d rows)", name, len(got))
			}
			if st := m.State().Settings; len(st.Slash) != 3 || reflect.ValueOf(st.Slash[1]).FieldByIndex(p).Len() != huge || st.Truncated.Slash {
				t.Fatal("capping wrote the model's own catalog, or marked it")
			}
			r1, r2 := restoredBoth(t, s, b, Options{})
			for _, r := range []*Model{r1, r2} {
				if st := r.State().Settings; !st.Truncated.Slash || !reflect.DeepEqual(st.Slash, got) {
					t.Fatalf("a restored client's catalog: marked %v, %d rows", st.Truncated.Slash, len(st.Slash))
				}
				r.Fold(agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{Slash: &agent.SlashCatalog{Items: []agent.SlashItem{before}}}, At: at(2), Seq: 2})
				if st := r.State().Settings; st.Truncated.Slash || !reflect.DeepEqual(st.Slash, []agent.SlashItem{before}) {
					t.Fatalf("the next slash section did not replace the catalog and clear its mark: %+v", st.Truncated)
				}
			}
		})
	}
}

// TestASlashSnapshotCarriesAtMostSlashRows (plan 043 P7): a catalog's NUMBER
// of rows is bounded in a snapshot, as no per-item cap bounds it — the first
// SlashRows rows a snapshot may carry, in order, and settings.truncated.slash
// set when any was left out; a row dropped for an action field is not one of
// them. A catalog of exactly SlashRows rows is carried whole and unmarked.
// Negative control: a bound counting the dropped row keeps 511 rows of the
// third case.
func TestASlashSnapshotCarriesAtMostSlashRows(t *testing.T) {
	rows := func(n int) []agent.SlashItem {
		out := make([]agent.SlashItem, n)
		for i := range out {
			out[i] = agent.SlashItem{Name: fmt.Sprintf("r%03d", i), Source: agent.SlashSourceSkill,
				Run: agent.SlashRun{Kind: agent.SlashRunPrompt, Arg: agent.SlashArgOptional}}
		}
		return out
	}
	over := agent.SlashItem{Name: strings.Repeat("n", ItemCap+1), Run: agent.SlashRun{Kind: agent.SlashRunPrompt}}
	for _, tc := range []struct {
		name   string
		items  []agent.SlashItem
		want   []agent.SlashItem
		marked bool
	}{
		{"exactly SlashRows", rows(SlashRows), rows(SlashRows), false},
		{"past SlashRows", rows(SlashRows + 88), rows(SlashRows), true},
		{"a dropped row is not counted", append([]agent.SlashItem{over}, rows(SlashRows)...), rows(SlashRows), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(Options{})
			foldAll(t, m, true, slashDeltaAt(1, tc.items))
			s, b := snapshotOf(t, m, capBudget)
			if got := s.Settings; !reflect.DeepEqual(got.Slash, tc.want) || got.Truncated.Slash != tc.marked {
				t.Fatalf("%d rows marked %v; want %d marked %v", len(got.Slash), got.Truncated.Slash, len(tc.want), tc.marked)
			}
			if len(m.State().Settings.Slash) != len(tc.items) {
				t.Fatal("the bound dropped the model's own rows")
			}
			_, r := restoredBoth(t, s, b, Options{})
			if st := r.State().Settings; len(st.Slash) != len(tc.want) || st.Truncated.Slash != tc.marked {
				t.Fatalf("restored through the codec: %d rows marked %v", len(st.Slash), st.Truncated.Slash)
			}
		})
	}
}

// olderSlash is body as a decoder built before the slash section reads it:
// this decoder, with the section under a key no build knows — exactly what a
// decoder that has no field for "slash" does with it (it skips the key), which
// TestADecoderIgnoresASectionItDoesNotKnow (internal/agent) holds for any
// section. The key appears only as a member name, never inside a string,
// where JSON escapes its quotes.
func olderSlash(body []byte) []byte {
	return bytes.ReplaceAll(body, []byte(`"slash":`), []byte(`"slashOfALaterBuild":`))
}

// TestADecoderThatIgnoresSlashFoldsTheSameState (plan 043 §2.6, acceptance 3:
// shed compatibility): a client that predates the slash section — its decoder
// skips the key, so a slash-only delta reaches its fold as a delta touching
// nothing — folds every other part of the session exactly as a client that
// knows it does: the whole session script, whose deltas carry the catalog
// beside other sections and alone, decoded both ways and folded, gives the
// same history, the same last-ended list and the same state but for the
// catalog itself; and a snapshot of it, decoded both ways and restored, the
// same again. Negative control: a fold row for Slash that also cleared the
// plugin rows goes red at the state comparison.
func TestADecoderThatIgnoresSlashFoldsTheSameState(t *testing.T) {
	evs := sessionScript()
	evs = append(evs, laterScript(len(evs))...)
	evs = append(evs, sequencedAt(slashDeltaAt(60, scriptSlash(agent.Skill{Name: "lint", Description: "alone, a scan's"})), len(evs)+1))
	current, older := New(Options{}), New(Options{})
	sawAlone := false
	for _, ev := range evs {
		body, err := agent.EncodeEvent(ev)
		if err != nil {
			t.Fatal(err)
		}
		cur, err := agent.DecodeEvent(body)
		if err != nil {
			t.Fatal(err)
		}
		old, err := agent.DecodeEvent(string(olderSlash([]byte(body))))
		if err != nil {
			t.Fatalf("an older decoder refused %s: %v", body, err)
		}
		if old.State != nil && *old.State == (agent.StateDelta{}) && cur.State.Slash != nil {
			sawAlone = true
		}
		cur.Seq, old.Seq = ev.Seq, ev.Seq
		current.Fold(cur)
		older.Fold(old)
	}
	if !sawAlone {
		t.Fatal("fixture: the script has no slash-only delta")
	}
	same := func(what string, cur, old *Model) {
		t.Helper()
		cs, os := cur.State(), old.State()
		if len(cs.Settings.Slash) == 0 {
			t.Fatalf("%s: fixture: the current client folded no catalog", what)
		}
		if os.Settings.Slash != nil || os.Settings.Truncated.Slash {
			t.Fatalf("%s: the older client holds a catalog: %+v", what, os.Settings.Slash)
		}
		cs.Settings.Slash = nil
		if !reflect.DeepEqual(cs, os) {
			t.Fatalf("%s: the states differ but for the catalog:\ncurrent %+v\nolder   %+v", what, cs, os)
		}
		if !reflect.DeepEqual(cur.History(), old.History()) {
			t.Fatalf("%s: the histories differ", what)
		}
		if !reflect.DeepEqual(cur.EndedAsks(), old.EndedAsks()) {
			t.Fatalf("%s: the last-ended lists differ", what)
		}
	}
	same("folded", current, older)

	_, b := snapshotOf(t, current, 0)
	cur, err := DecodeSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	old, err := DecodeSnapshot(olderSlash(b))
	if err != nil {
		t.Fatalf("an older decoder refused the snapshot: %v", err)
	}
	same("restored", Restore(cur, Options{}), Restore(old, Options{}))
}

// bigCommands is n advertised commands whose descriptions are each 240 KiB —
// under ItemCap, so nothing caps them — the catalog whose slash rows double
// what the settings carry (plan 043 review W4).
func bigCommands(n int) []agent.CommandInfo {
	out := make([]agent.CommandInfo, n)
	for i := range out {
		out[i] = agent.CommandInfo{Name: fmt.Sprintf("cmd%d", i), Description: strings.Repeat(string(rune('a'+i)), 240<<10)}
	}
	return out
}

// TestTheSlashCatalogYieldsToTheSnapshotBudget (plan 043 review W4): the
// slash catalog never makes a snapshot fail that would succeed without it.
// Nine commands with 240 KiB descriptions — every string under ItemCap, so
// no cap applies — fit a default 4 MiB snapshot on their own, at about 2.2
// MB, which is what a host before the catalog sent; their slash rows repeat
// every description, about 4.4 MB with them. The snapshot leaves the catalog
// out rather than refuse: no settings.slash, settings.truncated.slash set,
// the commands and the transcript whole, and every other cap the first
// attempt made made again — a queued prompt one byte over ItemCap still
// carried as its head and named in truncatedQueue — and a client restored
// from it holds no catalog and the mark until the next slash section, which
// replaces both. At a budget the mark itself does not fit, the snapshot is
// byte for byte the one the same session without a catalog gives. Negative
// controls: snapshot without the retry (build alone) refuses the default
// budget ErrSnapshotTooLarge; a capQueue that cuts the cut's own rows in
// place leaves the retry a short row it does not mark.
func TestTheSlashCatalogYieldsToTheSnapshotBudget(t *testing.T) {
	cmds := bigCommands(9)
	slash := agent.BuildSlashCatalog(agent.SlashInputs{Commands: cmds}).Items
	text := agent.Event{Type: agent.EventText, Text: "the session's reply", At: at(2)}
	long := strings.Repeat("q", ItemCap+1)
	queued := agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q-long", Text: long, QueuedAt: at(3)},
		QueueChange: agent.QueueQueued, At: at(3)}
	before, after := New(Options{}), New(Options{})
	foldAll(t, before, true, sequenced([]agent.Event{
		{Type: agent.EventMeta, State: &agent.StateDelta{Commands: &agent.CommandsState{Commands: cmds}}, At: at(1)}, text, queued,
	})...)
	foldAll(t, after, true, sequenced([]agent.Event{
		{Type: agent.EventMeta, State: &agent.StateDelta{Commands: &agent.CommandsState{Commands: cmds},
			Slash: &agent.SlashCatalog{Items: slash}}, At: at(1)}, text, queued,
	})...)

	// What a host before the catalog sent, and what it sends now.
	old, oldBytes := snapshotOf(t, before, 0)
	if len(old.Settings.Commands) != 9 || len(old.Main.Entries) != 1 {
		t.Fatalf("fixture: the snapshot without a catalog carries %d commands and %d entries", len(old.Settings.Commands), len(old.Main.Entries))
	}
	if !slices.Equal(old.TruncatedQueue, []string{"q-long"}) {
		t.Fatalf("fixture: the snapshot without a catalog marks the queue %v", old.TruncatedQueue)
	}
	s, b := snapshotOf(t, after, 0)
	if st := s.Settings; st.Slash != nil || !st.Truncated.Slash || st.Truncated.Commands ||
		!reflect.DeepEqual(st.Commands, cmds) || len(s.Main.Entries) != 1 {
		t.Fatalf("with the catalog: %d slash rows, marked %v, commands marked %v, %d commands and %d entries",
			len(st.Slash), st.Truncated.Slash, st.Truncated.Commands, len(st.Commands), len(s.Main.Entries))
	}
	if !slices.Equal(s.TruncatedQueue, []string{"q-long"}) || len(s.Queue) != 1 || s.Queue[0].Text != long[:ItemCap] {
		t.Fatalf("the retry lost a cap the first attempt made: truncatedQueue %v, %d rows", s.TruncatedQueue, len(s.Queue))
	}
	if q := after.State().Queue; len(q) != 1 || len(q[0].Text) != ItemCap+1 {
		t.Fatal("capping the queue wrote the model's own row")
	}
	if whole, _ := snapshotOf(t, after, 1<<40); !reflect.DeepEqual(whole.Settings.Slash, slash) || whole.Settings.Truncated.Slash {
		t.Fatal("fixture: given the room, the snapshot does not carry the catalog whole")
	}
	if !reflect.DeepEqual(after.State().Settings.Slash, slash) {
		t.Fatal("leaving the catalog out of the snapshot touched the model's own")
	}
	r1, r2 := restoredBoth(t, s, b, Options{})
	for _, r := range []*Model{r1, r2} {
		if st := r.State().Settings; st.Slash != nil || !st.Truncated.Slash || !reflect.DeepEqual(st.Commands, cmds) {
			t.Fatalf("restored: %d slash rows, marked %v", len(st.Slash), st.Truncated.Slash)
		}
		r.Fold(sequencedAt(slashDeltaAt(3, slash[:2]), 3))
		if st := r.State().Settings; !reflect.DeepEqual(st.Slash, slash[:2]) || st.Truncated.Slash {
			t.Fatalf("the next slash section did not restate the catalog and clear the mark: %d rows, marked %v", len(st.Slash), st.Truncated.Slash)
		}
	}

	// Where even the mark does not fit, the snapshot is the one without a
	// catalog, byte for byte; given the marked snapshot's own size, the mark
	// is carried.
	tight, size, err := after.snapshotSized(len(oldBytes))
	if err != nil {
		t.Fatalf("at the budget the snapshot without a catalog fills exactly: %v", err)
	}
	got, err := EncodeSnapshot(tight)
	if err != nil || !bytes.Equal(got, oldBytes) || size != len(oldBytes) {
		t.Fatalf("at the budget the snapshot without a catalog fills exactly, it is not that snapshot (%d bytes, %v)", len(got), err)
	}
	if marked, _, err := after.snapshotSized(len(b)); err != nil || marked.Settings.Slash != nil || !marked.Settings.Truncated.Slash {
		t.Fatalf("at the marked snapshot's own size: %+v, %v", marked.Settings.Truncated, err)
	}
}

// TestASnapshotKeepsTheSlashRowsItCanRead (plan 043 P17, review W9): a
// snapshot whose settings.slash holds a row a decoder cannot read — a name
// that is a number, a row that is a string — decodes with the readable rows
// alone, and every other section, the transcript included, as it came; a
// settings.slash that is not an object reads as no catalog. The restored
// model holds what was read. Negative control: decodeSettings refusing the
// settings for a bad row (a strict DecodeSlashCatalog) fails the first decode.
func TestASnapshotKeepsTheSlashRowsItCanRead(t *testing.T) {
	m := New(Options{})
	rows := scriptSlash()
	foldAll(t, m, true, sequenced([]agent.Event{
		{Type: agent.EventMeta, State: &agent.StateDelta{Title: strp("kept"),
			Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "review"}}},
			Slash:    &agent.SlashCatalog{Items: rows}}, At: at(1)},
		{Type: agent.EventText, Text: "a reply", At: at(2)},
	})...)
	_, b := snapshotOf(t, m, 0)
	first, err := agent.EncodeSlashCatalog(&agent.SlashCatalog{Items: rows[:1]})
	if err != nil {
		t.Fatal(err)
	}
	// The first row's object, as the section writes it, with bad rows put
	// after it.
	row := strings.TrimSuffix(strings.TrimPrefix(string(first), `{"items":[`), `]}`)
	bad := bytes.Replace(b, []byte(row+","), []byte(row+`,{"name":42},"a string",`), 1)
	if bytes.Equal(bad, b) {
		t.Fatalf("fixture: the snapshot has no row %s to follow: %s", row, b)
	}
	for _, tc := range []struct {
		name string
		body []byte
		want []agent.SlashItem
	}{
		{"bad rows among good ones", bad, rows},
		{"a section that is a number", bytes.Replace(b, []byte(`"slash":{"items":`), []byte(`"slash":5,"later":{"items":`), 1), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := DecodeSnapshot(tc.body)
			if err != nil {
				t.Fatalf("the snapshot was refused: %v", err)
			}
			if st := s.Settings; st.Title != "kept" || len(st.Commands) != 1 || len(s.Main.Entries) != 1 || !reflect.DeepEqual(st.Slash, tc.want) {
				t.Fatalf("decoded: title %q, %d commands, %d entries, %d slash rows (want %d)",
					st.Title, len(st.Commands), len(s.Main.Entries), len(st.Slash), len(tc.want))
			}
			if got := Restore(s, Options{}).State().Settings.Slash; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("restored: %d slash rows, want %d", len(got), len(tc.want))
			}
		})
	}
}

// TestABuildLeavesItsCutAsItFoundIt (plan 043 review of W4): the snapshot
// retries without the slash catalog over the same cut, so no attempt may
// write it. A session with a string past ItemCap in every place a snapshot
// caps one — a queued prompt, a todo, the turn's text, a roster row's prompt
// and output, an open question's title, a command's, a plugin's and a slash
// row's description, a config option, the send-now — built again from a cut
// already built from, at a budget it fits and one it is windowed to, gives
// the snapshot a fresh cut of the model gives, and the cut afterwards is that
// fresh cut. Negative control: a capQueue that cuts the cut's own rows in
// place fails the comparison with the fresh cut's snapshot (truncatedQueue
// lost the row).
func TestABuildLeavesItsCutAsItFoundIt(t *testing.T) {
	over := strings.Repeat("o", ItemCap+1)
	slash := scriptSlash()
	slash[len(slash)-1].Description = over
	m := New(Options{})
	foldAll(t, m, false, sequenced(append(sessionScript(),
		agent.Event{Type: agent.EventMeta, State: &agent.StateDelta{
			Config:   &agent.ConfigState{Options: []agent.ConfigOption{{ID: "effort", Name: over}}},
			Commands: &agent.CommandsState{Commands: []agent.CommandInfo{{Name: "c", Description: over}}},
			Plugins:  &agent.PluginsState{Plugins: []agent.PluginCommand{{Qualified: "p:x", Description: over}}},
			Slash:    &agent.SlashCatalog{Items: slash},
			SendNow:  &agent.SendNowState{Armed: true, Text: over},
		}, At: at(50)},
		agent.Event{Type: agent.EventQueue, Queue: &agent.QueuedPrompt{ID: "q-over", Text: over}, QueueChange: agent.QueueQueued, At: at(51)},
		agent.Event{Type: agent.EventTodos, Todos: []agent.Todo{{ID: "t", Content: over}}, At: at(52)},
		agent.Event{Type: agent.EventSubagent, Subagent: &agent.SubagentInfo{ID: "sub-over", Status: agent.SubagentRunning, Prompt: over, Output: over},
			SubagentChange: agent.SubagentChangeSpawned, At: at(53)},
		agent.Event{Type: agent.EventQuestion, Question: &agent.QuestionEvent{ID: "ask-over", Title: over}, At: at(54)},
		agent.Event{Type: agent.EventTurn, Turn: &agent.TurnInfo{ID: "turn-over", Phase: agent.TurnStarted, Text: over}, At: at(55)},
		agent.Event{Type: agent.EventText, Text: "after", At: at(56)},
	))...)
	c := m.snapshotCut()
	full, _, err := c.build(1<<40, "", c.settings)
	if err != nil {
		t.Fatal(err)
	}
	if !full.TodosTruncated || len(full.TruncatedQueue) == 0 || !full.Turn.Truncated || !full.Settings.Truncated.Slash {
		t.Fatalf("fixture: the caps were not all reached: todos %v, queue %v, turn %v, settings %+v",
			full.TodosTruncated, full.TruncatedQueue, full.Turn.Truncated, full.Settings.Truncated)
	}
	b, err := EncodeSnapshot(full)
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int{1 << 40, len(b) - 200} {
		fresh := m.snapshotCut()
		want, wn, err1 := fresh.build(budget, "", fresh.settings)
		got, gn, err2 := c.build(budget, "", c.settings)
		if err1 != nil || err2 != nil {
			t.Fatalf("budget %d: %v, %v", budget, err1, err2)
		}
		if budget < len(b) && !slices.ContainsFunc(transcriptsOf(want), func(ts *TranscriptSnap) bool { return ts.Windowed }) {
			t.Fatalf("fixture: a budget of %d did not window the snapshot", budget)
		}
		if gn != wn || !reflect.DeepEqual(got, want) {
			t.Fatalf("budget %d: a cut built from before gives another snapshot than a fresh one (%d, %d bytes; truncatedQueue %v, want %v)",
				budget, gn, wn, got.TruncatedQueue, want.TruncatedQueue)
		}
	}
	if fresh := m.snapshotCut(); !reflect.DeepEqual(c, fresh) {
		t.Fatal("a build wrote the cut it was built from")
	}
}
