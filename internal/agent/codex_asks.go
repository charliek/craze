package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/codexrpc"
)

// Server->client request handling for codexSession (Plan 054 § D3):
//   - item/commandExecution/requestApproval (filtering config-mutating amendments)
//   - item/fileChange/requestApproval
//   - item/tool/requestUserInput (isSecret decline, options: null, AllowText,
//     unattended first-option vs free-text needs_input)
//   - item/permissions/requestApproval (typed empty-permissions denial)
//   - mcpServer/elicitation/request (explicit decline)
//   - serverRequest/resolved (immediate retirement via acp.ErrRequestWithdrawn)

func canonicalReqKey(id json.RawMessage) string {
	return strings.TrimSpace(string(id))
}

func (s *codexSession) registerPendingReq(key string, cancel context.CancelCauseFunc) {
	s.mu.Lock()
	if s.pendingReqs == nil {
		s.pendingReqs = make(map[string]context.CancelCauseFunc)
	}
	s.pendingReqs[key] = cancel
	s.mu.Unlock()
}

func (s *codexSession) unregisterPendingReq(key string) {
	s.mu.Lock()
	delete(s.pendingReqs, key)
	s.mu.Unlock()
}

func (s *codexSession) cancelAllPendingReqsLocked(cause error) {
	for k, cancel := range s.pendingReqs {
		cancel(cause)
		delete(s.pendingReqs, k)
	}
}

func (s *codexSession) handleServerRequestResolved(raw json.RawMessage) {
	var p codexrpc.ServerRequestResolvedParams
	if json.Unmarshal(raw, &p) != nil || len(p.RequestID) == 0 {
		return
	}
	if !s.isMainThread(p.ThreadID) {
		return
	}
	key := canonicalReqKey(p.RequestID)
	s.mu.Lock()
	cancel, ok := s.pendingReqs[key]
	if ok {
		delete(s.pendingReqs, key)
	}
	s.mu.Unlock()
	if ok && cancel != nil {
		cancel(acp.ErrRequestWithdrawn)
	}
}

// matchApprovalTurn binds an incoming approval or user-input request to the
// immutable mode/policy/token of the matching active wire turn (H1).
// Returns ok=false for child threads, unknown/stale turns, or already-settled turns.
func (s *codexSession) matchApprovalTurn(threadID, turnID string) (tok TurnToken, mode string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return TurnToken{}, "", false
	}
	if s.threadID != "" && threadID != "" && threadID != s.threadID {
		return TurnToken{}, "", false
	}
	turn := s.activeTurn
	if turn != nil && !turn.settling && (turn.phase == codexPhaseSent || turn.phase == codexPhaseIdentified) {
		if turnID == "" || turn.turnID == "" || turn.turnID == turnID {
			return turn.token, turn.mode, true
		}
		return TurnToken{}, "", false
	}
	if s.foreignTurnID != "" && (turnID == "" || s.foreignTurnID == turnID) {
		return TurnToken{}, s.snap.CurrentMode, true
	}
	return TurnToken{}, "", false
}

