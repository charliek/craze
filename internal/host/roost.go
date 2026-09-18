package host

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/charliek/craze/internal/version"
)

// Fixed identity and wording of craze's roost reports (plan 015 §3.4). source
// is roost's open string, rendered verbatim; craze never borrows "cursor" or
// "grok", which would collide with roost's own adapters for those agents.
const (
	roostSource       = "craze"
	roostOp           = "tab.agent_report"
	roostTitle        = "craze"
	roostTurnComplete = "Turn complete"
	// roostProviderKey follows roost's `<product>.` rule for a metadata key
	// one product owns; model and version are roost's own bare keys.
	roostProviderKey = "craze.provider"
)

// blockedFallback is a Blocked body for a card whose header is empty. roost
// rejects attention=set with an empty body, so a card must never reach it
// without one; failedFallback plays the same part for Failed.
const blockedFallback = "needs input"

// Roost reports craze's state to the roost tab it runs in, through
// tab.agent_report, the one op every roost agent adapter writes (plan 015
// §3.4). Ownership is (source, session_id): craze claims the tab for each ACP
// session, reports under preserve, and releases it at exit.
//
// Report and Release are only ever called by one Hub worker goroutine, never
// concurrently, so the bookkeeping below needs no lock of its own.
type Roost struct {
	socket string
	tabID  string // ROOST_TAB_ID in canonical decimal

	// claimed is the session a claim was acknowledged for — ok and accepted.
	// A Report for any other session claims first, so a claim lost to a
	// timeout is sent again. It is cleared when roost answers that the tab has
	// no owner at all, and deliberately kept when another owner holds the tab:
	// a manual override stands until craze has a new session to claim for.
	claimed string
	// claimAttempt is the session of the last claim sent, acknowledged or not.
	// Release hands that session back, and sends nothing while it is empty.
	claimAttempt string

	// stated is whether a state has been attempted since the last claim, and
	// last is that state. A claim puts roost's lifecycle back to inactive, so
	// whatever was stated before it no longer stands and is stated again.
	stated bool
	last   roostState
	// lifecycle is the lifecycle craze last attempted on roost: inactive from a
	// claim, then whatever each state line set. A finished line is sent only
	// while it is working or waiting — a stop or a cancel is news only for a
	// turn craze last said was running, and never right after a claim, which
	// would raise a "Turn complete" for a turn roost did not see. This is
	// roost's own lifecycle_if guard done on craze's side: roost-session 0.0.19
	// rejects lifecycle_if as an unknown field.
	lifecycle string
	// model is the model last attempted in a metadata map.
	model string
}

// roostState is what decides whether a status is a new state line.
type roostState struct {
	kind            Kind
	message, detail string
}

// NewRoostFromEnv builds a Roost iff roost's own hook gate is met (plan 015
// §3.4): ROOST_SOCKET is non-empty and ROOST_TAB_ID is a positive int64. ok is
// false, with a nil *Roost, otherwise.
//
// The tab id is parsed exactly as roost's hook entrypoints parse it
// (crates/roost-agent/src/hook.rs parse_tab_id: Rust's str::parse::<i64>,
// then > 0). strconv.ParseInt with base 10 accepts and rejects the same
// strings: an optional leading '+' or '-', ASCII digits, leading zeros
// allowed; no surrounding whitespace, no underscores, nothing out of range.
// The id is sent in canonical form, as roost's hook re-serialises it.
func NewRoostFromEnv(getenv func(string) string) (r *Roost, ok bool) {
	socket := getenv("ROOST_SOCKET")
	if socket == "" {
		return nil, false
	}
	id, err := strconv.ParseInt(getenv("ROOST_TAB_ID"), 10, 64)
	if err != nil || id <= 0 {
		return nil, false
	}
	return &Roost{socket: socket, tabID: strconv.FormatInt(id, 10)}, true
}

// Name implements Reporter.
func (r *Roost) Name() string { return "roost" }

type roostRequest struct {
	ID     string      `json:"id"`
	Op     string      `json:"op"`
	Params roostParams `json:"params"`
}

