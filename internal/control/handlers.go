package control

import (
	"context"
	"errors"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/transcript"
)

// handle is one admitted request's handler goroutine (§3.7 role 2): it runs
// the method against the engine the connection bound at hello and queues the
// reply, which takes over the request's admission slot. A reply whose
// connection has gone is dropped.
func (c *conn) handle(b *bound, info protocol.MethodInfo, req *request) {
	res, perr, ok := c.run(b, info, req)
	switch {
	case !ok:
		c.drop()
	case perr != nil:
		c.replyErr(req.id, perr)
	default:
		c.reply(req.id, res)
	}
}

// outcome is a handler's answer: a result, or an error, and whether a reply
// is still owed at all (false: the connection closed while it waited).
type outcome struct {
	res  any
	perr *protocol.Error
	ok   bool
}

func answer(res any) outcome               { return outcome{res: res, ok: true} }
func refusal(perr *protocol.Error) outcome { return outcome{perr: perr, ok: true} }

// run dispatches one method.
func (c *conn) run(b *bound, info protocol.MethodInfo, req *request) (any, *protocol.Error, bool) {
	var o outcome
	switch req.method {
	case protocol.MethodSessionsList:
		o = c.sessionsList(b, info, req)
	case protocol.MethodSessionState:
		o = c.sessionState(b, info, req)
	case protocol.MethodSessionSnapshot:
		o = c.sessionSnapshot(b, info, req)
	case protocol.MethodSessionSync:
		o = c.sessionSync(b, info, req)
	case protocol.MethodAsksList:
		o = c.asksList(b, info, req)
	case protocol.MethodAsksGet:
		o = c.asksGet(b, info, req)
	case protocol.MethodSessionAttach, protocol.MethodSessionDetach:
		// Attach, detach and the forwarder are C7's (plan 027 §5).
		o = refusal(refused(protocol.CodeUnsupported, protocol.ReasonUnsupported,
			"%s is not served by this build yet", req.method))
	case protocol.MethodSessionPrompt:
		o = c.sessionPrompt(b, info, req)
	case protocol.MethodSessionCancel:
		o = c.sessionCancel(b, info, req)
	case protocol.MethodSessionDisarm:
		o = c.sessionDisarm(b, info, req)
	case protocol.MethodQueueAdd:
		o = c.queueAdd(b, info, req)
	case protocol.MethodQueueEdit:
		o = c.queueEdit(b, info, req)
	case protocol.MethodQueueRemove:
		o = c.queueRemove(b, info, req)
	case protocol.MethodQueueClear:
		o = c.queueClear(b, info, req)
	case protocol.MethodSessionSet:
		o = c.sessionSet(b, info, req)
	case protocol.MethodSessionSetTitle:
		o = c.sessionSetTitle(b, info, req)
	case protocol.MethodSubagentCancel:
		o = c.subagentCancel(b, info, req)
	case protocol.MethodAsksAnswer:
		o = c.asksAnswer(b, info, req)
	default:
		// Every method protocol.Method knows is answered above, or in
		// dispatch (hello, and the ones a host does not serve).
		o = refusal(&protocol.Error{Code: protocol.RPCMethodNotFound, Message: "unknown method " + req.method,
			Data: protocol.ErrorData{Code: protocol.CodeUnsupported, Reason: protocol.ReasonUnknownMethod}})
	}
	return o.res, o.perr, o.ok
}

// params decodes req's params into p strictly and checks what every method of
// its kind carries: a canonical commandId (a mutating method; SF-12, the
// wire's own validation — the engine's parseCommand stays the second line),
// check's method-specific rules, and then the sessionId (a session-scoped
// method): every -32602 is decided before unknown_session.
func (c *conn) params(b *bound, info protocol.MethodInfo, req *request, p any, check func() *protocol.Error) *protocol.Error {
	if perr := decodeParams(req.params, p); perr != nil {
		return perr
	}
	sessionID, commandID := idsOf(p)
	if info.Mutating && !canonicalCommandID(commandID) {
		return badParams("params.commandId %q is not a canonical positive decimal", commandID)
	}
	if check != nil {
		if perr := check(); perr != nil {
			return perr
		}
	}
	if info.SessionScoped && sessionID != b.crazeID {
		return refused(protocol.CodeUnknownSession, protocol.ReasonUnknownSession,
			"this host serves no session %q", sessionID)
	}
	return nil
}

