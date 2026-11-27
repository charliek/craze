package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/codexrpc"
)

// Server->client request handling for codexSession (Plan 054 § D3, M2b):
//   - Wire-ordered request registration & serverRequest/resolved cancellation
//     (in codexrpc.Client) plus per-request single-transition reply gate (H2).
//   - Strict main-thread + live wire-turn ownership binding with bounded wait
//     for turnID identification (H1).
//   - item/commandExecution/requestApproval: nil vs explicit availableDecisions,
//     filtering persistent config-mutating amendments while preserving Decline (M5).
//   - item/fileChange/requestApproval, item/permissions/requestApproval,
//     mcpServer/elicitation/request, and item/tool/requestUserInput.

var codexApprovalTurnIDWait = 2 * time.Second

// codexPendingReq arbitrates serverRequest/resolved withdrawal against reply
// commitment for one server->client request so a withdrawn request never sends
// a stale reply and an old worker can never delete a reused request ID (H2).
type codexPendingReq struct {
	key       string
	mu        sync.Mutex
	withdrawn bool
	replied   bool
	cancel    context.CancelCauseFunc
}

// commitReply runs fn at most once, ONLY if this request has not been withdrawn
// (either via codexPendingReq or via codexrpc's wire-ordered reqCtx) and ctx is
// still live.
func (r *codexPendingReq) commitReply(ctx context.Context, fn func(context.Context) error) (sent bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replied || r.withdrawn {
		return false, nil
	}
	if ctx != nil && ctx.Err() != nil {
		if errors.Is(context.Cause(ctx), codexrpc.ErrRequestResolved) || errors.Is(context.Cause(ctx), acp.ErrRequestWithdrawn) {
			r.withdrawn = true
		}
		return false, nil
	}
	r.replied = true
	return true, fn(ctx)
}

func canonicalReqKey(id json.RawMessage) string {
	return strings.TrimSpace(string(id))
}

func (s *codexSession) registerPendingReq(key string, cancel context.CancelCauseFunc) *codexPendingReq {
	req := &codexPendingReq{key: key, cancel: cancel}
	s.mu.Lock()
	if s.pendingReqs == nil {
		s.pendingReqs = make(map[string]*codexPendingReq)
	}
	s.pendingReqs[key] = req
	s.mu.Unlock()
	return req
}

func (s *codexSession) unregisterPendingReq(req *codexPendingReq) {
	if req == nil {
		return
	}
	s.mu.Lock()
	// Delete ONLY by pointer identity so an old worker never removes a reused ID.
	if s.pendingReqs[req.key] == req {
		delete(s.pendingReqs, req.key)
	}
	s.mu.Unlock()
}

func (s *codexSession) cancelAllPendingReqsLocked(cause error) {
	for k, req := range s.pendingReqs {
		if req != nil && req.cancel != nil {
			req.cancel(cause)
		}
		delete(s.pendingReqs, k)
	}
}

func (s *codexSession) handleServerRequestResolved(_ json.RawMessage) {
	// Wire-ordered request withdrawal runs synchronously in
	// codexrpc.Client.resolveServerRequestOnWire on readLoop via the exact
	// owning request context (cancelling it with codexrpc.ErrRequestResolved,
	// which onServerRequest translates to acp.ErrRequestWithdrawn). Looking up
	// pendingReqs here on the delayed notification worker could race with a
	// reused numeric request ID and withdraw a newer in-flight request.
}

