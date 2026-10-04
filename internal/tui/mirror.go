package tui

import (
	"slices"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/transcript"
)

// The mirror (plan 027 §3.13, "The mirror onto the fold").
//
// What the TUI draws of the session's state — m.snap, m.queue and the armed
// send-now — is a derived value, and it comes from three places, never from
// the engine's live memory:
//
//  1. m.shared, this client's own fold of the stream: the settings (title,
//     mode, model, config, the catalogs of commands and plugins, the armed
//     send-now, and — once a native session has reloaded its table — the
//     models it offers), the todo list, the roster, the ordered tools, the
//     queue and the agent's own turn (transcript.Model.Mirror);
//  2. Backend.Info(), the session's facts: the provider — by name, for its
//     local vocabulary; its capabilities come from Info through caps() — the
//     provider's session id, and the model and mode catalogs, the model
//     catalog only while it is newer than the one the fold carries (plan 034
//     §3.4, C4r);
//  3. the overlays below: what this client's own commands confirmed that the
//     fold has not caught up with yet.
//
// recompute builds it after every fold (reduceEvent) and after every overlay
// write, and may be called mid-handler: code that reads the mirror right after
// writing an overlay sees the overlay (astra 18). m.snap keeps agent.Snapshot's
// field names, so every reader of it is unchanged.

// recompute derives m.snap, m.queue and m.sendNowArmed from the fold, Info and
// the overlays, and settles what follows from them as the live refresh it
// replaces did: m.model, the model dialog's repair, and the sub-agent rows'
// first-sight stamps. The settings overlays whose confirmation the fold has
// now reached are retired first (retireReached). With no backend there is
// nothing to derive from, and the mirror is left as it is (a bare Model a test
// built).
func (m *Model) recompute() {
	if m.eng == nil {
		return
	}
	// The backend's facts — or, while a restore is applied, its item's own
	// (Model.info).
	info := m.info()
	var f transcript.Mirror
	if m.shared != nil {
		f = m.shared.Mirror()
	}
	m.retireReached(f.Settings)
	// The models the session offers: of the fold's catalog section — once a
	// delta has carried one, a native session that reloaded its table (plan
	// 034 §3.4) — and Info's list, the newer by revision; at the same
	// revision they are one list, and the fold's is taken, ordered with every
	// other section this client folds. Info is read live in process, so it
	// can be ahead of the stream: a session that published two revisions
	// before this client folded the first shows the second from Info, and
	// the first, folded after, must not bring back what the second took away
	// (C4r, r9 #8). Over the socket Info's revision is 0 until the wire
	// carries it (C5), and the fold's list is taken whenever there is one.
	models := info.Models
	if c := f.Settings.Catalog; c != nil && c.Revision >= info.CatalogRevision {
		models = c.Models
	}
	snap := agent.Snapshot{
		Models:       models,
		Modes:        info.Modes,
		Commands:     f.Settings.Commands,
		Config:       f.Settings.Config,
		Tools:        f.Tools,
		Subagents:    f.Agents,
		Todos:        f.Todos,
		Title:        f.Settings.Title,
		SessionID:    info.ProviderSessionID,
		CurrentModel: f.Settings.Model,
		CurrentMode:  f.Settings.Mode,
		Provider:     providerNamed(info.Provider),
		ForeignTurn:  f.Turn.Foreign != nil && f.Turn.Foreign.Running,
		Plugins:      f.Settings.Plugins,
		// The session's usage (plan 028 §3.14): the status row reads it from
		// the fold like every other settings section.
		Usage: f.Settings.Usage,
	}
	queue := f.Queue
	// The fold ends the arm on the started that fires it (SF-55), and carries
	// each child's activity as the live session moves it (SF-54): both are
	// the fold's word as it stands.
	armed := f.Settings.SendNow.Armed
	m.ov.apply(&snap, &queue, &armed)
	// A catalog whose models change under the open model dialog (a refresh,
	// another client's key) keeps the selection on the model it was on, or on
	// the connect row (plan 034 §3.4): the box's selection is an index, and a
	// list that grew or lost a model would otherwise move it, or leave it on
	// one the person did not choose. A catalog with the same models in
	// another order — the current model moves to the top — keeps the index,
	// as it always has.
	selID, onRow, selected := "", false, false
	if m.dialog == dialogModel && !sameModelSet(m.snap.Models, snap.Models) {
		list := m.dialogModelList()
		switch {
		case m.mdlg.connect && m.mdlg.sel == len(list):
			onRow, selected = true, true
		case m.mdlg.sel >= 0 && m.mdlg.sel < len(list):
			selID, selected = list[m.mdlg.sel].ID, true
		}
	}
	m.snap, m.queue, m.sendNowArmed = snap, queue, armed
	if selected {
		list := m.dialogModelList()
		switch {
		case onRow:
			m.mdlg.sel = len(list)
		case m.mdlg.sel >= len(list) || list[m.mdlg.sel].ID != selID:
			if i := slices.IndexFunc(list, func(md agent.ModelInfo) bool { return md.ID == selID }); i >= 0 {
				m.mdlg.sel = i
			} else {
				m.mdlg.sel = max(min(m.mdlg.sel, m.modelListRows(list)-1), 0)
			}
		}
	}
	// The host's word on its permission mode and its start (plan 030 §3.7),
	// read with the rest of its facts, so the status rows move with them.
	m.hostPerm, m.hostStart = info.PermissionMode, info.StartedAt

	if m.snap.CurrentModel != "" {
		m.model = m.snap.CurrentModel
	}
	if m.dialog == dialogModel {
		// The model dialog's tabs are this snapshot's catalog, which a delta
		// can change under the open box — another client's model change
		// brings another model's options. A focused tab whose option has gone
		// hands the focus back to the list for good, rather than taking it
		// back if the option returns (plan 025 design 4).
		m.mdlg = m.mdlg.repaired(m.modelDialogTabs())
	}
	// Stamp the rows on first sight in the mirror, not only on the lifecycle
	// event: a tool re-emit can carry a finished status one Update ahead of
	// the `finished` event, and a finished row without its stamp would drop
	// out of the band for that frame and move the selection under the user.
	for i := range m.snap.Subagents {
		s := &m.snap.Subagents[i]
		if subagentTerminal(*s) {
			m.noteAgentDone(s.ID)
		} else {
			m.noteAgentStart(s.ID)
		}
	}
}