// safeCodexCommandDecisions parses `availableDecisions []json.RawMessage`,
// defaulting to accept/acceptForSession/decline/cancel when null/empty, and
// strictly filtering out config-mutating amendment objects
// (acceptWithExecpolicyAmendment, applyNetworkPolicyAmendment) per Plan 054 § D3.
func safeCodexCommandDecisions(raw []json.RawMessage) []string {
	if len(raw) == 0 {
		return []string{"accept", "acceptForSession", "decline", "cancel"}
	}
	var out []string
	seen := make(map[string]bool)
	for _, entry := range raw {
		var str string
		if json.Unmarshal(entry, &str) != nil {
			// Skip JSON object variants (e.g. acceptWithExecpolicyAmendment,
			// applyNetworkPolicyAmendment) so Craze never offers or picks
			// persistent config-mutating amendments.
			continue
		}
		switch str {
		case "accept", "acceptForSession", "decline", "cancel":
			if !seen[str] {
				seen[str] = true
				out = append(out, str)
			}
		}
	}
	if len(out) == 0 {
		return []string{"decline", "cancel"}
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

func (s *codexSession) flushAsks() {
	_ = s.log.Flush(context.Background(), s.done)
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
	key := canonicalReqKey(*msg.ID)
	ctx, cancel := context.WithCancelCause(reqCtx)
	s.registerPendingReq(key, cancel)
	defer func() {
		s.unregisterPendingReq(key)
		cancel(nil)
	}()

	switch msg.Method {
	case codexrpc.RequestCommandApproval:
		s.handleCommandApproval(ctx, client, *msg.ID, msg.Params)
		return true
	case codexrpc.RequestFileChangeApproval:
		s.handleFileChangeApproval(ctx, client, *msg.ID, msg.Params)
		return true
	case codexrpc.RequestUserInput:
		s.handleRequestUserInput(ctx, client, *msg.ID, msg.Params)
		return true
	case codexrpc.RequestPermissionsApproval:
		_ = client.Reply(*msg.ID, codexrpc.PermissionsApprovalReply{
			Permissions: map[string]any{},
			Scope:       "turn",
		})
		return true
	case codexrpc.RequestMCPElicitation:
		_ = client.Reply(*msg.ID, map[string]any{"action": "decline"})
		return true
	default:
		return false
	}
}

func (s *codexSession) handleCommandApproval(ctx context.Context, client *codexrpc.Client, id json.RawMessage, raw json.RawMessage) {
	var p codexrpc.CommandApprovalParams
	if json.Unmarshal(raw, &p) != nil {
		_ = client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		return
	}
	tok, mode, ok := s.matchApprovalTurn(p.ThreadID, p.TurnID)
	if !ok {
		_ = client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		return
	}
	// Hard read-only invariant (Plan 054 § C3, H1): ALWAYS deny in ask/plan mode
	// based on the immutable mode captured when this turn started.
	if s.provider().ModeKind(mode) == ModeReadOnly || s.provider().ModeKind(mode) == ModePlan {
		_ = client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		return
	}

	decisions := safeCodexCommandDecisions(p.AvailableDecisions)
	toolLabel := "shell"
	if cmd := sanitizeLine(p.Command); cmd != "" {
		toolLabel = "shell: " + truncateUTF8(cmd, 120)
	} else if r := sanitizeLine(p.Reason); r != "" {
		toolLabel = "shell: " + truncateUTF8(r, 120)
	}
	s.decideCodexApproval(ctx, client, id, tok, toolLabel, decisions)
}

func (s *codexSession) handleFileChangeApproval(ctx context.Context, client *codexrpc.Client, id json.RawMessage, raw json.RawMessage) {
	var p codexrpc.FileChangeApprovalParams
	if json.Unmarshal(raw, &p) != nil {
		_ = client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		return
	}
	tok, mode, ok := s.matchApprovalTurn(p.ThreadID, p.TurnID)
	if !ok {
		_ = client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		return
	}
	// Hard read-only invariant: ALWAYS deny in ask/plan mode.
	if s.provider().ModeKind(mode) == ModeReadOnly || s.provider().ModeKind(mode) == ModePlan {
		_ = client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		return
	}

	decisions := []string{"accept", "acceptForSession", "decline", "cancel"}
	toolLabel := "apply_patch"
	if r := sanitizeLine(p.Reason); r != "" {
		toolLabel = "apply_patch: " + truncateUTF8(r, 120)
	} else if p.GrantRoot != "" {
		toolLabel = "apply_patch: " + truncateUTF8(sanitizeLine(p.GrantRoot), 120)
	}
	s.decideCodexApproval(ctx, client, id, tok, toolLabel, decisions)
}

func (s *codexSession) decideCodexApproval(ctx context.Context, client *codexrpc.Client, id json.RawMessage, tok TurnToken, toolLabel string, decisions []string) {
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
		ans := AskAnswer{Cancel: true}
		decision := "decline"
		if codexHasDecision(decisions, "accept") {
			ans = AskAnswer{OptionID: "accept"}
			decision = "accept"
		} else if codexHasDecision(decisions, "acceptForSession") {
			ans = AskAnswer{OptionID: "acceptForSession"}
			decision = "acceptForSession"
		}
		rec := s.asks.Automatic(tok, askReq, ans)
		s.flushAsks()
		err := client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: decision})
		s.asks.Report(rec.ID, AskReport{Consumed: true, Delivered: err == nil})
		return
	}

	if ctx.Err() != nil {
		rec := s.asks.AnsweredEarly(tok, askReq, AskCancelled, callEndedBy(ctx))
		s.flushAsks()
		if rec.By != AskByWithdrawn {
			_ = client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: "cancel"})
		}
		return
	}

	parked, err := s.asks.Open(ctx, tok, askReq)
	if err != nil {
		_ = client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: "decline"})
		return
	}
	s.flushAsks()
	rec := parked.Wait()
	s.flushAsks()

	// If serverRequest/resolved withdrew the request on Codex's side, do not
	// send a stale reply.
	if rec.By == AskByWithdrawn {
		return
	}
	decision := "decline"
	switch rec.Outcome {
	case AskAnswered:
		if rec.Answer.OptionID != "" && codexHasDecision(decisions, rec.Answer.OptionID) {
			decision = rec.Answer.OptionID
		}
	case AskCancelled, AskTurnEnded, AskClosing:
		if codexHasDecision(decisions, "cancel") {
			decision = "cancel"
		}
	}
	replyErr := client.Reply(id, codexrpc.ApprovalDecisionReply{Decision: decision})
	s.asks.Report(rec.ID, AskReport{Consumed: true, Delivered: replyErr == nil})
}