// matchApprovalTurn binds an incoming approval or user-input request to the
// exact live wire turn on the main thread and its immutable captured mode (H1):
//   - Requires non-empty main-thread ID matching s.threadID and non-empty turnID.
//   - Rejects any already-settled turnID.
//   - If a local turn is in `sent` (awaiting turn/started or turn/start reply),
//     waits boundedly for turn.idReady before checking exact turnID equality.
//   - Foreign turns (unknown execution policy) return ok=false for escalations
//     (allowForeign=false) so autonomous turns cannot inherit a mid-turn mode switch.
func (s *codexSession) matchApprovalTurn(ctx context.Context, threadID, turnID string, allowForeign bool) (turn *codexTurnState, tok TurnToken, mode string, ok bool) {
	if threadID == "" || turnID == "" {
		return nil, TurnToken{}, "", false
	}
	for {
		s.mu.Lock()
		if s.closed || s.threadID == "" || threadID != s.threadID || s.settledTurns[turnID] {
			s.mu.Unlock()
			return nil, TurnToken{}, "", false
		}
		t := s.activeTurn
		if t != nil && !t.settling {
			if t.phase == codexPhaseIdentified {
				if t.turnID == turnID {
					tok, mode = t.token, t.mode
					s.mu.Unlock()
					return t, tok, mode, true
				}
				s.mu.Unlock()
				return nil, TurnToken{}, "", false
			}
			if t.phase == codexPhaseSent {
				// Wait boundedly until turn/started or turn/start reply establishes ID.
				idReady := t.idReady
				done := t.done
				s.mu.Unlock()
				timer := time.NewTimer(codexApprovalTurnIDWait)
				select {
				case <-idReady:
					timer.Stop()
					continue
				case <-done:
					timer.Stop()
					return nil, TurnToken{}, "", false
				case <-ctx.Done():
					timer.Stop()
					return nil, TurnToken{}, "", false
				case <-s.closingCh:
					timer.Stop()
					return nil, TurnToken{}, "", false
				case <-timer.C:
					return nil, TurnToken{}, "", false
				}
			}
		}
		if allowForeign && s.foreignTurnID != "" && s.foreignTurnID == turnID {
			// Foreign user-input questions (non-escalation) may be answered.
			s.mu.Unlock()
			return nil, TurnToken{}, codexAskMode, true
		}
		s.mu.Unlock()
		return nil, TurnToken{}, "", false
	}
}

func (s *codexSession) turnCancelLatched(turn *codexTurnState) bool {
	if turn == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return turn.cancelPending || turn.settling || turn.phase == codexPhaseSettled
}

// safeCodexCommandDecisions parses `availableDecisions []json.RawMessage` (M5):
//   - Only when raw == nil (absent/null on the wire) apply the 4 legacy defaults.
//   - When raw != nil (including `[]` or Probe 1's `[accept, {amendment}, cancel]`):
//     filter affirmative choices (`accept`, `acceptForSession`) strictly against
//     the supplied list, NEVER offer config-mutating amendment objects, ALWAYS
//     include safe `"decline"` (accepted by Codex even when omitted from
//     availableDecisions, verified in Probe 1 frames-06:45), and preserve `"cancel"`
//     when offered (or if no choices were offered).
func safeCodexCommandDecisions(raw []json.RawMessage) []string {
	if raw == nil {
		return []string{"accept", "acceptForSession", "decline", "cancel"}
	}
	var out []string
	seen := make(map[string]bool)
	add := func(d string) {
		if !seen[d] {
			seen[d] = true
			out = append(out, d)
		}
	}
	for _, entry := range raw {
		var str string
		if json.Unmarshal(entry, &str) != nil {
			// Skip JSON object variants (acceptWithExecpolicyAmendment,
			// applyNetworkPolicyAmendment) so Craze never offers or picks
			// persistent config-mutating amendments.
			continue
		}
		switch str {
		case "accept", "acceptForSession", "decline", "cancel":
			add(str)
		}
	}
	// Always include safe "decline" (distinct from "cancel") so the user can
	// decline a single command without aborting the whole turn.
	if !seen["decline"] {
		if seen["cancel"] {
			// Insert decline right before cancel.
			idx := len(out) - 1
			for i, v := range out {
				if v == "cancel" {
					idx = i
					break
				}
			}
			out = append(out[:idx], append([]string{"decline"}, out[idx:]...)...)
		} else {
			add("decline")
		}
	}
	return out
}

func codexPermissionOptions(decisions []string) []PermissionOption {
	opts := make([]PermissionOption, 0, len(decisions))
	for _, d := range decisions {
		switch d {
		case "accept":
			opts = append(opts, PermissionOption{OptionID: "accept", Name: "Allow once", Kind: "allow_once"})
		case "acceptForSession":
			opts = append(opts, PermissionOption{OptionID: "acceptForSession", Name: "Allow for session", Kind: "allow_always"})
		case "decline":
			opts = append(opts, PermissionOption{OptionID: "decline", Name: "Decline", Kind: "reject_once"})
		case "cancel":
			opts = append(opts, PermissionOption{OptionID: "cancel", Name: "Cancel turn", Kind: "reject_once"})
		}
	}
	return opts
}

func codexHasDecision(decisions []string, want string) bool {
	for _, d := range decisions {
		if d == want {
			return true
		}
	}
	return false
}