// providerNamed is the provider Info names, for its local vocabulary — what a
// mode id means, the implement prompt, the label (§3.13): read from this
// binary's provider table by name, a documented, protocol-stable vocabulary
// per provider. "" is the zero value, as a session that has not reported one
// answers (it reads as cursor's); a name this binary does not know is that
// name with no vocabulary of its own — today's zero-value behaviour.
func providerNamed(name string) agent.ProviderInfo {
	if name == "" {
		return agent.ProviderInfo{}
	}
	p, err := agent.ProviderByName(name)
	if err != nil {
		return agent.ProviderInfo{Name: name}
	}
	return p.Info()
}

// sendNowPending reports whether a send-now is armed, as the mirror says: the
// fold's send-now section, under this client's own
// Submit and Disarm result overlays (§3.13, "Live reads that decide
// behaviour") — so the Esc ladder sees this client's own arm or disarm at
// once, as the live read it replaces did.
func (m Model) sendNowPending() bool {
	return m.eng != nil && m.sendNowArmed
}

// ---------------------------------------------------------------- observing

// observe is what one folded event does to the mirror's own bookkeeping,
// before recompute reads the fold: the settings sections' revisions and the
// overlays the event retires — a settings
// overlay by its own delta, a command-result overlay by its command's own
// event for its item (§3.12, "Retirement is by the command's own event for
// that item").
func (m *Model) observe(ev agent.Event) {
	switch ev.Type {
	case agent.EventMeta:
		st := ev.State
		if st == nil {
			return
		}
		// The highest StateDelta Seq applied per settings section: what a
		// confirmation's revision is judged against (retireReached, mayApply).
		// It is recorded for every delta, this model's own included: an echo
		// is still a change that has been applied, and the revision has to be
		// able to overtake a request issued before it.
		if st.Mode != nil && ev.Seq > m.modeRev {
			m.modeRev = ev.Seq
		}
		if st.Model != nil && ev.Seq > m.modelRev {
			m.modelRev = ev.Seq
		}
		if st.Config != nil && ev.Seq > m.configRev {
			m.configRev = ev.Seq
		}
		m.ov = m.ov.retireDelta(ev.Cause, st)
	case agent.EventQueue:
		if ev.Queue != nil {
			m.ov = m.ov.retireRow(ev.Cause, ev.Queue.ID)
		}
	}
}

