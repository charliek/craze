package control

import (
	"encoding/json"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/transcript"
)

// The documents the server builds from the engine's reads (plan 027 §3.3,
// §3.13). Every list goes out as [] and never null, settings.config as {}
// when there are no options, and only codec output is embedded raw.

// sessionCapabilities is the session capability set on the wire: every
// agent.Capabilities field under its wire name, plus what every host of
// protocol 1 states — cancel, approvals and historyCursor, and stop false,
// which is the provider's answer: stop is the host's own, and a server that
// serves session.stop turns it on in the document it builds
// (sessionInfoReady; plan 030 §3.6a), as a server with the row facts turns on
// rowFacts (§3.8), which is omitted while false. TestEveryCapabilityIsOnTheWire
// holds every agent.Capabilities field to a wire name here and in the schema,
// so a field added to the struct fails the gate until it is mapped.
func sessionCapabilities(c agent.Capabilities) protocol.SessionCapabilities {
	return protocol.SessionCapabilities{
		Interject:           c.Interject,
		SubagentCancel:      c.SubagentCancel,
		SubagentBackground:  c.SubagentBackground,
		Modes:               c.Modes,
		Effort:              c.Effort,
		FastToggle:          c.FastToggle,
		SubagentRows:        c.SubagentRows,
		SubagentTranscript:  c.SubagentTranscript,
		Todos:               c.Todos,
		AskCards:            c.AskCards,
		PlanCards:           c.PlanCards,
		ParameterizedPicker: c.ParameterizedPicker,
		Cancel:              true,
		Approvals:           true,
		HistoryCursor:       true,
		Stop:                false,
	}
}

// codecs is the two codecs' versions, hello's codecs.
func codecs() protocol.Codecs {
	return protocol.Codecs{Event: agent.EventCodecVersion, Snapshot: transcript.SnapshotVersion}
}

// retryHorizon is the receipts table's bound on the wire.
func retryHorizon(h engine.RetryHorizon) protocol.RetryHorizon {
	return protocol.RetryHorizon{Commands: h.Commands, AgeMs: h.Age.Milliseconds()}
}

// sessionInfo is the session info document (§3.3, §3.13) and the State it was
// read with: the one builder the attach reply, the ready notification
// (attach.go) and sessions.list share.
func (s *Server) sessionInfo(eng *engine.Engine) (protocol.SessionInfo, engine.State) {
	info, st, _ := s.sessionInfoReady(eng)
	return info, st
}

// sessionInfoReady is sessionInfo and whether the session was ready when it
// was read — the one reading of Ready the document's catalogs were decided by,
// which the attach reply's ready says. Readiness is read BEFORE the state, so
// catalogs go out only from a state read after the session was up: "empty
// until ready".
func (s *Server) sessionInfoReady(eng *engine.Engine) (protocol.SessionInfo, engine.State, bool) {
	ready := false
	select {
	case <-eng.Ready():
		ready = true
	default:
	}
	st := eng.State()
	info := protocol.SessionInfo{
		SessionID:         st.CrazeSessionID,
		ProviderSessionID: st.SessionID,
		Incarnation:       st.Incarnation,
		HostID:            s.hostID,
		Workspace:         s.opts.Workspace,
		Provider:          protocol.Provider{Name: st.Provider.Name, Label: st.Provider.Label()},
		Catalogs:          protocol.Catalogs{Models: []protocol.CatalogModel{}, Modes: []protocol.CatalogMode{}},
		Capabilities:      sessionCapabilities(st.Provider.Capabilities()),
		RetryHorizon:      retryHorizon(st.RetryHorizon),
		// The host's, each left out when the host does not set it (plan 030
		// X1): an older host's document, and the fake host's by default.
		PermissionMode: s.opts.PermissionMode,
		StartedAt:      s.opts.StartedAt.UTC(),
	}
	// stop is this server's: true where it serves session.stop (stop.go).
	info.Capabilities.Stop = s.opts.Stop != nil
	// rowFacts is too: its sessions.list row carries the row facts (rowFacts,
	// plan 030 §3.8), and a document of a server without them leaves it out.
	info.Capabilities.RowFacts = s.opts.RowFacts
	if ready {
		for _, m := range st.Models {
			info.Catalogs.Models = append(info.Catalogs.Models, protocol.CatalogModel{ID: m.ID, Name: m.Name})
		}
		for _, m := range st.Modes {
			info.Catalogs.Modes = append(info.Catalogs.Modes, protocol.CatalogMode{ID: m.ID, Name: m.Name, Description: m.Description})
		}
	}
	return info, st, ready
}

// headAsk is the head open ask, nil when none is open.
func headAsk(st engine.State) *protocol.HeadAsk {
	if st.PendingAsks == 0 || st.HeadAsk.Kind == "" {
		return nil
	}
	return &protocol.HeadAsk{ID: st.HeadAsk.ID, Kind: string(st.HeadAsk.Kind), Label: st.HeadAsk.Label}
}

// rowFacts puts the row facts on row (plan 030 §3.8, §3.10): the engine's,
// computed now from st — the State the row was built from, so the facts
// describe the row they go on — each left out when unset. Since goes on the
// wire in UTC, as startedAt does.
func rowFacts(row *protocol.SessionRow, eng *engine.Engine, st engine.State) {
	f := eng.RowFacts(st)
	if row.HeadAsk != nil {
		row.HeadAsk.Summary = f.Summary
	}
	row.Doing, row.LastReply = f.Doing, f.LastReply
	if !f.Since.IsZero() {
		row.Since = f.Since.UTC()
	}
	row.StartFailed, row.StartErr = st.StartFailed, f.StartErr
	row.Prompted = st.Prompted
}