// roostParams is roost's TabAgentReportParams as roost-session 0.0.19 accepts
// it. roost denies unknown fields, so nothing may be added here that the
// oldest roost craze supports does not have — lifecycle_if, which newer roost
// accepts, is deliberately absent (see Roost.lifecycle). Every optional
// key is omitted when empty, so an absent lifecycle means "unchanged", an
// absent attention means "preserve", and a title or body is only ever on the
// wire with a value — the builders below give every attention=set line both.
type roostParams struct {
	TabID           string            `json:"tab_id"`
	Source          string            `json:"source"`
	SessionID       string            `json:"session_id"`
	OwnershipAction string            `json:"ownership_action"`
	Lifecycle       string            `json:"lifecycle,omitempty"`
	Attention       string            `json:"attention,omitempty"`
	Severity        string            `json:"severity,omitempty"`
	Title           string            `json:"title,omitempty"`
	Body            string            `json:"body,omitempty"`
	Detail          string            `json:"detail,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

type roostReply struct {
	ID     string `json:"id"`
	OK     *bool  `json:"ok"`
	Result *struct {
		Accepted *bool `json:"accepted"`
		Tab      *struct {
			Ownership *struct {
				Source    string `json:"source"`
				SessionID string `json:"session_id"`
			} `json:"ownership"`
		} `json:"tab"`
	} `json:"result"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Report sends, in order and each only when needed: a claim for a session not
// yet claimed; the state line for a status whose state differs from the last
// one attempted, with the model folded in when it changed; or, with no state
// line, a metadata line for a model change alone (plan 015 §3.4). Nothing is
// sent without a session id: a claim nothing can match would strand the tab.
func (r *Roost) Report(ctx context.Context, s Status, seq uint64) error {
	if s.SessionID == "" {
		return nil
	}

	if s.SessionID != r.claimed {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.claimAttempt = s.SessionID
		r.stated = false
		r.lifecycle = "inactive"
		r.model = s.Model
		accepted, err := r.send(ctx, seq, r.claimParams(s))
		if err != nil {
			return fmt.Errorf("claim: %w", err)
		}
		if !accepted {
			// roost always accepts a claim; a server that did not has left
			// nothing a preserve line could match, so the next Report claims.
			return nil
		}
		r.claimed = s.SessionID
	}

	// md is nil unless the model changed to a non-empty one: under preserve
	// roost only merges metadata, and has no way to delete a key, so a model
	// that went empty has nothing to say.
	var md map[string]string
	if s.Model != r.model {
		md = metadataOf("model", s.Model)
	}
	r.model = s.Model

	state := roostState{kind: s.Kind, message: s.Message, detail: s.Detail}
	if !r.stated || state != r.last {
		r.stated, r.last = true, state
		// A status with no line of its own is still recorded as stated, so it
		// is not weighed again on every Report.
		if p, ok := r.stateParams(s); ok {
			r.lifecycle = p.Lifecycle
			p.Metadata = md
			return r.sendChecked(ctx, seq, p)
		}
	}

	if md == nil {
		return nil
	}
	p := r.base(s.SessionID, "preserve")
	p.Metadata = md
	return r.sendChecked(ctx, seq, p)
}

// Release hands the tab back for the session of the last claim attempted, as
// the final write. A reporter that never attempted a claim owns nothing and
// sends nothing.
func (r *Roost) Release(ctx context.Context, seq uint64) error {
	if r.claimAttempt == "" {
		return nil
	}
	p := r.base(r.claimAttempt, "release")
	p.Lifecycle = "inactive"
	p.Attention = "clear"
	p.Detail = "session_end"
	return r.sendChecked(ctx, seq, p)
}

func (r *Roost) base(session, action string) roostParams {
	return roostParams{
		TabID:           r.tabID,
		Source:          roostSource,
		SessionID:       session,
		OwnershipAction: action,
	}
}

// claimParams takes the tab for s's session. A claim replaces roost's owner
// metadata wholesale, so it carries every key craze sets.
func (r *Roost) claimParams(s Status) roostParams {
	p := r.base(s.SessionID, "claim")
	p.Lifecycle = "inactive"
	p.Attention = "clear"
	p.Detail = "session_start"
	p.Metadata = metadataOf("model", s.Model, roostProviderKey, s.Provider, "version", version.Version)
	return p
}

// stateParams is s's state line, per plan 015 §3.4's table. ok is false for
// the ready Idle a session comes up with — the claim's inactive lifecycle
// already says it, and roost derives that to none — and for a stop or cancel
// when craze did not last tell roost a turn was running (Roost.lifecycle).
func (r *Roost) stateParams(s Status) (p roostParams, ok bool) {
	p = r.base(s.SessionID, "preserve")
	switch s.Kind {
	case Working:
		p.Lifecycle = "working"
		p.Attention = "clear"
		p.Detail = s.Detail
	case Blocked:
		p.Lifecycle = "waiting"
		p.alert("warn", cmp.Or(s.Message, blockedFallback))
		p.Detail = s.Detail
	case Failed:
		p.Lifecycle = "failed"
		p.alert("error", cmp.Or(s.Message, failedFallback))
		p.Detail = DetailError
	default:
		if s.Detail == DetailReady || (r.lifecycle != "working" && r.lifecycle != "waiting") {
			return roostParams{}, false
		}
		p.Lifecycle = "finished"
		if s.Detail == DetailCancelled {
			p.Attention = "clear"
			p.Detail = DetailCancelled
		} else {
			p.alert("info", roostTurnComplete)
			p.Detail = DetailStop
		}
	}
	return p, true
}

// alert sets attention with its severity, and the title and body roost's
// validate_report requires of every attention=set line.
func (p *roostParams) alert(severity, body string) {
	p.Attention = "set"
	p.Severity = severity
	p.Title = roostTitle
	p.Body = body
}

// sendChecked sends a line whose accepted answer the caller has no use for,
// after the Reporter contract's ctx check (hub.go): a Close that has landed
// stops the line from being written at all.
func (r *Roost) sendChecked(ctx context.Context, seq uint64, p roostParams) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := r.send(ctx, seq, p)
	return err
}