func (s *codexSession) handleRequestUserInput(ctx context.Context, client *codexrpc.Client, id json.RawMessage, raw json.RawMessage) {
	var p codexrpc.RequestUserInputParams
	if json.Unmarshal(raw, &p) != nil {
		_ = client.Reply(id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
		return
	}
	tok, _, ok := s.matchApprovalTurn(p.ThreadID, p.TurnID)
	if !ok {
		_ = client.Reply(id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
		return
	}
	// Plan 054 § D3: decline immediately if any question has isSecret: true so
	// secret values never reach the unmasked TUI or journal.
	for _, q := range p.Questions {
		if q.IsSecret {
			_ = client.Reply(id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
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
	// - If any question is free-text-only (options == null/empty), DO NOT
	//   fabricate bogus text — park the ask so unattended `craze new --wait`
	//   exits 4 (needs_input) cleanly (Plan 054 § D3).
	if EffectiveApproval(s.opts).Question == ApprovalFirstOption && allHaveOptions {
		rec := s.asks.Automatic(tok, askReq, AskAnswer{Answers: autoAnswers})
		s.flushAsks()
		reply := codexUserInputReplyFromRecord(rec)
		err := client.Reply(id, reply)
		s.asks.Report(rec.ID, AskReport{Consumed: true, Delivered: err == nil})
		return
	}

	if ctx.Err() != nil {
		rec := s.asks.AnsweredEarly(tok, askReq, AskCancelled, callEndedBy(ctx))
		s.flushAsks()
		if rec.By != AskByWithdrawn {
			_ = client.Reply(id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
		}
		return
	}

	parked, err := s.asks.Open(ctx, tok, askReq)
	if err != nil {
		_ = client.Reply(id, codexrpc.RequestUserInputReply{Answers: map[string]codexrpc.UserInputAnswer{}})
		return
	}
	s.flushAsks()
	rec := parked.Wait()
	s.flushAsks()

	if rec.By == AskByWithdrawn {
		return
	}
	reply := codexUserInputReplyFromRecord(rec)
	replyErr := client.Reply(id, reply)
	s.asks.Report(rec.ID, AskReport{Consumed: true, Delivered: replyErr == nil})
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
