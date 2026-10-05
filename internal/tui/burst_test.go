package tui

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// Input bursts (SF-40, plan 037 §3.5): bubbletea groups the runes that reach
// it together up to a space into one KeyRunes message, and a non-paste key's
// String() is its bare text, so a burst spelling a key name — "up", "end",
// "home", "delete", "backspace" — matched that key's binding and the word
// vanished into a cursor move or a deletion. Update splits a burst first, into
// one KeyRunes message per extended grapheme cluster (X23), each through the
// command gate exactly as a key typed alone: so a burst is the same keys typed
// one at a time, in the composer, beside its chips, in every input box, and
// under the gate — and a key of several runes (a keycap emoji, a letter and
// its combining accent, a ZWJ sequence) stays one key.

// parserSplit is text as bubbletea's input parser hands it over when it
// arrives in one read (key.go's detectOneMsg): the runes up to a space as one
// KeyRunes message, and each space as its own KeySpace message.
func parserSplit(text string) []tea.KeyMsg {
	var out []tea.KeyMsg
	var word []rune
	flush := func() {
		if len(word) > 0 {
			out = append(out, tea.KeyMsg{Type: tea.KeyRunes, Runes: word})
			word = nil
		}
	}
	for _, r := range text {
		if r == ' ' {
			flush()
			out = append(out, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
			continue
		}
		word = append(word, r)
	}
	flush()
	return out
}

// burstKey is one burst: text in one KeyRunes message, as the parser hands
// over runes that arrived together with no space among them.
func burstKey(text string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)}
}

// typeBurst sends text to m as the parser would hand it over (parserSplit),
// one message per Update.
func typeBurst(t *testing.T, m Model, text string) Model {
	t.Helper()
	for _, k := range parserSplit(text) {
		m, _ = press(m, k)
	}
	return m
}

// keyNameWords is a draft made of words that spell key names.
const keyNameWords = "go up home end delete backspace"

// The keys of several runes the cases type: a keycap one (1, VS16, the
// combining keycap), a decomposed Á (A and a combining acute), and a family
// emoji (three people joined by ZWJ).
const (
	keycapOne  = "1\uFE0F\u20E3"
	decomposed = "A\u0301"
	zwjFamily  = "\U0001F468\u200D\U0001F469\u200D\U0001F467"
)

// TestBurstKeys: a KeyRunes message of more than one grapheme cluster,
// neither a paste nor Alt+…, is a burst, split one key per cluster with every
// rune of the message kept; anything else — one cluster of several runes
// included — is no burst.
func TestBurstKeys(t *testing.T) {
	for _, tc := range []struct {
		name string
		k    tea.KeyMsg
		want []string
	}{
		{"several runes", burstKey("up"), []string{"u", "p"}},
		{"clusters of several runes", burstKey("x" + keycapOne + decomposed + zwjFamily + "e\u0301"),
			[]string{"x", keycapOne, decomposed, zwjFamily, "e\u0301"}},
		{"one rune", burstKey("u"), nil},
		{"one keycap", burstKey(keycapOne), nil},
		{"one decomposed letter", burstKey(decomposed), nil},
		{"one ZWJ sequence", burstKey(zwjFamily), nil},
		{"a paste", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("up"), Paste: true}, nil},
		{"alt", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("up"), Alt: true}, nil},
		{"a space", tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}, nil},
		{"a real key", tea.KeyMsg{Type: tea.KeyUp}, nil},
	} {
		var got []string
		for _, k := range burstKeys(tc.k) {
			if k.Type != tea.KeyRunes || k.Alt || k.Paste {
				t.Errorf("%s: a key %+v is not a plain KeyRunes", tc.name, k)
			}
			got = append(got, string(k.Runes))
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: burstKeys = %q, want %q", tc.name, got, tc.want)
		}
	}
	// A rune no string can hold is kept as it came, not made U+FFFD.
	odd := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a', 0xD800, 'b'}}
	var runes []rune
	for _, k := range burstKeys(odd) {
		runes = append(runes, k.Runes...)
	}
	if !slices.Equal(runes, odd.Runes) {
		t.Errorf("a surrogate's burst came back as %U, want %U", runes, odd.Runes)
	}
}

