package tui

import (
	"context"
	"fmt"
	"hash/fnv"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
)

// The command gate's invisibility watch (plan 027 §3.12 (d),
// TestTheGateIsInvisible). TestMain installs it package-wide, as it installs
// the parity watch, so every gate any test opens — a unit fixture's, the
// pump's, every frame golden's async run — is held to it: from the end of the
// Update that opened the gate to the Update its reply lands in, the model's
// state does not change. The issuing Update's own work (the pre-call half of
// the handler) is allowed; nothing after it is, until the continuation.
//
// It compares a digest of the state, never View's bytes, which read the clock
// (astra 5): every field of the model View and the parity watch read, minus
// the gate's own bookkeeping and the handles that are not the model's state
// (gateDigestSkips).

// gateWatch is the package's one watch.
var gateWatch = &gateInvisibility{opened: map[*gate]gateDigest{}, expect: map[*gate]*gateBreak{}}

func installGateWatch() { gateHook = gateWatch.hook }

// gateInvisibility holds the digest each open gate was issued with.
type gateInvisibility struct {
	mu     sync.Mutex
	opened map[*gate]gateDigest
	// expect is the gates a test has said will be broken on purpose
	// (TestTheInvisibilityWatchSeesAHeldMutation): their breaks are recorded
	// there, not failed.
	expect map[*gate]*gateBreak
	// checks counts the comparisons made, so a test can say the watch looked.
	checks int
	failed error
}

// gateBreak is what the watch saw change under a gate a test expected broken.
type gateBreak struct {
	phase  gatePhase
	fields []string
}

func (w *gateInvisibility) hook(phase gatePhase, m *Model) {
	g := m.gate
	if phase == gateOpened {
		d := digestModel(m)
		w.mu.Lock()
		w.opened[g] = d
		w.mu.Unlock()
		return
	}
	d := digestModel(m)
	w.mu.Lock()
	defer w.mu.Unlock()
	want, ok := w.opened[g]
	if !ok {
		// A gate this watch never saw open: one a test built by hand. Nothing
		// to hold it to.
		return
	}
	w.checks++
	if phase == gateReleasing {
		delete(w.opened, g)
	}
	changed := want.diff(d)
	if len(changed) == 0 {
		return
	}
	if b, ok := w.expect[g]; ok {
		if b.phase == 0 {
			b.phase, b.fields = phase, changed
		}
		return
	}
	err := fmt.Errorf("the command gate is not invisible (plan 027 §3.12 (d)): %s changed %s while gate %d was open",
		phaseName(phase), strings.Join(changed, ", "), g.id)
	if w.failed == nil {
		w.failed = err
	}
	panic(err)
}

func phaseName(p gatePhase) string {
	switch p {
	case gateHeld:
		return "a held message"
	case gateReleasing:
		return "the release, before its continuation,"
	}
	return fmt.Sprintf("phase %d", p)
}

// expectBreak records g's breaks for the caller instead of failing the run.
func (w *gateInvisibility) expectBreak(g *gate) *gateBreak {
	w.mu.Lock()
	defer w.mu.Unlock()
	b := &gateBreak{}
	w.expect[g] = b
	return b
}

func (w *gateInvisibility) forget(g *gate) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.expect, g)
	delete(w.opened, g)
}

func (w *gateInvisibility) checked() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.checks
}

func (w *gateInvisibility) err() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

// gateDigest is a model's state, field by field: a hash of each digested
// field, so a break names what moved.
type gateDigest map[string]uint64