func (s *codexSession) flushAsks(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	_ = s.log.Flush(ctx, s.done)
}

func (s *codexSession) onServerRequest(reqCtx context.Context, msg *codexrpc.Message) bool {
	if msg.ID == nil {
		return false
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return false
	}

	// Translate wire-ordered codexrpc.ErrRequestResolved into acp.ErrRequestWithdrawn
	// so AskRegistry records AskByWithdrawn cleanly even on back-to-back frames (H2).
	ctx, cancel := context.WithCancelCause(context.Background())
	stopBridge := context.AfterFunc(reqCtx, func() {
		cause := context.Cause(reqCtx)
		if errors.Is(cause, codexrpc.ErrRequestResolved) {
			cancel(acp.ErrRequestWithdrawn)
		} else {
			cancel(cause)
		}
	})
	defer stopBridge()

	key := canonicalReqKey(*msg.ID)
	req := s.registerPendingReq(key, cancel)
	defer func() {
		s.unregisterPendingReq(req)
		cancel(nil)
	}()

	switch msg.Method {
	case codexrpc.RequestCommandApproval:
		s.handleCommandApproval(ctx, req, client, *msg.ID, msg.Params)
		return true
	case codexrpc.RequestFileChangeApproval:
		s.handleFileChangeApproval(ctx, req, client, *msg.ID, msg.Params)
		return true
	case codexrpc.RequestUserInput:
		s.handleRequestUserInput(ctx, req, client, *msg.ID, msg.Params)
		return true
	case codexrpc.RequestPermissionsApproval:
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, *msg.ID, codexrpc.PermissionsApprovalReply{
				Permissions: map[string]any{},
				Scope:       "turn",
			})
		})
		return true
	case codexrpc.RequestMCPElicitation:
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, *msg.ID, map[string]any{"action": "decline"})
		})
		return true
	default:
		return false
	}
}

func (s *codexSession) handleCommandApproval(ctx context.Context, req *codexPendingReq, client *codexrpc.Client, id json.RawMessage, raw json.RawMessage) {
	var p codexrpc.CommandApprovalParams
	if json.Unmarshal(raw, &p) != nil {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		})
		return
	}
	turn, tok, mode, ok := s.matchApprovalTurn(ctx, p.ThreadID, p.TurnID, false)
	if !ok {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		})
		return
	}
	// Hard read-only invariant (Plan 054 § C3, H1): ALWAYS deny in ask/plan mode
	// using the immutable mode captured when this wire turn started.
	if s.provider().ModeKind(mode) == ModeReadOnly || s.provider().ModeKind(mode) == ModePlan {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		})
		return
	}

	decisions := safeCodexCommandDecisions(p.AvailableDecisions)
	toolLabel := "shell"
	if cmd := sanitizeLine(p.Command); cmd != "" {
		toolLabel = "shell: " + truncateUTF8(cmd, 120)
	} else if r := sanitizeLine(p.Reason); r != "" {
		toolLabel = "shell: " + truncateUTF8(r, 120)
	}
	s.decideCodexApproval(ctx, req, client, id, turn, tok, toolLabel, decisions)
}

func (s *codexSession) handleFileChangeApproval(ctx context.Context, req *codexPendingReq, client *codexrpc.Client, id json.RawMessage, raw json.RawMessage) {
	var p codexrpc.FileChangeApprovalParams
	if json.Unmarshal(raw, &p) != nil {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		})
		return
	}
	turn, tok, mode, ok := s.matchApprovalTurn(ctx, p.ThreadID, p.TurnID, false)
	if !ok {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		})
		return
	}
	// Hard read-only invariant: ALWAYS deny in ask/plan mode.
	if s.provider().ModeKind(mode) == ModeReadOnly || s.provider().ModeKind(mode) == ModePlan {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		})
		return
	}

	decisions := []string{"accept", "acceptForSession", "decline", "cancel"}
	toolLabel := "apply_patch"
	if r := sanitizeLine(p.Reason); r != "" {
		toolLabel = "apply_patch: " + truncateUTF8(r, 120)
	} else if p.GrantRoot != "" {
		toolLabel = "apply_patch: " + truncateUTF8(sanitizeLine(p.GrantRoot), 120)
	}
	s.decideCodexApproval(ctx, req, client, id, turn, tok, toolLabel, decisions)
}