// ---------------------------------------------------------------- overlays

// overlays are this client's own overlays on the fold (§3.13's inventory):
//
//   - the settings overlays — the mode, the model, and one per config option —
//     each written by this client's own request, optimistically when it is
//     sent and with its confirmed value once it answers (settingEntry);
//   - the command-result overlays of §3.12 — a queue row present, absent or
//     edited, the send-now armed or not, the title — each installed from a
//     gated command's result (resultEntry).
//
// Every entry names the command it came from (its Cause), and is retired by
// that command's own event for its item: equality with the fold is never the
// test (the ABA cases, §3.12). A settings entry is also retired by its
// refusal, by a newer entry for the same key, and once confirmed by the fold's
// revision for its section reaching the confirmation's (retireReached). A
// restore — a new session — clears them all.
//
// It is a value: every write returns a new one, on fresh slices, because every
// copy of Model shares whatever slices it holds.
type overlays struct {
	mode    settingEntry
	model   settingEntry
	config  []settingEntry
	results []resultEntry
}

// settingEntry is one settings overlay (§3.13's mode, model and config
// values).
type settingEntry struct {
	// set says the entry exists.
	set bool
	// gen is the request it belongs to — m.modeGen for the mode, m.applyGen
	// for the model and the options — which is what its answer is matched on.
	gen int
	// cause is the command whose delta retires it: that delta is the change
	// the session committed for this request, at the revision its answer
	// names (engine.SetResult.Rev), so the fold applying it and the fold's
	// revision reaching Rev are one event.
	cause string
	// id is a config entry's option id.
	id string
	// value is what the entry shows: the value asked for until the request is
	// answered, the value the session confirmed once it is.
	value string
	// at is the section's revision when the request was sent (mayApply).
	at uint64
	// confirmed says the request was answered with success, and rev is the
	// revision it answered with (engine.SetResult.Rev, 0 when none was
	// learned: settled then has no revision to wait for).
	confirmed bool
	rev       uint64
}

// settled reports whether a confirmed entry has nothing left to show that the
// fold does not: something newer than the confirmation has been applied to its
// section (mayApply's rule, extended from the three revisions to every
// overlay: plan 027 §3.13), or the fold has reached the confirmation — its
// revision applied, or, with no revision to wait for (Rev == 0, the change's
// delta number was never learned), at any point — and shows the confirmed
// value. applied is the section's revision now, and folded the value the fold
// shows. An unconfirmed entry is never settled: only its answer, or its own
// delta (retireDelta), ends it.
func (e settingEntry) settled(applied uint64, folded string) bool {
	if !e.confirmed {
		return false
	}
	if !mayApply(applied, e.at, e.rev) {
		return true
	}
	return (e.rev == 0 || applied >= e.rev) && folded == e.value
}

// resultKind is what a command-result overlay asserts (§3.12's table).
type resultKind int

const (
	// resultRowPresent: row R is in the queue (Submit that queued R).
	resultRowPresent resultKind = iota + 1
	// resultRowAbsent: row R is not (Submit that started a turn from R,
	// Unqueue, each row ClearQueue removed).
	resultRowAbsent
	// resultRowEdited: row R holds the new text (EditQueued).
	resultRowEdited
	// resultArmed and resultDisarmed: the send-now is armed (Submit that
	// armed one), or not (Disarm).
	resultArmed
	resultDisarmed
	// resultTitle: the title is the new name (SetTitle).
	resultTitle
)

// resultEntry is one command-result overlay: what kind of fact, the command
// that confirmed it, and the item — a queue row, the send-now, the title.
type resultEntry struct {
	kind  resultKind
	cause string
	// row is the queue item: for present the row itself, for edited its id,
	// new text and version, for absent its id.
	row agent.QueuedPrompt
	// title is the title a resultTitle entry shows.
	title string
}