func (d gateDigest) diff(o gateDigest) []string {
	var out []string
	for k, v := range d {
		if o[k] != v {
			out = append(out, k)
		}
	}
	for k := range o {
		if _, ok := d[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// gateDigestSkips is every Model field the digest leaves out, and why. Every
// other field is digested, by reflection, so a field added to Model is held
// still under a gate unless it is named here (TestTheGateDigestSkipsOnlyNamedFields).
var gateDigestSkips = map[string]string{
	// The gate's own bookkeeping (§3.12 (d)).
	"gate":        "the gate itself",
	"gateSeq":     "the gate's numbering",
	"held":        "the held queue",
	"heldBytes":   "the held queue's bytes",
	"heldDrained": "the held queue's drained slots",
	"reading":     "the reader's flag: an event arriving while gated moves it",
	"syncPending": "the frame harness's pending token",
	"gateSync":    "the gate's mode",
	// Handles, not state: what they point at lives on other goroutines, or is
	// a function.
	"eng":          "the backend (its own goroutines)",
	"owner":        "the session owner (a lock, read by the exit tails)",
	"shell":        "the shell controller (its own goroutine and lock)",
	"term":         "the terminal colours' writer",
	"chains":       "the model-change chain lock",
	"host":         "the host hub",
	"sessionIndex": "the index store",
	"clock":        "a function",
	"onEngine":     "a function",
	"newSession":   "a function",
	"loadSession":  "a function",
	"spawnNew":     "a function",
	"spawnLoad":    "a function",
	"claimSession": "a function",
	"refuseLoad":   "a function",
	"foldIn":       "the fold's per-event scratch (the fold itself is shared's digest)",
}

// digestModel is m's state digest. Most fields are walked by reflection; a few
// hold bubbles components or the shared transcript, whose internals include a
// lock or a cache View fills, and are digested through their accessors.
func digestModel(m *Model) gateDigest {
	d := gateDigest{}
	v := reflect.ValueOf(m).Elem()
	ty := v.Type()
	w := &digestWriter{path: map[uintptr]bool{}}
	for i := range ty.NumField() {
		f := ty.Field(i)
		if _, skip := gateDigestSkips[f.Name]; skip {
			continue
		}
		w.buf = w.buf[:0]
		switch f.Name {
		case "input":
			w.buf = fmt.Appendf(w.buf, "%q %d %+v %v %d %d %q", m.input.Value(), m.input.Line(), m.input.LineInfo(),
				m.input.Focused(), m.input.Height(), m.input.Width(), m.input.Placeholder)
		case "mdlg":
			w.buf = fmt.Appendf(w.buf, "%q %d %v ", m.mdlg.filter.Value(), m.mdlg.filter.Position(), m.mdlg.filter.Focused())
			w.walk(reflect.ValueOf(m.mdlg.sel))
			w.walk(reflect.ValueOf(m.mdlg.focus))
			w.walk(reflect.ValueOf(m.mdlg.chosen))
			w.walk(reflect.ValueOf(m.mdlg.touched))
		case "shared":
			digestShared(w, m)
		default:
			w.walk(v.Field(i))
		}
		h := fnv.New64a()
		_, _ = h.Write(w.buf)
		d[f.Name] = h.Sum64()
	}
	return d
}

// digestShared is the shared transcript as a frame and the parity watch read
// it: its position and state, and every transcript's every entry — kind, text,
// payload, times, the open stream's tail materialised — with each
// transcript's trim, window and stream flags and todo counts (History), the
// endings of the asks it has seen (EndedAsks), and each transcript's size and
// tail. An entry rewritten in place, to text of the same length, moves it
// (astra C17 5).
func digestShared(w *digestWriter, m *Model) {
	s := m.shared
	if s == nil {
		w.str("nil")
		return
	}
	w.buf = fmt.Appendf(w.buf, "seq=%d incarnation=%q ", s.Seq(), s.Incarnation())
	w.walk(reflect.ValueOf(s.State()))
	w.walk(reflect.ValueOf(s.History()))
	w.walk(reflect.ValueOf(s.EndedAsks()))
	for _, id := range append([]string{""}, s.Subs()...) {
		tr := s.Main
		if id != "" {
			tr = s.Sub(id)
		}
		if tr == nil {
			continue
		}
		w.buf = fmt.Appendf(w.buf, "|%s len=%d bytes=%d tail=%q", id, tr.Len(), tr.Bytes(), tr.Tail())
	}
}

// digestWriter walks a value into a buffer: every scalar, every string,
// through pointers, slices, maps (in key order) and interfaces — never into a
// type whose internals another goroutine may write or View may fill. A pointer
// is expanded wherever it is reached, and only a cycle (a pointer already on
// the path to it) is cut, so the digest does not depend on a map's iteration
// order.
type digestWriter struct {
	buf  []byte
	path map[uintptr]bool
}

var (
	digestOpaque = map[reflect.Type]bool{
		reflect.TypeFor[sync.Mutex]():      true,
		reflect.TypeFor[sync.RWMutex]():    true,
		reflect.TypeFor[sync.Once]():       true,
		reflect.TypeFor[lipgloss.Style]():  true,
		reflect.TypeFor[textarea.Model]():  true,
		reflect.TypeFor[textinput.Model](): true,
		reflect.TypeFor[*time.Location]():  true,
	}
	contextType = reflect.TypeFor[context.Context]()
)

func (w *digestWriter) str(s string) { w.buf = append(w.buf, s...) }

func (w *digestWriter) walk(v reflect.Value) {
	if !v.IsValid() {
		w.str("<invalid>")
		return
	}
	if digestOpaque[v.Type()] || v.Type() == contextType {
		w.str("<opaque>")
		return
	}
	switch v.Kind() {
	case reflect.Bool:
		w.buf = strconv.AppendBool(w.buf, v.Bool())
		w.str(";")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		w.buf = strconv.AppendInt(w.buf, v.Int(), 10)
		w.str(";")
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		w.buf = strconv.AppendUint(w.buf, v.Uint(), 10)
		w.str(";")
	case reflect.Float32, reflect.Float64:
		w.buf = strconv.AppendFloat(w.buf, v.Float(), 'g', -1, 64)
		w.str(";")
	case reflect.Complex64, reflect.Complex128:
		w.buf = fmt.Appendf(w.buf, "%v;", v.Complex())
	case reflect.String:
		w.buf = strconv.AppendInt(w.buf, int64(v.Len()), 10)
		w.str(":")
		w.str(v.String())
		w.str(";")
	case reflect.Func, reflect.Chan, reflect.UnsafePointer:
		if v.IsNil() {
			w.str("fn(nil);")
		} else {
			w.str("fn;")
		}
	case reflect.Pointer:
		if v.IsNil() {
			w.str("nil;")
			return
		}
		p := v.Pointer()
		if w.path[p] {
			w.str("@cycle;")
			return
		}
		w.path[p] = true
		w.str("&")
		w.walk(v.Elem())
		delete(w.path, p)
	case reflect.Interface:
		if v.IsNil() {
			w.str("nil;")
			return
		}
		w.str("(")
		w.str(v.Elem().Type().String())
		w.str(")")
		w.walk(v.Elem())
	case reflect.Slice, reflect.Array:
		if v.Kind() == reflect.Slice && v.IsNil() {
			w.str("nil;")
			return
		}
		w.str("[")
		w.buf = strconv.AppendInt(w.buf, int64(v.Len()), 10)
		w.str(":")
		for i := range v.Len() {
			w.walk(v.Index(i))
		}
		w.str("]")
	case reflect.Map:
		if v.IsNil() {
			w.str("nil;")
			return
		}
		type kv struct{ k, v string }
		var entries []kv
		it := v.MapRange()
		for it.Next() {
			kw := &digestWriter{path: w.path}
			kw.walk(it.Key())
			vw := &digestWriter{path: w.path}
			vw.walk(it.Value())
			entries = append(entries, kv{string(kw.buf), string(vw.buf)})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].k < entries[j].k })
		w.str("{")
		w.buf = strconv.AppendInt(w.buf, int64(len(entries)), 10)
		w.str(":")
		for _, e := range entries {
			w.str(e.k)
			w.str("=")
			w.str(e.v)
			w.str(",")
		}
		w.str("}")
	case reflect.Struct:
		w.str("{")
		for i := range v.NumField() {
			w.walk(v.Field(i))
		}
		w.str("}")
	}
}
