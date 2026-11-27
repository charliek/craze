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
	turns     []codexrpc.Turn // chronological (oldest -> newest)
	truncated bool
	reason    string
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
				// Fallback only when thread/turns/list is unsupported and
				// thread/resume already hydrated turns.
				return codexResumeHistory{turns: res.Thread.Turns}
			}
			// If we hit our deadline or a transient listing error after loading
			// some pages (or none), show a visible notice rather than silently
			// pretending the thread was empty.
			if len(descTurns) > 0 || listCtx.Err() != nil {
				truncated = true
				reason = sanitizeLine(err.Error())
			}
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
		turns:     descTurns,
		truncated: truncated,
		reason:    reason,
	}
}

// resumeThread opens an existing Codex thread via `thread/resume`, paginates
// its history via `thread/turns/list`, and replays items chronologically under
// EventReplay brackets (Plan 054 § D6).
func (s *codexSession) resumeThread(
	ctx context.Context,
	client *codexrpc.Client,
	cwd, binary, startMode, resolvedModel string,
	sbMode codexrpc.SandboxMode,
	apPolicy codexrpc.ApprovalPolicy,
) error {
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

	// Begin ReplayStart bracket. If Close interrupts anywhere before ReplayEnd,
	// return immediately WITHOUT emitting ReplayEnd (Plan 054 § D6).
	if !s.emitCtx(ctx, Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayStart}}) {
		return fmt.Errorf("agent: session closed")
	}
	s.replaying.Store(true)
	defer s.replaying.Store(false)

	if history.truncated {
		desc := "Showing the most recent turns from this Codex thread"
		if history.reason != "" {
			desc += " (" + history.reason + ")"
		}
		if !s.emitCtx(ctx, Event{
			Type: EventNotice,
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
		// Reset per-turn delta/message tracking between replayed turns so
		// multi-turn agentMessages replay cleanly.
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
			s.processItem(rawItem, true, true)
		}
	}

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
	s.ready = true
	delta := s.installDeltaLocked()
	s.enqueueDeltaLocked("", Event{Replayed: true}, delta)
	s.mu.Unlock()

	if err := s.log.Flush(ctx, s.done); err != nil {
		return fmt.Errorf("agent: session closed")
	}
	s.replaying.Store(false)
	s.log.noteSession(journal.SessionNote{
		ProviderSessionID: tid,
		LoadedFrom:        s.opts.LoadSessionID,
		AgentBinary:       binary,
	})
	if !s.emitCtx(ctx, Event{Type: EventReplay, Replay: &ReplayInfo{Phase: ReplayEnd}}) {
		return fmt.Errorf("agent: session closed")
	}
	return nil
}