// onRow reports whether the entry is about queue row id.
func (r resultEntry) onRow(id string) bool {
	switch r.kind {
	case resultRowPresent, resultRowAbsent, resultRowEdited:
		return r.row.ID == id
	}
	return false
}

// inDelta reports whether state delta st carries the entry's item: the
// send-now section for an arm or a disarm, the title for a title.
func (r resultEntry) inDelta(st *agent.StateDelta) bool {
	switch r.kind {
	case resultArmed, resultDisarmed:
		return st.SendNow != nil
	case resultTitle:
		return st.Title != nil
	}
	return false
}

// rowAbsent is the "row id absent" entry of the command sent as cause
// (§3.12's table: a Submit that started a turn from the row, an Unqueue, each
// row a ClearQueue removed).
func rowAbsent(cause, id string) resultEntry {
	return resultEntry{kind: resultRowAbsent, cause: cause, row: agent.QueuedPrompt{ID: id}}
}

// apply lays the overlays over the fold's facts, in place on the values
// recompute is building: the settings first, then the command results in the
// order they were installed, each over what the ones before it left.
func (o overlays) apply(snap *agent.Snapshot, queue *[]agent.QueuedPrompt, armed *bool) {
	if o.mode.set {
		snap.CurrentMode = o.mode.value
	}
	if o.model.set {
		snap.CurrentModel = o.model.value
	}
	if len(o.config) > 0 {
		cfg := slices.Clone(snap.Config)
		for _, e := range o.config {
			for i := range cfg {
				if cfg[i].ID == e.id {
					cfg[i].Current = e.value
				}
			}
		}
		snap.Config = cfg
	}
	if len(o.results) == 0 {
		return
	}
	q := slices.Clone(*queue)
	for _, r := range o.results {
		switch r.kind {
		case resultRowPresent:
			if !slices.ContainsFunc(q, func(p agent.QueuedPrompt) bool { return p.ID == r.row.ID }) {
				// A queued row joins the back: Submit's queue mode appends.
				q = append(q, r.row)
			}
		case resultRowAbsent:
			q = slices.DeleteFunc(q, func(p agent.QueuedPrompt) bool { return p.ID == r.row.ID })
		case resultRowEdited:
			for i := range q {
				if q[i].ID == r.row.ID {
					q[i].Text, q[i].Version = r.row.Text, r.row.Version
				}
			}
		case resultArmed:
			*armed = true
		case resultDisarmed:
			*armed = false
		case resultTitle:
			snap.Title = r.title
		}
	}
	if len(q) == 0 {
		q = nil
	}
	*queue = q
}

// retireDelta is the overlays after a state delta caused by cause: every
// entry that delta is the command's own event for — a settings entry whose
// section it carries, the send-now's result overlays when it carries the
// send-now section, the title's when it carries the title — is retired.
// Nothing is retired by a delta with no cause, or by one another command
// caused, whatever value it carries (the ABA cases, §3.12).
func (o overlays) retireDelta(cause string, st *agent.StateDelta) overlays {
	if cause == "" {
		return o
	}
	if st.Mode != nil && o.mode.set && o.mode.cause == cause {
		o.mode = settingEntry{}
	}
	if st.Model != nil && o.model.set && o.model.cause == cause {
		o.model = settingEntry{}
	}
	if st.Config != nil {
		o.config = dropEntries(o.config, func(e settingEntry) bool { return e.cause == cause })
	}
	o.results = dropEntries(o.results, func(r resultEntry) bool { return r.cause == cause && r.inDelta(st) })
	return o
}

// retireRow is the overlays after a queue event for row id caused by cause:
// that command's own event for that row, whatever it says — a queued, an
// edit, a removal or a send — retires its entry for the row, and only that
// one. So a clear's entry for each row it removed lasts until that row's own
// removal is folded, and a Submit's "absent" for the row it started lasts only
// until the row's sent — or the queued that restores it, when the agent's own
// turn refused that turn (X1): both carry the Submit's cause.
func (o overlays) retireRow(cause, id string) overlays {
	if cause == "" {
		return o
	}
	o.results = dropEntries(o.results, func(r resultEntry) bool { return r.cause == cause && r.onRow(id) })
	return o
}