// stateResult is session.state's result: State's projection and the host's
// live settings (§3.3; §3.12's Settings). foreignTurn is State's own
// (Snapshot.ForeignTurn): nothing here reads the session's flag (X1, X39).
func stateResult(st engine.State) (protocol.StateResult, error) {
	queue, err := queueRows(st.Queue)
	if err != nil {
		return protocol.StateResult{}, err
	}
	config, err := agent.EncodeConfigState(&agent.ConfigState{Options: st.Config})
	if err != nil {
		return protocol.StateResult{}, err
	}
	// The usage section, absent when the session has none (every ACP session;
	// a native one before its first step): nil encodes to no key at all.
	usage, err := agent.EncodeUsageState(st.Usage)
	if err != nil {
		return protocol.StateResult{}, err
	}
	r := protocol.StateResult{
		Activity:    protocol.Activity(st.Activity),
		ForeignTurn: st.ForeignTurn,
		Turn:        st.Turn,
		Waiting:     st.Waiting,
		Queue:       queue,
		PendingAsks: st.PendingAsks,
		HeadAsk:     headAsk(st),
		Err:         st.Err,
		StartFailed: st.StartFailed,
		Prompted:    st.Prompted,
		Cancelled:   st.Cancelled,
		Settings:    protocol.Settings{Model: st.CurrentModel, Mode: st.CurrentMode, Config: config, Usage: usage},
	}
	if a := st.SendNow; a != nil {
		r.SendNow = &protocol.ArmedSend{Text: a.Text, FromRow: a.FromRow, Turn: a.Turn, Cause: a.Cause}
	}
	r.LastTurn = lastTurn(st)
	return r, nil
}

// lastTurn is State.LastTurn on the wire (plan 030 §3.7): session.state's and
// a sessions.list row's, nil — no member at all — while a turn runs, before
// any has ended, and so for every host that never had one.
func lastTurn(st engine.State) *protocol.LastTurn {
	lt := st.LastTurn
	if lt == nil {
		return nil
	}
	return &protocol.LastTurn{Outcome: protocol.TurnOutcome(lt.Outcome), Err: lt.Err, EndedAt: lt.EndedAt.UTC(), TurnID: lt.TurnID}
}

// queueRows is each row as the event codec writes a queue row.
func queueRows(rows []agent.QueuedPrompt) ([]json.RawMessage, error) {
	out := make([]json.RawMessage, 0, len(rows))
	for _, q := range rows {
		raw, err := agent.EncodeQueuedPrompt(q)
		if err != nil {
			return nil, err
		}
		out = append(out, raw)
	}
	return out, nil
}

// askSummary is one open ask as asks.list carries it (§3.2): no body.
func askSummary(r agent.AskRecord) protocol.AskSummary {
	return protocol.AskSummary{ID: r.ID, Kind: string(r.Kind), Label: r.Label(), OpenedAt: r.OpenedAt.UTC()}
}

// askRecord is one ask as asks.get carries it (§3.2, X6): its body's strings
// capped at the snapshot's ItemCap by the snapshot's own rule
// (transcript.CapAskBody), truncated set when one was cut. The answer is
// carried for an ask a client answered, and absent otherwise.
func askRecord(r agent.AskRecord) (protocol.AskRecord, error) {
	capped, cut := transcript.CapAskBody(r.Body)
	body, err := encodeAskBody(capped)
	if err != nil {
		return protocol.AskRecord{}, err
	}
	rec := protocol.AskRecord{
		ID: r.ID, Kind: string(r.Kind), Status: protocol.AskStatus(r.Status),
		Body: body, OpenedAt: r.OpenedAt.UTC(), Truncated: cut,
	}
	if r.Status == agent.AskResolved {
		rec.Outcome, rec.By, rec.ResolvedAt = string(r.Outcome), r.By, r.ResolvedAt.UTC()
		if r.Outcome == agent.AskAnswered || !answerIsZero(r.Answer) {
			a := r.Answer
			rec.Answer = &protocol.Answer{OptionID: a.OptionID, Cancel: a.Cancel, Answers: a.Answers,
				Skip: a.Skip, Accept: a.Accept, Reject: a.Reject}
		}
	}
	return rec, nil
}

func answerIsZero(a agent.AskAnswer) bool {
	return a.OptionID == "" && !a.Cancel && a.Answers == nil && !a.Skip && !a.Accept && !a.Reject
}

// wireAskBody is an ask's opening on the wire (event.json's askBody): each of
// the three payloads as the event codec's leaf wrapper writes it — the
// composition the snapshot codec makes for an open ask's body.
type wireAskBody struct {
	Permission json.RawMessage `json:"permission,omitempty"`
	Question   json.RawMessage `json:"question,omitempty"`
	Plan       json.RawMessage `json:"plan,omitempty"`
}

func encodeAskBody(b agent.AskBody) (json.RawMessage, error) {
	var w wireAskBody
	var err error
	if w.Permission, err = agent.EncodePermissionEvent(b.Permission); err != nil {
		return nil, err
	}
	if w.Question, err = agent.EncodeQuestionEvent(b.Question); err != nil {
		return nil, err
	}
	if w.Plan, err = agent.EncodePlanEvent(b.Plan); err != nil {
		return nil, err
	}
	return rawJSON(w)
}