// command runs one mutating command (§3.6): past the host-wide cap it is
// refused unavailable, reason busy — not run, never stored; otherwise run makes
// the engine call, on a server-owned context where it takes one, and the reply
// barrier follows it, success or error. A barrier that fails because the log
// is closing never turns the command's answer into another: the reply carries
// the command's own result (§3.7).
//
// Just before the engine call, the binding it was admitted under must not have
// moved on (Server.movedOn): if a resume has since transferred the client to
// another connection (a newer generation), the binding has since been dropped
// (X15: the drop erases a transfer's evidence), or the engine was replaced,
// the command does not run under the client id this connection held (astra r5
// 3) — no reply is owed (the connection is closing), the receipts table never
// sees its id, and the client's resend on its resumed connection runs it once
// (or, its binding dropped, its resume is resumed: false: outcome unknown).
// A plain release is not moving on: a connection that merely closed, its
// binding still in the table, still runs the command it admitted — losing the
// connection does not cancel an admitted command (§3.6) — and its receipt
// answers the resend. A command that reached the engine keeps running,
// detached, whatever happens after.
func (c *conn) command(b *bound, method string, run func() outcome) outcome {
	if h := c.srv.hooks.beforeCommand; h != nil {
		h(method)
	}
	if !c.srv.admitCommand() {
		return refusal(refused(protocol.CodeUnavailable, protocol.ReasonBusy,
			"the host has %d commands in flight; resend the same commandId", protocol.CommandsPerHost))
	}
	o, ran := func() (outcome, bool) {
		defer c.srv.commandDone()
		if c.srv.movedOn(b) {
			return outcome{}, false
		}
		return run(), true
	}()
	if !ran {
		return outcome{}
	}
	if h := c.srv.hooks.beforeBarrier; h != nil {
		h(method)
	}
	if _, _, ok := c.barrier(b); !ok {
		return outcome{}
	}
	return o
}

// commandCtx is a blocking command's server-owned context (§3.6): bounded,
// and never the connection's, so losing the connection cancels nothing.
func commandCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), commandTimeout)
}

// ------------------------------------------------------------------ reads

func (c *conn) sessionsList(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.SessionsListParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	// The rows are read first and the cursor after them, so the cursor covers
	// every event describing what the rows show: the rows are consistent with
	// it or behind it (§3.3, SD-28).
	si, st := c.srv.sessionInfo(b.eng)
	seq, err := b.eng.SyncSeq(c.ctx)
	switch {
	case errors.Is(err, agent.ErrLogClosing):
		return refusal(refused(protocol.CodeNotAccepting, protocol.ReasonNotAccepting,
			"the session is closing: there is no seq to vouch for the rows"))
	case err != nil:
		return outcome{}
	}
	row := protocol.SessionRow{
		SessionInfo: si,
		Title:       st.Title,
		Activity:    protocol.Activity(st.Activity),
		ForeignTurn: st.ForeignTurn,
		PendingAsks: st.PendingAsks,
		HeadAsk:     headAsk(st),
	}
	return answer(protocol.SessionsListResult{Epoch: c.srv.hostID, Cursor: seq, Sessions: []protocol.SessionRow{row}})
}

func (c *conn) sessionState(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.StateParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	r, err := stateResult(b.eng.State())
	if err != nil {
		return refusal(failed(err))
	}
	return answer(r)
}