func (s *codexSession) decideCodexApproval(ctx context.Context, req *codexPendingReq, client *codexrpc.Client, id json.RawMessage, turn *codexTurnState, tok TurnToken, toolLabel string, decisions []string) {
	askReq := AskRequest{
		Kind: AskPermission,
		Body: AskBody{
			Permission: &PermissionEvent{
				Tool:    toolLabel,
				Options: codexPermissionOptions(decisions),
			},
		},
	}

	if EffectiveApproval(s.opts).Permission == ApprovalAllow {
		if ctx.Err() != nil {
			_ = s.asks.AnsweredEarly(tok, askReq, AskCancelled, callEndedBy(ctx))
			s.flushAsks(ctx)
			return
		}
		ans := AskAnswer{Cancel: true}
		decision := "decline"
		if !s.turnCancelLatched(turn) {
			if codexHasDecision(decisions, "accept") {
				ans = AskAnswer{OptionID: "accept"}
				decision = "accept"
			} else if codexHasDecision(decisions, "acceptForSession") {
				ans = AskAnswer{OptionID: "acceptForSession"}
				decision = "acceptForSession"
			}
		} else if codexHasDecision(decisions, "cancel") {
			decision = "cancel"
		}
		rec := s.asks.Automatic(tok, askReq, ans)
		s.flushAsks(ctx)
		sent, err := req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: decision})
		})
		s.asks.Report(rec.ID, AskReport{Consumed: true, Delivered: sent && err == nil})
		return
	}

	if ctx.Err() != nil {
		rec := s.asks.AnsweredEarly(tok, askReq, AskCancelled, callEndedBy(ctx))
		s.flushAsks(ctx)
		if rec.By != AskByWithdrawn {
			_, _ = req.commitReply(ctx, func(c context.Context) error {
				return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: "cancel"})
			})
		}
		return
	}

	parked, err := s.asks.Open(ctx, tok, askReq)
	if err != nil {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		})
		return
	}
	s.flushAsks(ctx)
	rec := parked.Wait()
	s.flushAsks(ctx)

	if rec.By == AskByWithdrawn {
		return
	}
	decision := "decline"
	switch rec.Outcome {
	case AskAnswered:
		if rec.Answer.OptionID != "" && codexHasDecision(decisions, rec.Answer.OptionID) {
			decision = rec.Answer.OptionID
		}
		// Reject affirmative replies if turn cancellation has since been latched (H2).
		if (decision == "accept" || decision == "acceptForSession") && s.turnCancelLatched(turn) {
			if codexHasDecision(decisions, "cancel") {
				decision = "cancel"
			} else {
				decision = "decline"
			}
		}
	case AskCancelled, AskTurnEnded, AskClosing:
		if codexHasDecision(decisions, "cancel") {
			decision = "cancel"
		}
	}
	sent, replyErr := req.commitReply(ctx, func(c context.Context) error {
		return client.ReplyCtx(c, id, codexrpc.ApprovalDecisionReply{Decision: decision})
	})
	s.asks.Report(rec.ID, AskReport{Consumed: true, Delivered: sent && replyErr == nil})
}