// send writes one tab.agent_report and reads its reply, skipping event frames
// and replies to any other id. accepted is roost's answer to the ownership
// check. A report it did not accept is classified here:
//
//   - the tab has no owner (roost restarted, or a release beat this report):
//     not an error; claimed is cleared so the next Report claims again.
//   - someone else owns it — `manual` from `roostctl tab set-state`, a real
//     adapter, or another craze session: an error naming the owner, so the Hub
//     warns once. claimed stays, so craze keeps sending preserve lines (which
//     roost drops) and never takes the tab back for this session.
func (r *Roost) send(ctx context.Context, seq uint64, p roostParams) (accepted bool, err error) {
	id := strconv.FormatUint(seq, 10)
	var reply roostReply
	err = roundTrip(ctx, r.socket, roostRequest{ID: id, Op: roostOp, Params: p}, func(line []byte) (bool, error) {
		var frame map[string]json.RawMessage
		if err := json.Unmarshal(line, &frame); err != nil {
			return true, fmt.Errorf("decode reply: %w", err)
		}
		if frame == nil {
			return true, errors.New("decode reply: not a JSON object")
		}
		if _, ok := frame["event"]; ok {
			return false, nil
		}
		var rep roostReply
		if err := json.Unmarshal(line, &rep); err != nil {
			return true, fmt.Errorf("decode reply: %w", err)
		}
		if rep.ID != id {
			return false, nil
		}
		reply = rep
		return true, nil
	})
	if err != nil {
		return false, err
	}

	switch {
	case reply.OK == nil:
		return false, errors.New("reply has no ok field")
	case !*reply.OK && reply.Error != nil:
		return false, fmt.Errorf("roost error %s: %s", reply.Error.Code, reply.Error.Message)
	case !*reply.OK:
		return false, errors.New("reply is ok:false with no error")
	case reply.Result == nil || reply.Result.Accepted == nil:
		return false, errors.New("reply has no result.accepted")
	case reply.Result.Tab == nil:
		// roost always returns the tab, accepted or not. A reply without it is
		// malformed either way, and must not acknowledge a claim.
		return false, errors.New("reply has no result.tab")
	case *reply.Result.Accepted:
		return true, nil
	}
	if o := reply.Result.Tab.Ownership; o != nil && o.Source != "" {
		return false, fmt.Errorf("report not accepted: tab %s is owned by source %q, session %q", r.tabID, o.Source, o.SessionID)
	}
	r.claimed = ""
	return false, nil
}

// metadataOf builds a metadata map from key, value pairs, leaving out every
// empty value; nil when nothing is left, so the key is omitted from the wire.
func metadataOf(kv ...string) map[string]string {
	var md map[string]string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		if md == nil {
			md = map[string]string{}
		}
		md[kv[i]] = kv[i+1]
	}
	return md
}