// TestAGraphemeIsOneKey (X23): a key of several runes — a keycap 1, a
// decomposed Á — typed on an open permission card answers nothing, as before
// SF-40: split by rune its first rune would be "1" (allow once) or "A" (allow
// always). Inside a burst, a ZWJ emoji and combining text reach the composer
// whole, as slow typing puts them there.
func TestAGraphemeIsOneKey(t *testing.T) {
	for _, key := range []string{keycapOne, decomposed, "x" + keycapOne} {
		m, stub := sizedCards(t)
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(true)})
		// The fixture's gate runs a card's answer inline (gateSync): a key
		// that answered has answered by the time press returns.
		m, _ = press(m, burstKey(key))
		if headAsk(m) != "perm-1" || len(stub.Asks().Resolved()) != 0 || len(stub.Asks().Asks()) != 1 {
			t.Fatalf("%q on a permission card: head %q, resolved %q", key, headAsk(m), resolvedAsks(stub))
		}
	}
	text := "hi" + zwjFamily + "e\u0301" + keycapOne + decomposed + "!"
	burst, _ := press(sized(t), burstKey(text))
	slow := typeComposer(t, sized(t), text)
	if got := burst.input.Value(); got != text || got != slow.input.Value() {
		t.Fatalf("the burst left %q, slow typing %q; want %q", got, slow.input.Value(), text)
	}
	if burst.composerCursorOffset() != len(text) || slow.composerCursorOffset() != len(text) {
		t.Fatalf("the cursor: %d after the burst, %d after slow typing; want %d", burst.composerCursorOffset(), slow.composerCursorOffset(), len(text))
	}
}

// TestABurstIsTypedAsItsKeys: the parser's split of words that spell key
// names leaves the composer holding the text, the cursor at its end — the
// live repro's sentence included — exactly as typing it a key at a time does.
func TestABurstIsTypedAsItsKeys(t *testing.T) {
	for _, text := range []string{keyNameWords, "go up the tree and the end of it", "left right down"} {
		m := typeBurst(t, sized(t), text)
		if got := m.input.Value(); got != text {
			t.Fatalf("the burst of %q left the composer holding %q", text, got)
		}
		if off := m.composerCursorOffset(); off != len(text) {
			t.Fatalf("the burst of %q left the cursor at %d, want the end, %d", text, off, len(text))
		}
		slow := typeComposer(t, sized(t), text)
		if slow.input.Value() != m.input.Value() || slow.composerCursorOffset() != m.composerCursorOffset() {
			t.Fatalf("the burst and slow typing differ: %q at %d, %q at %d",
				m.input.Value(), m.composerCursorOffset(), slow.input.Value(), slow.composerCursorOffset())
		}
	}
}

// TestABurstInsertsAtTheCursor: the same burst typed with the cursor in the
// middle of a draft is inserted there, and the cursor follows it.
func TestABurstInsertsAtTheCursor(t *testing.T) {
	m := caret(sized(t), "hello world", len("hello"))
	m = typeBurst(t, m, " up home end")
	if got, want := m.input.Value(), "hello up home end world"; got != want {
		t.Fatalf("the draft is %q, want %q", got, want)
	}
	if off, want := m.composerCursorOffset(), len("hello up home end"); off != want {
		t.Fatalf("the cursor is at %d, want %d, after the burst", off, want)
	}
}

// TestABurstBesideAChipRemovesNoChip: a burst spelling backspace right after
// an image chip, or delete right before one, is text — the chip and its image
// stay — where the real keys take the whole chip (TestBackspaceAndDeleteTake
// AWholeChip).
func TestABurstBesideAChipRemovesNoChip(t *testing.T) {
	m, _ := seeChip(t)
	m, _ = press(m, burstKey("backspace"))
	if got := m.input.Value(); got != "see [Image #1]backspace" || len(m.images.list) != 1 {
		t.Fatalf("a burst of backspace after the chip: %q, %d images", got, len(m.images.list))
	}

	m, _ = seeChip(t)
	for range len("[Image #1]") {
		m = pressKey(t, m, tea.KeyLeft)
	}
	m, _ = press(m, burstKey("delete"))
	if got := m.input.Value(); got != "see delete[Image #1]" || len(m.images.list) != 1 {
		t.Fatalf("a burst of delete before the chip: %q, %d images", got, len(m.images.list))
	}
}

// TestABurstOfUnicodeArrivesWhole: a burst of multi-byte runes arrives
// intact.
func TestABurstOfUnicodeArrivesWhole(t *testing.T) {
	m := typeBurst(t, sized(t), "héllo 日本 ü")
	if got := m.input.Value(); got != "héllo 日本 ü" {
		t.Fatalf("the composer holds %q", got)
	}
	if off := m.composerCursorOffset(); off != len("héllo 日本 ü") {
		t.Fatalf("the cursor is at byte %d, want the end", off)
	}
}