func (s *codexSession) handleRequestUserInput(ctx context.Context, req *codexPendingReq, client *codexrpc.Client, id json.RawMessage, raw json.RawMessage) {
	var p codexrpc.RequestUserInputParams
	if json.Unmarshal(raw, &p) != nil {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
		})
		return
	}
	_, tok, _, ok := s.matchApprovalTurn(ctx, p.ThreadID, p.TurnID, true)
	if !ok {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
		})
		return
	}
	// Plan 054 § D3: decline immediately if any question has isSecret: true so
	// secret values never reach the unmasked TUI or journal.
	for _, q := range p.Questions {
		if q.IsSecret {
			_, _ = req.commitReply(ctx, func(c context.Context) error {
				return client.ReplyCtx(c, id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
			})
			return
		}
	}

	questions := make([]Question, 0, len(p.Questions))
	allHaveOptions := len(p.Questions) > 0
	autoAnswers := make(map[string][]string, len(p.Questions))
	title := ""
	for _, q := range p.Questions {
		if title == "" && strings.TrimSpace(q.Header) != "" {
			title = sanitizeText(strings.TrimSpace(q.Header))
		}
		opts := make([]Option, 0, len(q.Options))
		for _, o := range q.Options {
			opts = append(opts, Option{
				ID:          o.Label,
				Label:       sanitizeText(o.Label),
				Description: sanitizeText(o.Description),
			})
		}
		if len(opts) == 0 {
			allHaveOptions = false
		} else {
			autoAnswers[q.ID] = []string{opts[0].ID}
		}
		questions = append(questions, Question{
			ID:        q.ID,
			Prompt:    sanitizeText(q.Question),
			Options:   opts,
			AllowText: q.IsOther || len(q.Options) == 0,
			Required:  false,
		})
	}
	if title == "" {
		title = "Codex question"
	}

	askReq := AskRequest{
		Kind: AskQuestion,
		Body: AskBody{
			Question: &QuestionEvent{
				Title:     title,
				Questions: questions,
			},
		},
	}

	// Unattended policy (ApprovalFirstOption):
	// - If every question offers options, pick each first option automatically.
	// - If any question is free-text-only (options == null/empty):
	//   * On an unattended detached host (`craze new --wait`, Unattended: true),
	//     DO NOT fabricate bogus text — park the ask so `craze new --wait` exits 4
	//     (`needs_input`) and can be answered via `craze attach` (Plan 054 § D3).
	//   * On standalone headless (`craze prompt`, !Unattended && !Interactive),
	//     there is no TUI and no attachable host, so cancel immediately rather
	//     than hanging until interrupted.
	if EffectiveApproval(s.opts).Question == ApprovalFirstOption {
		if allHaveOptions {
			if ctx.Err() != nil {
				_ = s.asks.AnsweredEarly(tok, askReq, AskCancelled, callEndedBy(ctx))
				s.flushAsks(ctx)
				return
			}
			rec := s.asks.Automatic(tok, askReq, AskAnswer{Answers: autoAnswers})
			s.flushAsks(ctx)
			reply := codexUserInputReplyFromRecord(rec)
			sent, err := req.commitReply(ctx, func(c context.Context) error {
				return client.ReplyCtx(c, id, reply)
			})
			s.asks.Report(rec.ID, AskReport{Consumed: true, Delivered: sent && err == nil})
			return
		}
		if !s.opts.Unattended && !s.opts.Interactive {
			rec := s.asks.AnsweredEarly(tok, askReq, AskCancelled, AskByPolicy)
			s.flushAsks(ctx)
			if rec.By != AskByWithdrawn {
				_, _ = req.commitReply(ctx, func(c context.Context) error {
					return client.ReplyCtx(c, id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
				})
			}
			return
		}
	}

	if ctx.Err() != nil {
		rec := s.asks.AnsweredEarly(tok, askReq, AskCancelled, callEndedBy(ctx))
		s.flushAsks(ctx)
		if rec.By != AskByWithdrawn {
			_, _ = req.commitReply(ctx, func(c context.Context) error {
				return client.ReplyCtx(c, id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
			})
		}
		return
	}

	parked, err := s.asks.Open(ctx, tok, askReq)
	if err != nil {
		_, _ = req.commitReply(ctx, func(c context.Context) error {
			return client.ReplyCtx(c, id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
		})
		return
	}
	s.flushAsks(ctx)
	rec := parked.Wait()
	s.flushAsks(ctx)

	if rec.By == AskByWithdrawn {
		return
	}
	reply := codexUserInputReplyFromRecord(rec)
	sent, replyErr := req.commitReply(ctx, func(c context.Context) error {
		return client.ReplyCtx(c, id, reply)
	})
	s.asks.Report(rec.ID, AskReport{Consumed: true, Delivered: sent && replyErr == nil})
}

func codexUserInputReplyFromRecord(rec AskRecord) codexrpc.RequestUserInputReply {
	out := codexrpc.RequestUserInputReply{
		Answers: make(map[string]codexrpc.UserInputAnswer),
	}
	if rec.Outcome != AskAnswered && rec.Outcome != AskAutomatic {
		return out
	}
	if rec.Answer.Skip || rec.Answer.Cancel {
		return out
	}
	for qid, picks := range rec.Answer.Answers {
		var vals []string
		for _, p := range picks {
			if p != "" {
				vals = append(vals, p)
			}
		}
		if len(vals) > 0 {
			out.Answers[qid] = codexrpc.UserInputAnswer{Answers: vals}
		}
	}
	for qid, txt := range rec.Answer.Text {
		if strings.TrimSpace(txt) == "" {
			continue
		}
		cur := out.Answers[qid]
		cur.Answers = append(cur.Answers, txt)
		out.Answers[qid] = cur
	}
	return out
}