func (c *conn) sessionSnapshot(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.SnapshotParams
	check := func() *protocol.Error {
		if p.Budget != nil && p.Budget.SnapshotBytes < 0 {
			return badParams("params.budget.snapshotBytes must be positive")
		}
		return nil
	}
	if perr := c.params(b, info, req, &p, check); perr != nil {
		return refusal(perr)
	}
	budget := 0
	if p.Budget != nil {
		budget = min(p.Budget.SnapshotBytes, protocol.SnapshotBytesMax)
	}
	snap, err := b.eng.TranscriptSnapshot(p.AgentID, budget)
	switch {
	case errors.Is(err, transcript.ErrSnapshotTooLarge):
		return refusal(refused(protocol.CodeFailed, protocol.ReasonSnapshotTooLarge, "%v", err))
	case err != nil:
		return refusal(engineError(err))
	}
	raw, err := transcript.EncodeSnapshot(snap)
	if err != nil {
		return refusal(failed(err))
	}
	return answer(protocol.SnapshotResult{Snapshot: raw})
}

func (c *conn) sessionSync(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.SyncParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	seq, closing, ok := c.barrier(b)
	switch {
	case !ok:
		return outcome{}
	case closing:
		return refusal(refused(protocol.CodeNotAccepting, protocol.ReasonNotAccepting,
			"the session is closing: there is no seq to wait for"))
	}
	return answer(protocol.SyncResult{Seq: seq})
}

func (c *conn) asksList(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.AsksListParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	asks := b.eng.Asks()
	out := make([]protocol.AskSummary, 0, len(asks))
	for _, r := range asks {
		out = append(out, askSummary(r))
	}
	return answer(protocol.AsksListResult{Asks: out})
}

func (c *conn) asksGet(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.AsksGetParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	r, ok := b.eng.Ask(p.AskID)
	if !ok {
		return refusal(refused(protocol.CodeUnknownAsk, protocol.ReasonUnknownAsk, "no ask %q in this session", p.AskID))
	}
	rec, err := askRecord(r)
	if err != nil {
		return refusal(failed(err))
	}
	return answer(protocol.AsksGetResult{Ask: rec})
}

// --------------------------------------------------------------- commands

func (c *conn) sessionPrompt(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.PromptParams
	check := func() *protocol.Error {
		switch p.Mode {
		case protocol.PromptQueue, protocol.PromptSendNow:
		case protocol.PromptInterject:
			if p.FromRow != "" {
				// Interject takes text alone: the engine's Interject has no
				// row (plan 027 X6).
				return badParams("params.fromRow cannot go with mode interject: an interjection takes text")
			}
		default:
			return badParams("params.mode %q is none of queue, send_now, interject", p.Mode)
		}
		return nil
	}
	if perr := c.params(b, info, req, &p, check); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		if p.Mode == protocol.PromptInterject {
			ctx, cancel := commandCtx()
			defer cancel()
			if err := b.eng.Interject(ctx, cmd, p.Text); err != nil {
				return refusal(engineError(err))
			}
			return answer(protocol.PromptResult{})
		}
		res, err := b.eng.Submit(cmd, p.Text, engine.SubmitMode(p.Mode), p.FromRow)
		if err != nil {
			return refusal(engineError(err))
		}
		return promptResult(res)
	})
}

// promptResult is a Submit's answer on the wire: the started turn with the
// text it started with (SubmitResult.Text), the row, or armed.
func promptResult(res engine.SubmitResult) outcome {
	switch {
	case res.Turn != "":
		text := res.Text
		return answer(protocol.PromptResult{Turn: res.Turn, Text: &text})
	case res.Queued != nil:
		row, err := agent.EncodeQueuedPrompt(*res.Queued)
		if err != nil {
			return refusal(failed(err))
		}
		return answer(protocol.PromptResult{Queued: row})
	case res.Armed:
		return answer(protocol.PromptResult{Armed: true})
	}
	return answer(protocol.PromptResult{})
}

func (c *conn) sessionCancel(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.CancelParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		ctx, cancel := commandCtx()
		defer cancel()
		res, err := b.eng.Cancel(ctx, cmd, p.TurnID)
		wire := protocol.CancelResult{Outcome: protocol.CancelOutcome(res.Outcome), Turn: res.Turn, Reported: res.Reported}
		if err != nil {
			e := engineError(err)
			// A cancel that ran and failed carries its result beside the error
			// (§3.2: reported, above all); a refused one has none.
			if res.Outcome != "" {
				raw, rerr := rawJSON(wire)
				if rerr != nil {
					return refusal(failed(rerr))
				}
				e.Data.Result = raw
			}
			return refusal(e)
		}
		return answer(wire)
	})
}