// modelFilter is m with /model open, its filter empty.
func modelFilter(t *testing.T, m Model) Model {
	t.Helper()
	m.input.SetValue("/model")
	m, _ = press(m, enter())
	if m.dialog != dialogModel {
		t.Fatal("fixture: /model opened no dialog")
	}
	return m
}

// TestABurstStopsAtTheCharLimit: a burst into an input with a limit — the
// model dialog's filter, 64 runes — stops at the limit exactly where slow
// typing stops, and a key name inside the limit is text.
func TestABurstStopsAtTheCharLimit(t *testing.T) {
	fill := strings.Repeat("x", 60)
	burst := modelFilter(t, sized(t))
	burst = typeInto(t, burst, fill)
	burst, _ = press(burst, burstKey("home"))
	burst, _ = press(burst, burstKey("endless"))
	slow := modelFilter(t, sized(t))
	slow = typeInto(t, slow, fill+"home"+"endless")
	got, want := burst.mdlg.filter.Value(), slow.mdlg.filter.Value()
	if got != want || want != fill+"home" {
		t.Fatalf("the burst left the filter %q, slow typing %q; want %q", got, want, fill+"home")
	}
	if burst.mdlg.filter.Position() != slow.mdlg.filter.Position() {
		t.Fatalf("the cursor: %d after the burst, %d after slow typing", burst.mdlg.filter.Position(), slow.mdlg.filter.Position())
	}
}

// TestRealKeysStillNavigate: the keys a burst used to be taken for still do
// their work when they are real keys: Home, End, ←, Delete, Backspace and ↑.
func TestRealKeysStillNavigate(t *testing.T) {
	m := draft(sized(t), "one two")
	if m = pressKey(t, m, tea.KeyHome); m.composerCursorOffset() != 0 {
		t.Fatalf("Home left the cursor at %d", m.composerCursorOffset())
	}
	if m = pressKey(t, m, tea.KeyEnd); m.composerCursorOffset() != len("one two") {
		t.Fatalf("End left the cursor at %d", m.composerCursorOffset())
	}
	for range 3 {
		m = pressKey(t, m, tea.KeyLeft)
	}
	if m = pressKey(t, m, tea.KeyDelete); m.input.Value() != "one wo" {
		t.Fatalf("Delete left %q", m.input.Value())
	}
	if m = pressKey(t, m, tea.KeyBackspace); m.input.Value() != "onewo" {
		t.Fatalf("Backspace left %q", m.input.Value())
	}
	m = draft(sized(t), "first\nsecond")
	if m = pressKey(t, m, tea.KeyUp); m.input.Line() != 0 {
		t.Fatalf("↑ in a two-line draft left the cursor on line %d", m.input.Line())
	}
}

// TestAPasteIsStillAPaste: a bracketed paste of key-name words goes in as one
// paste, and a pasted image path still becomes a chip; neither is a burst.
func TestAPasteIsStillAPaste(t *testing.T) {
	m := pasteText(t, sized(t), "up home")
	if got := m.input.Value(); got != "up home" {
		t.Fatalf("the paste left %q", got)
	}
	img, _ := imageModel(t)
	img = pasteText(t, img, shotPNG(t))
	if got := img.input.Value(); got != "[Image #1]" || len(img.images.list) != 1 {
		t.Fatalf("an image path's paste left %q, %d images", got, len(img.images.list))
	}
}