// withResult is the overlays with one more command-result entry, after every
// one installed before it.
func (o overlays) withResult(r resultEntry) overlays {
	o.results = append(slices.Clip(o.results), r)
	return o
}

// dropEntries is es without the entries drop names — the command results or
// the config entries — on a fresh slice when any goes (nil when none is left)
// and es itself when none does.
func dropEntries[E any](es []E, drop func(E) bool) []E {
	if !slices.ContainsFunc(es, drop) {
		return es
	}
	out := slices.DeleteFunc(slices.Clone(es), drop)
	if len(out) == 0 {
		return nil
	}
	return out
}

// configEntry is the config entry for option id, and where it is.
func (o overlays) configEntry(id string) (settingEntry, int) {
	for i, e := range o.config {
		if e.id == id {
			return e, i
		}
	}
	return settingEntry{}, -1
}

// withConfig is the overlays with e as option e.id's entry, replacing the one
// there — unless that one belongs to a newer request, whose entry stands.
func (o overlays) withConfig(e settingEntry) overlays {
	cur, i := o.configEntry(e.id)
	if i >= 0 && cur.gen > e.gen {
		return o
	}
	cfg := slices.Clone(o.config)
	if i >= 0 {
		cfg[i] = e
	} else {
		cfg = append(cfg, e)
	}
	o.config = cfg
	return o
}

// retireReached drops every confirmed settings entry the fold has caught up
// with (settingEntry.settled), against the section revisions observe keeps and
// the fold's settings s.
func (m *Model) retireReached(s transcript.Settings) {
	if m.ov.mode.settled(m.modeRev, s.Mode) {
		m.ov.mode = settingEntry{}
	}
	if m.ov.model.settled(m.modelRev, s.Model) {
		m.ov.model = settingEntry{}
	}
	m.ov.config = dropEntries(m.ov.config, func(e settingEntry) bool {
		return e.settled(m.configRev, optionValue(s.Config, e.id))
	})
}

// optionValue is option id's current value in cfg, "" when cfg has no such
// option.
func optionValue(cfg []agent.ConfigOption, id string) string {
	for _, o := range cfg {
		if o.ID == id {
			return o.Current
		}
	}
	return ""
}

// ------------------------------------------------ the settings overlays' writes

// requestMode is the mode overlay for a SetMode this client is sending as c:
// the chip shows id from now until the request is refused, a newer one
// replaces it, or its own delta is folded.
func (m *Model) requestMode(gen int, cause, id string) {
	m.ov.mode = settingEntry{set: true, gen: gen, cause: cause, value: id, at: m.modeRev}
	m.recompute()
}

// confirmMode is SetMode request gen answered with success, confirming value
// at revision rev: the chip holds it until the fold's mode revision reaches rev
// — or, with rev 0 (the change published nothing), until the fold shows it or
// something newer is applied — checked here too, since the delta may already
// be folded (settled; §3.13: modeSettled no longer takes the chip down on
// success). Its own delta retires it as well (retireDelta). An answer for a
// request whose entry is gone — replaced by a newer one, or already retired by
// its delta — changes nothing.
func (m *Model) confirmMode(gen int, value string, rev uint64) {
	if e := m.ov.mode; e.set && e.gen == gen {
		e.value, e.confirmed, e.rev = value, true, rev
		m.ov.mode = e
	}
	m.recompute()
}

// refuseMode is SetMode request gen refused: its own entry goes, and nothing
// is put back — the chip reads the fold (SF-38's rule, for the mode).
func (m *Model) refuseMode(gen int) {
	if e := m.ov.mode; e.set && e.gen == gen {
		m.ov.mode = settingEntry{}
	}
	m.recompute()
}

// requestModel is the model overlay for a model change this client is sending
// as cause, from request gen (m.applyGen).
func (m *Model) requestModel(gen int, cause, id string) {
	m.ov.model = settingEntry{set: true, gen: gen, cause: cause, value: id, at: m.modelRev}
	m.recompute()
}