func (c *conn) sessionDisarm(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.DisarmParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		if err := b.eng.Disarm(cmd); err != nil {
			return refusal(engineError(err))
		}
		return answer(protocol.Empty{})
	})
}

func (c *conn) queueAdd(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.QueueAddParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		row, err := b.eng.Queue(cmd, p.Text)
		if err != nil {
			return refusal(engineError(err))
		}
		raw, err := agent.EncodeQueuedPrompt(row)
		if err != nil {
			return refusal(failed(err))
		}
		return answer(protocol.QueueAddResult{Row: raw})
	})
}

func (c *conn) queueEdit(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.QueueEditParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		if err := b.eng.EditQueued(cmd, p.RowID, p.Text, p.ExpectedVersion); err != nil {
			return refusal(engineError(err))
		}
		return answer(protocol.Empty{})
	})
}

func (c *conn) queueRemove(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.QueueRemoveParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		row, err := b.eng.Unqueue(cmd, p.RowID)
		if err != nil {
			return refusal(engineError(err))
		}
		raw, err := agent.EncodeQueuedPrompt(row)
		if err != nil {
			return refusal(failed(err))
		}
		return answer(protocol.QueueRemoveResult{Row: raw})
	})
}

func (c *conn) queueClear(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.QueueClearParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		rows, err := b.eng.ClearQueue(cmd)
		if err != nil {
			return refusal(engineError(err))
		}
		removed, err := queueRows(rows)
		if err != nil {
			return refusal(failed(err))
		}
		return answer(protocol.QueueClearResult{Removed: removed})
	})
}

func (c *conn) sessionSet(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.SetParams
	check := func() *protocol.Error {
		switch p.Setting.Kind {
		case protocol.SettingModel, protocol.SettingMode, protocol.SettingConfig:
			return nil
		}
		return badParams("params.setting.kind %q is none of model, mode, config", p.Setting.Kind)
	}
	if perr := c.params(b, info, req, &p, check); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	s := engine.Setting{Kind: engine.SettingKind(p.Setting.Kind), ID: p.Setting.ID, Value: p.Setting.Value, ForModel: p.Setting.ForModel}
	return c.command(b, req.method, func() outcome {
		ctx, cancel := commandCtx()
		defer cancel()
		res, err := b.eng.Set(ctx, cmd, s)
		if err != nil {
			return refusal(engineError(err))
		}
		return answer(protocol.SetResult{Value: res.Value, Rev: res.Rev})
	})
}

func (c *conn) sessionSetTitle(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.SetTitleParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		if err := b.eng.SetTitle(cmd, p.Title); err != nil {
			return refusal(engineError(err))
		}
		return answer(protocol.Empty{})
	})
}

func (c *conn) subagentCancel(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.SubagentCancelParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	return c.command(b, req.method, func() outcome {
		if err := b.eng.CancelSubagent(cmd, p.AgentID); err != nil {
			return refusal(engineError(err))
		}
		return answer(protocol.Empty{})
	})
}

func (c *conn) asksAnswer(b *bound, info protocol.MethodInfo, req *request) outcome {
	var p protocol.AsksAnswerParams
	if perr := c.params(b, info, req, &p, nil); perr != nil {
		return refusal(perr)
	}
	cmd := engine.Command{Client: b.client, ID: p.CommandID}
	a := agent.AskAnswer{OptionID: p.Answer.OptionID, Cancel: p.Answer.Cancel, Answers: p.Answer.Answers,
		Skip: p.Answer.Skip, Accept: p.Answer.Accept, Reject: p.Answer.Reject}
	return c.command(b, req.method, func() outcome {
		if err := b.eng.Answer(cmd, p.AskID, a); err != nil {
			return refusal(engineError(err))
		}
		return answer(protocol.Empty{})
	})
}