// TestABurstIntoEveryInputBox: the session list's input, the model filter,
// /connect's key field (masked: read from the model) and the sign-in's
// redirect field each take a burst of key-name words as text, as slow typing
// does — and ← spelled out on an empty composer opens no list.
func TestABurstIntoEveryInputBox(t *testing.T) {
	t.Run("the session list's input", func(t *testing.T) {
		m, fs, _ := newSessModel(t, 100, 30)
		m = newList(t, m, fs)
		m = typeBurst(t, m, keyNameWords)
		if v, cur := inputOf(m); v != keyNameWords || cur != len(keyNameWords) || !m.sessList.open {
			t.Fatalf("the list's input %q, cursor %d, open %v", v, cur, m.sessList.open)
		}
	})
	t.Run("the model filter", func(t *testing.T) {
		m := typeBurst(t, modelFilter(t, sized(t)), "up end")
		if got := m.mdlg.filter.Value(); got != "up end" || m.dialog != dialogModel {
			t.Fatalf("the filter %q, dialog %v", got, m.dialog)
		}
	})
	t.Run("the connect key field", func(t *testing.T) {
		dir, getenv := connectFixture(t, nil)
		m := connectKeyStep(t, nativeStub(), dir, getenv)
		for _, w := range []string{"sk-", "home", "end", "left", "delete"} {
			m, _ = press(m, burstKey(w))
		}
		if got := m.cdlg.key.Value(); got != "sk-homeendleftdelete" || m.cdlg.step != connectKey {
			t.Fatalf("the key field holds %d bytes, step %d", len(got), m.cdlg.step)
		}
	})
	t.Run("the sign-in's redirect field", func(t *testing.T) {
		m, _ := signInModel(t, false)
		m, _ = beginStep(t, m)
		for _, w := range []string{"code", "home", "end", "backspace"} {
			m, _ = press(m, burstKey(w))
		}
		if got := m.cdlg.key.Value(); got != "codehomeendbackspace" || m.cdlg.step != connectSignIn {
			t.Fatalf("the redirect field holds %d bytes, step %d", len(got), m.cdlg.step)
		}
	})
	t.Run("left on an empty composer", func(t *testing.T) {
		m, _, _ := newSessModel(t, 100, 30)
		m, _ = press(m, burstKey("left"))
		if m.sessList.open || m.input.Value() != "left" {
			t.Fatalf("a burst spelling left: list open %v, composer %q", m.sessList.open, m.input.Value())
		}
	})
}

// ------------------------------------------------------------ under the gate

// update is one message through Model.Update — the production entry, the
// burst split first — with its commands sorted as send sorts them.
func (r *gateRig) update(msg tea.Msg) {
	r.t.Helper()
	tm, cmd := r.m.Update(msg)
	r.m = tm.(Model)
	r.sort(cmd)
}

// heldRunes is the held queue's keys, as text.
func heldRunes(m Model) string {
	var b strings.Builder
	for _, h := range m.held {
		if k, ok := h.msg.(tea.KeyMsg); ok {
			b.WriteString(string(k.Runes))
		}
	}
	return b.String()
}

// twoPermissionCards is a gated model with two permission cards queued,
// perm-1 at the head.
func twoPermissionCards(t *testing.T) (*gateRig, *Stub) {
	t.Helper()
	m, stub := gatedModel(t)
	for _, id := range []string{"perm-1", "perm-2"} {
		p := stubPermissionEvent(false)
		p.ID = id
		m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: p})
	}
	if len(m.cards) != 2 || headAsk(m) != "perm-1" {
		t.Fatalf("fixture: %d cards, head %q", len(m.cards), headAsk(m))
	}
	return newGateRig(t, m), stub
}

// resolvedAsks is the ids of the asks the session has resolved, in order,
// with the option each was answered with.
func resolvedAsks(stub *Stub) []string {
	var out []string
	for _, r := range stub.Asks().Resolved() {
		out = append(out, r.ID+"="+r.Answer.OptionID)
	}
	return out
}

// TestABurstAnswersTwoCardsInOrder (round 2's panic): a burst "yy" over two
// queued permission cards answers both, in order. The first y's answer opens
// the gate; the second y is held — not handed to the second card while the
// first card's gate is open, which would issue a second gated call and panic
// (gate.go's run) — and drained once the gate closes, where it answers the
// second card.
func TestABurstAnswersTwoCardsInOrder(t *testing.T) {
	r, stub := twoPermissionCards(t)
	r.update(burstKey("yy"))
	if r.m.gate == nil || len(r.calls) != 1 || heldRunes(r.m) != "y" {
		t.Fatalf("after the burst: gate %v, %d calls, held %q — want the first answer in flight and the second y held",
			r.m.gate != nil, len(r.calls), heldRunes(r.m))
	}
	if headAsk(r.m) != "perm-2" {
		t.Fatalf("the head card is %q while the first answer is in flight", headAsk(r.m))
	}
	r.answer()
	r.drainAll()
	if r.m.gate == nil || len(r.calls) != 1 || len(r.m.held) != 0 {
		t.Fatalf("the held y did not answer the second card: gate %v, %d calls, held %d", r.m.gate != nil, len(r.calls), len(r.m.held))
	}
	r.answer()
	r.drainAll()
	if got, want := resolvedAsks(stub), []string{"perm-1=opt-once", "perm-2=opt-once"}; !slices.Equal(got, want) {
		t.Fatalf("resolved %q, want %q", got, want)
	}
	if r.m.cardOpen() || len(texts(r.m, entryError)) != 0 {
		t.Fatalf("after both answers: cards %d, errors %q", len(r.m.cards), texts(r.m, entryError))
	}
}