// confirmModel is a model step of request gen, sent as cause when the model
// section stood at at, answered with success: value, confirmed at rev. It
// becomes the model overlay — the request's own entry, confirmed, or a new
// one where its entry is already gone — and holds until the fold reaches it;
// the check is made here too, since the delta may already be folded
// (settled). A newer request's entry stands.
func (m *Model) confirmModel(gen int, cause, value string, at, rev uint64) {
	e := settingEntry{set: true, gen: gen, cause: cause, value: value, at: at, confirmed: true, rev: rev}
	switch cur := m.ov.model; {
	case cur.set && cur.gen > gen:
	case e.settled(m.modelRev, m.foldedSettings().Model):
		if cur.set && cur.gen == gen {
			m.ov.model = settingEntry{}
		}
	default:
		m.ov.model = e
	}
	m.recompute()
}

// foldedSettings is the fold's settings now.
func (m *Model) foldedSettings() transcript.Settings {
	if m.shared == nil {
		return transcript.Settings{}
	}
	return m.shared.Mirror().Settings
}

// refuseModel is model request gen refused, or answered with nothing the
// session could read: its own entry goes, and nothing is put back (SF-38).
func (m *Model) refuseModel(gen int) {
	if e := m.ov.model; e.set && e.gen == gen {
		m.ov.model = settingEntry{}
	}
	m.recompute()
}

// requestOption is the config overlay for option id's change to value, sent
// as cause from request gen.
func (m *Model) requestOption(gen int, cause, id, value string) {
	m.ov = m.ov.withConfig(settingEntry{set: true, gen: gen, cause: cause, id: id, value: value, at: m.configRev})
	m.recompute()
}

// confirmOption is option id's change, sent as cause from request gen when
// the config section stood at at, answered with success: value confirmed at
// rev. It becomes the option's overlay — the request's own entry, confirmed,
// or a new one for a step that had none (/model's effort, or a dialog step
// re-resolved onto another option of the destination's catalog) — unless the
// fold has already caught up with it (settled) or a newer request's entry
// stands (withConfig).
func (m *Model) confirmOption(gen int, cause, id, value string, at, rev uint64) {
	e := settingEntry{set: true, gen: gen, cause: cause, id: id, value: value, at: at, confirmed: true, rev: rev}
	if e.settled(m.configRev, optionValue(m.foldedSettings().Config, id)) {
		m.ov.config = dropEntries(m.ov.config, func(c settingEntry) bool { return c.id == id && c.gen == gen })
	} else {
		m.ov = m.ov.withConfig(e)
	}
	m.recompute()
}

// dropUnconfirmed is every settings entry of request gen — its model's and
// its options' — that its answer did not confirm: a step refused, skipped, or
// never sent once an earlier one failed — nothing is put back.
func (m *Model) dropUnconfirmed(gen int) {
	if e := m.ov.model; e.set && e.gen == gen && !e.confirmed {
		m.ov.model = settingEntry{}
	}
	m.ov.config = dropEntries(m.ov.config, func(e settingEntry) bool { return e.gen == gen && !e.confirmed })
	m.recompute()
}

// ------------------------------------------ the command-result overlays' writes

// noteResult installs one command-result overlay (§3.12's table) and redraws
// from it. A command with no cause names no event that could retire it, so it
// installs nothing.
func (m *Model) noteResult(r resultEntry) {
	if r.cause == "" {
		return
	}
	m.ov = m.ov.withResult(r)
	m.recompute()
}

// clearOverlays drops every overlay: the session they were about is gone (a
// restore, a new session).
func (m *Model) clearOverlays() {
	m.ov = overlays{}
}

// sameModelSet reports whether a and b offer the same model ids, in any order.
func sameModelSet(a, b []agent.ModelInfo) bool {
	if len(a) != len(b) {
		return false
	}
	ids := make(map[string]struct{}, len(a))
	for _, md := range a {
		ids[md.ID] = struct{}{}
	}
	for _, md := range b {
		if _, ok := ids[md.ID]; !ok {
			return false
		}
	}
	return true
}
