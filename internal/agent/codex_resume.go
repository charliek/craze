package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charliek/craze/internal/codexrpc"
	"github.com/charliek/craze/internal/journal"
)

// Bounded history resume & chronological replay for codexSession (Plan 054 § D6).
var (
	codexResumeMaxTurns = 200
	codexResumeMaxItems = 2000
	codexResumeMaxBytes = 16 << 20 // 16 MiB raw item payload cap
	codexResumeTimeout  = 10 * time.Second
	codexResumePageSize = 50
)

type codexResumeHistory struct {
	turns       []codexrpc.Turn // chronological (oldest -> newest)
	truncated   bool
	unavailable bool
	reason      string
}

// boundChronologicalTurns applies the shared turn/item/byte caps to an already
// chronological slice of turns (e.g. from the -32601 hydrated fallback),
// retaining the newest suffix so fallback history never bypasses replay caps (M8).
func boundChronologicalTurns(turns []codexrpc.Turn) codexResumeHistory {
	desc := make([]codexrpc.Turn, len(turns))
	copy(desc, turns)
	slices.Reverse(desc)
	var (
		kept       []codexrpc.Turn
		totalItems int
		totalBytes int
		truncated  bool
		reason     string
	)
	for _, turn := range desc {
		if len(kept) >= codexResumeMaxTurns {
			truncated = true
			reason = fmt.Sprintf("reached %d-turn replay cap", codexResumeMaxTurns)
			break
		}
		turnBytes := 0
		for _, raw := range turn.Items {
			turnBytes += len(raw)
		}
		if totalItems+len(turn.Items) > codexResumeMaxItems {
			truncated = true
			reason = fmt.Sprintf("reached %d-item replay cap", codexResumeMaxItems)
			break
		}
		if totalBytes+turnBytes > codexResumeMaxBytes {
			truncated = true
			reason = "reached 16 MiB replay cap"
			break
		}
		kept = append(kept, turn)
		totalItems += len(turn.Items)
		totalBytes += turnBytes
	}
	slices.Reverse(kept)
	return codexResumeHistory{turns: kept, truncated: truncated, reason: reason}
}

// fetchResumeTurns paginates `thread/turns/list` in descending order with
// `itemsView: "full"`, bounded by turn count, item count, raw bytes, and time,
// then reverses into chronological order (oldest turn first).
func (s *codexSession) fetchResumeTurns(
	ctx context.Context,
	client *codexrpc.Client,
	threadID string,
	res *codexrpc.ThreadResumeResult,
) codexResumeHistory {
	listCtx, cancel := context.WithTimeout(ctx, codexResumeTimeout)
	defer cancel()

	var (
		descTurns   []codexrpc.Turn
		totalItems  int
		totalBytes  int
		truncated   bool
		unavailable bool
		reason      string
		cursor      string
		seenCursors = make(map[string]bool)
	)

	for {
		if listCtx.Err() != nil {
			truncated = true
			reason = "time limit reached"
			break
		}
		page, err := client.ThreadTurnsList(listCtx, codexrpc.ThreadTurnsListParams{
			ThreadID:      threadID,
			SortDirection: "desc",
			ItemsView:     "full",
			Limit:         codexResumePageSize,
			Cursor:        cursor,
		})
		if err != nil {
			var rpc *codexrpc.RPCError
			if len(descTurns) == 0 && errors.As(err, &rpc) && rpc.Code == codexrpc.CodeMethodNotFound && res != nil && len(res.Thread.Turns) > 0 {
				// Compatibility fallback runs through the exact same caps,
				// keeping the newest suffix (M8).
				return boundChronologicalTurns(res.Thread.Turns)
			}
			// Surface first-page failures as an explicit history-unavailable
			// notice instead of silently showing an empty transcript (M8).
			if len(descTurns) == 0 {
				unavailable = true
			} else {
				truncated = true
			}
			reason = sanitizeLine(err.Error())
			break
		}
		if page == nil || len(page.Data) == 0 {
			break
		}
		hitCap := false
		for _, turn := range page.Data {
			if len(descTurns) >= codexResumeMaxTurns {
				truncated = true
				reason = fmt.Sprintf("reached %d-turn replay cap", codexResumeMaxTurns)
				hitCap = true
				break
			}
			turnBytes := 0
			for _, raw := range turn.Items {
				turnBytes += len(raw)
			}
			if totalItems+len(turn.Items) > codexResumeMaxItems {
				truncated = true
				reason = fmt.Sprintf("reached %d-item replay cap", codexResumeMaxItems)
				hitCap = true
				break
			}
			if totalBytes+turnBytes > codexResumeMaxBytes {
				truncated = true
				reason = "reached 16 MiB replay cap"
				hitCap = true
				break
			}
			descTurns = append(descTurns, turn)
			totalItems += len(turn.Items)
			totalBytes += turnBytes
		}
		if hitCap {
			break
		}
		if page.NextCursor == nil || *page.NextCursor == "" {
			break
		}
		next := *page.NextCursor
		if seenCursors[next] {
			truncated = true
			reason = "pagination cursor repeated"
			break
		}
		seenCursors[next] = true
		cursor = next
	}

	// Reverse descending pages -> chronological order (oldest turn first).
	slices.Reverse(descTurns)
	return codexResumeHistory{
		turns:       descTurns,
		truncated:   truncated,
		unavailable: unavailable,
		reason:      reason,
	}
}