// refuseFirstAnswer is a backend whose first Answer is refused, as a session
// refuses an answer it cannot take; every other call is the backend's.
type refuseFirstAnswer struct {
	backend.Backend
	refused atomic.Bool
}

func (b *refuseFirstAnswer) Answer(ctx context.Context, c engine.Command, id string, a agent.AskAnswer) error {
	if b.refused.CompareAndSwap(false, true) {
		return agent.ErrBadAnswer
	}
	return b.Backend.Answer(ctx, c, id, a)
}

// TestARefusedAnswerMidBurst: the first y's answer is refused — its card back
// at the head, the refusal an error row — and the held second y, drained
// after it, answers that card again, as a second y typed slowly would: the
// second card stays up for a key of its own.
func TestARefusedAnswerMidBurst(t *testing.T) {
	r, stub := twoPermissionCards(t)
	r.m.eng = &refuseFirstAnswer{Backend: r.m.eng}
	r.update(burstKey("yy"))
	if heldRunes(r.m) != "y" {
		t.Fatalf("the second y is not held: %q", heldRunes(r.m))
	}
	r.answer()
	if headAsk(r.m) != "perm-1" || len(texts(r.m, entryError)) != 1 {
		t.Fatalf("after the refusal: head %q, errors %q", headAsk(r.m), texts(r.m, entryError))
	}
	r.drainAll()
	r.answer()
	r.drainAll()
	if got, want := resolvedAsks(stub), []string{"perm-1=opt-once"}; !slices.Equal(got, want) {
		t.Fatalf("resolved %q, want %q", got, want)
	}
	if headAsk(r.m) != "perm-2" || len(r.m.cards) != 1 {
		t.Fatalf("the second card: head %q, %d cards", headAsk(r.m), len(r.m.cards))
	}
}

// TestABurstOpensTheSlashMenuAsSlowTypingDoes: "/mo" in one burst opens the
// slash menu at its first rune and filters it with the rest, to the same
// menu and the same frame as typing it a key at a time.
func TestABurstOpensTheSlashMenuAsSlowTypingDoes(t *testing.T) {
	// Two models: a copy shares its composer's buffer with the original.
	slow, _ := gatedModel(t)
	slow = typeComposer(t, slow, "/mo")
	burst, _ := gatedModel(t)
	burst, _ = press(burst, burstKey("/mo"))
	if burst.input.Value() != "/mo" || !slices.Equal(slashNames(burst), slashNames(slow)) || len(slashNames(burst)) == 0 {
		t.Fatalf("the burst: %q, menu %q; slow typing: %q, menu %q", burst.input.Value(), slashNames(burst), slow.input.Value(), slashNames(slow))
	}
	if b, s := plainView(burst), plainView(slow); b != s {
		t.Fatalf("the frames differ:\nburst:\n%s\nslow:\n%s", b, s)
	}
}

// TestABurstJoinsTheKeysAlreadyHeld: a burst that arrives while keys are held
// — behind an open gate, or while the held queue drains with no gate open —
// is held behind them, rune by rune, and applied in arrival order.
func TestABurstJoinsTheKeysAlreadyHeld(t *testing.T) {
	m, stub := gatedModel(t)
	m = cardEvent(t, m, stub, agent.Event{Type: agent.EventPermission, Permission: stubPermissionEvent(false)})
	r := newGateRig(t, m)
	r.update(runeKey('y'))
	if r.m.gate == nil {
		t.Fatal("fixture: the answer opened no gate")
	}
	r.update(runeKey('a'))
	r.update(burstKey("up"))
	if got := heldRunes(r.m); got != "aup" || len(r.m.held) != 3 {
		t.Fatalf("held %q in %d messages, want a, u, p", got, len(r.m.held))
	}
	r.answer()
	r.drain()
	if r.m.gate != nil || heldRunes(r.m) != "up" || r.m.input.Value() != "a" {
		t.Fatalf("one drain in: gate %v, held %q, composer %q", r.m.gate != nil, heldRunes(r.m), r.m.input.Value())
	}
	r.update(burstKey("end"))
	if got := heldRunes(r.m); got != "upend" {
		t.Fatalf("a burst while the queue drains: held %q", got)
	}
	r.drainAll()
	if got := r.m.input.Value(); got != "aupend" || len(r.m.held) != 0 {
		t.Fatalf("the composer holds %q, %d held", got, len(r.m.held))
	}
}