// resumeThread opens an existing Codex thread via `thread/resume`, paginates
// its history via `thread/turns/list`, and replays items chronologically under
// EventReplay brackets (Plan 054 § D6, H4, M8, M9).
func (s *codexSession) resumeThread(
	ctx context.Context,
	client *codexrpc.Client,
	cwd, binary, startMode, resolvedModel string,
	sbMode codexrpc.SandboxMode,
	apPolicy codexrpc.ApprovalPolicy,
) error {
	// Pre-install the target threadID BEFORE thread/resume so notifications
	// emitted immediately by thread/resume (such as historical
	// thread/tokenUsage/updated) match isMainThread without race (M7, M9).
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("agent: session closed")
	}
	s.threadID = s.opts.LoadSessionID
	s.mu.Unlock()

	res, err := client.ThreadResume(ctx, codexrpc.ThreadResumeParams{
		ThreadID:       s.opts.LoadSessionID,
		ExcludeTurns:   true,
		Cwd:            cwd,
		Sandbox:        sbMode,
		ApprovalPolicy: apPolicy,
		Model:          resolvedModel,
	})
	if err != nil {
		return s.classifyRPCStartError(err)
	}
	tid := s.opts.LoadSessionID
	if res != nil && res.Thread.ID != "" {
		tid = res.Thread.ID
	}
	var savedWS *codexrpc.SandboxPolicy
	if res != nil && res.Sandbox != nil && res.Sandbox.Type == codexrpc.SandboxPolicyWorkspaceWrite {
		cp := *res.Sandbox
		cp.WritableRoots = slices.Clone(res.Sandbox.WritableRoots)
		savedWS = &cp
	}
	activeModel := resolvedModel
	if activeModel == "" && res != nil && res.Model != "" {
		activeModel = res.Model
	}
	if activeModel == "" {
		activeModel = s.defaultCatalogModel()
	}
	activeEffort := strings.TrimSpace(s.opts.LoadEffort)
	if activeEffort == "" && res != nil && res.ReasoningEffort != nil {
		activeEffort = strings.TrimSpace(*res.ReasoningEffort)
	}
	if activeEffort == "" {
		activeEffort = s.defaultEffortForModel(activeModel)
	}
	activeEffort, err = s.applyStartSettings(activeModel, activeEffort)
	if err != nil {
		return err
	}

	// Install threadID before fetching turns so thread-scoped notifications
	// (such as historical thread/tokenUsage/updated) match isMainThread.
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("agent: session closed")
	}
	s.threadID = tid
	s.workspaceSandbox = savedWS
	s.mu.Unlock()

	history := s.fetchResumeTurns(ctx, client, tid, res)

	// Wait for already-queued notifications (notably Probe 2's resumed
	// thread/tokenUsage/updated that precedes thread/turns/list's reply on the
	// wire) to be processed BEFORE installing snapshot usage (M9).
	if err := client.FlushNotifications(ctx); err != nil {
		return fmt.Errorf("agent: session closed")
	}

	// Seed title before ReplayStart bracket (honoring explicit Options.Title
	// first, then Codex's persisted thread name).
	s.mu.Lock()
	seeded := false
	title := sanitizeText(s.opts.Title)
	if title == "" && res != nil && res.Thread.Name != nil {
		title = sanitizeText(strings.TrimSpace(*res.Thread.Name))
	}
	if title != "" {
		s.snap.Title = title
		t := title
		s.enqueueDeltaLocked("", Event{}, &StateDelta{Title: &t})
		seeded = true
	}
	if s.opts.TitlePinned {
		s.titlePinned = true
	}
	s.mu.Unlock()
	if seeded {
		if err := s.log.Flush(ctx, s.done); err != nil {
			return fmt.Errorf("agent: session closed")
		}
	}

	// Begin ReplayStart bracket. Every historical event carries Replayed: true
	// explicitly (never relying on a shared mutable flag that could stamp
	// concurrent live notifications — M9), and uses replay ctx so Close can
	// unwind startup immediately even if the primary channel is blocked (H4).
	if !s.emitCtx(ctx, Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayStart}}) {
		return fmt.Errorf("agent: session closed")
	}

	if history.unavailable {
		desc := "Could not load prior turns from this Codex thread"
		if history.reason != "" {
			desc += " (" + history.reason + ")"
		}
		if !s.emitCtx(ctx, Event{
			Type:     EventNotice,
			Replayed: true,
			Notice: &NoticeInfo{
				Severity:    "warning",
				Title:       "History unavailable",
				Description: desc,
			},
		}) {
			return fmt.Errorf("agent: session closed")
		}
	} else if history.truncated {
		desc := "Showing the most recent turns from this Codex thread"
		if history.reason != "" {
			desc += " (" + history.reason + ")"
		}
		if !s.emitCtx(ctx, Event{
			Type:     EventNotice,
			Replayed: true,
			Notice: &NoticeInfo{
				Severity:    "info",
				Title:       "History truncated",
				Description: desc,
			},
		}) {
			return fmt.Errorf("agent: session closed")
		}
	}

	for _, turn := range history.turns {
		s.mu.Lock()
		closed := s.closed
		s.turnItemDeltas = make(map[string]bool)
		s.lastMsgItemID = ""
		if turn.ID != "" {
			s.recordSettledTurnLocked(turn.ID)
		}
		s.mu.Unlock()
		if closed || ctx.Err() != nil {
			return fmt.Errorf("agent: session closed")
		}
		for _, rawItem := range turn.Items {
			s.mu.Lock()
			closed = s.closed
			s.mu.Unlock()
			if closed || ctx.Err() != nil {
				return fmt.Errorf("agent: session closed")
			}
			if !s.processItemCtx(ctx, rawItem, true, true) {
				return fmt.Errorf("agent: session closed")
			}
		}
	}

	// Build and flush the install delta BEFORE opening admission (s.ready stays
	// false until ReplayEnd has been emitted — M9).
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("agent: session closed")
	}
	s.snap.SessionID = tid
	s.snap.CurrentMode = startMode
	s.snap.CurrentModel = activeModel
	s.currentEffort = activeEffort
	s.rebuildCatalogAndConfigLocked()
	s.rebuildSlashLocked()
	if s.usage.hasAny() {
		u := s.usage.state()
		u.Model = activeModel
		s.snap.Usage = &u
	}
	delta := s.installDeltaLocked()
	s.enqueueDeltaLocked("", Event{Replayed: true}, delta)
	s.mu.Unlock()

	if err := s.log.Flush(ctx, s.done); err != nil {
		return fmt.Errorf("agent: session closed")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed || ctx.Err() != nil {
		return fmt.Errorf("agent: session closed")
	}
	s.log.noteSession(journal.SessionNote{
		ProviderSessionID: tid,
		LoadedFrom:        s.opts.LoadSessionID,
		AgentBinary:       binary,
	})
	if !s.emitCtx(ctx, Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayEnd}}) {
		return fmt.Errorf("agent: session closed")
	}

	// Open admission ONLY AFTER ReplayEnd has committed, and consume any
	// skills/changed that arrived during startup (M9, L10).
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("agent: session closed")
	}
	s.ready = true
	dirtySkills := s.skillsDirty
	s.skillsDirty = false
	s.mu.Unlock()
	if dirtySkills {
		s.triggerSkillRefresh()
	}
	return nil
}
