package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/codexrpc"
)

func waitForOpenAsk(t *testing.T, cs *codexSession, kind AskKind) AskRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, a := range cs.Asks().Asks() {
			if a.Kind == kind {
				return a
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for open %s ask (open=%+v)", kind, cs.Asks().Asks())
	return AskRecord{}
}

func TestCodexCommandApprovalInteractiveFiltersAmendmentsAndForceAllows(t *testing.T) {
	t.Run("interactive --no-force filters amendments and honors answer", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-approval", Options{Force: false, Interactive: true})
		promptDone := make(chan error, 1)
		var text strings.Builder
		evs := collectSubEvents(t, cs, func() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, err := cs.Prompt(ctx, "run command")
				promptDone <- err
			}()
			open := waitForOpenAsk(t, cs, AskPermission)
			// Verify persistent config-mutating amendments were filtered out!
			var optIDs []string
			for _, o := range open.Body.Permission.Options {
				optIDs = append(optIDs, o.OptionID)
				if strings.Contains(o.OptionID, "Amendment") || strings.Contains(o.OptionID, "amendment") {
					t.Fatalf("config-mutating amendment leaked into PermissionOption: %+v", o)
				}
			}
			if !slices.Contains(optIDs, "accept") || !slices.Contains(optIDs, "decline") || !slices.Contains(optIDs, "cancel") {
				t.Fatalf("unexpected permission options: %v", optIDs)
			}
			if _, err := cs.Asks().Answer("client/1", open.ID, AskAnswer{OptionID: "accept"}); err != nil {
				t.Fatalf("Answer(accept): %v", err)
			}
			if err := <-promptDone; err != nil {
				t.Fatalf("Prompt: %v", err)
			}
		})
		for _, ev := range evs {
			if ev.Type == EventText {
				text.WriteString(ev.Text)
			}
		}
		if !strings.Contains(text.String(), "decision=accept") {
			t.Fatalf("expected decision=accept, got %q", text.String())
		}
	})

	t.Run("agent --force auto-accepts command approval", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-approval", Options{Force: true, Interactive: true})
		var text strings.Builder
		evs := collectSubEvents(t, cs, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := cs.Prompt(ctx, "run forced"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
		})
		for _, ev := range evs {
			if ev.Type == EventText {
				text.WriteString(ev.Text)
			}
		}
		if !strings.Contains(text.String(), "decision=accept") {
			t.Fatalf("expected decision=accept under Force, got %q", text.String())
		}
	})
}

func TestCodexCancelWithOpenApprovalCardAndServerRequestResolved(t *testing.T) {
	t.Run("Cancel unblocks open approval card and interrupts turn", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-approval", Options{Force: false, Interactive: true})
		promptRes := make(chan struct {
			res Result
			err error
		}, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, e := cs.Prompt(ctx, "wait on approval")
			promptRes <- struct {
				res Result
				err error
			}{r, e}
		}()

		open := waitForOpenAsk(t, cs, AskPermission)
		out, err := cs.Cancel(context.Background())
		if err != nil || !out.Settled {
			t.Fatalf("Cancel with open approval = (%+v, %v), want Settled=true nil err", out, err)
		}
		r := <-promptRes
		if r.err != nil || r.res.StopReason != acp.StopCancelled {
			t.Fatalf("Prompt after cancel = (%+v, %v), want StopCancelled", r.res, r.err)
		}
		if len(cs.Asks().Asks()) != 0 {
			t.Fatalf("open asks remain after cancel: %+v", cs.Asks().Asks())
		}
		// Verify ask was recorded resolved.
		found := false
		for _, rec := range cs.Asks().Resolved() {
			if rec.ID == open.ID && rec.Outcome == AskCancelled {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected resolved cancelled ask %q, got %+v", open.ID, cs.Asks().Resolved())
		}
	})

	t.Run("serverRequest/resolved retires parked ask immediately as withdrawn", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-approval", Options{Force: false, Interactive: true})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, _ = cs.Prompt(ctx, "will be resolved externally")
		}()
		open := waitForOpenAsk(t, cs, AskPermission)

		// Find the pending request ID key and synthesize serverRequest/resolved.
		cs.mu.Lock()
		var reqKey string
		for k := range cs.pendingReqs {
			reqKey = k
			break
		}
		tid := cs.threadID
		cs.mu.Unlock()
		if reqKey == "" {
			t.Fatal("expected pending request key registered while ask is parked")
		}
		payload, _ := json.Marshal(map[string]any{
			"threadId":  tid,
			"requestId": json.RawMessage(reqKey),
		})
		cs.handleServerRequestResolved(payload)

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if len(cs.Asks().Asks()) == 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if len(cs.Asks().Asks()) != 0 {
			t.Fatalf("parked ask was not retired on serverRequest/resolved: %+v", cs.Asks().Asks())
		}
		found := false
		for _, rec := range cs.Asks().Resolved() {
			if rec.ID == open.ID && rec.By == AskByWithdrawn {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected ask %q retired with By=withdrawn, got %+v", open.ID, cs.Asks().Resolved())
		}
	})
}

func TestCodexFileChangeApprovalInteractive(t *testing.T) {
	cs, _ := startCodexFake(t, "codex-filechange-approval", Options{Force: false, Interactive: true})
	promptDone := make(chan error, 1)
	var text strings.Builder
	evs := collectSubEvents(t, cs, func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := cs.Prompt(ctx, "edit file")
			promptDone <- err
		}()
		open := waitForOpenAsk(t, cs, AskPermission)
		if !strings.HasPrefix(open.Body.Permission.Tool, "apply_patch") {
			t.Fatalf("fileChange tool label = %q, want apply_patch prefix", open.Body.Permission.Tool)
		}
		if _, err := cs.Asks().Answer("client/1", open.ID, AskAnswer{OptionID: "accept"}); err != nil {
			t.Fatalf("Answer(accept): %v", err)
		}
		if err := <-promptDone; err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})
	for _, ev := range evs {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
	}
	if !strings.Contains(text.String(), "fc_decision=accept") {
		t.Fatalf("expected fc_decision=accept, got %q", text.String())
	}
}

func TestCodexRequestUserInputAttendedUnattendedSecretAndFreetext(t *testing.T) {
	t.Run("attended picks option and supports free text", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-ask", Options{Force: true, Interactive: true})
		promptDone := make(chan error, 1)
		var text strings.Builder
		evs := collectSubEvents(t, cs, func() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, err := cs.Prompt(ctx, "ask me")
				promptDone <- err
			}()
			open := waitForOpenAsk(t, cs, AskQuestion)
			if len(open.Body.Question.Questions) != 1 || open.Body.Question.Questions[0].Required {
				t.Fatalf("unexpected question body: %+v", open.Body.Question)
			}
			if _, err := cs.Asks().Answer("client/1", open.ID, AskAnswer{
				Answers: map[string][]string{"q1": {"Blue"}},
			}); err != nil {
				t.Fatalf("Answer(Blue): %v", err)
			}
			if err := <-promptDone; err != nil {
				t.Fatalf("Prompt: %v", err)
			}
		})
		for _, ev := range evs {
			if ev.Type == EventText {
				text.WriteString(ev.Text)
			}
		}
		if !strings.Contains(text.String(), `"Blue"`) {
			t.Fatalf("expected Blue answer echoed, got %q", text.String())
		}
	})

	t.Run("unattended with options auto-answers first option", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-ask", Options{Force: true, Interactive: false})
		var text strings.Builder
		evs := collectSubEvents(t, cs, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := cs.Prompt(ctx, "ask unattended"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
		})
		for _, ev := range evs {
			if ev.Type == EventText {
				text.WriteString(ev.Text)
			}
		}
		if !strings.Contains(text.String(), `"Red"`) {
			t.Fatalf("expected unattended first option Red, got %q", text.String())
		}
	})

	t.Run("isSecret question is declined immediately without parking", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-ask-secret", Options{Force: true, Interactive: true})
		var text strings.Builder
		evs := collectSubEvents(t, cs, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := cs.Prompt(ctx, "ask secret"); err != nil {
				t.Fatalf("Prompt: %v", err)
			}
		})
		for _, ev := range evs {
			if ev.Type == EventText {
				text.WriteString(ev.Text)
			}
		}
		if !strings.Contains(text.String(), `"answers":{}`) {
			t.Fatalf("expected empty answers for isSecret question, got %q", text.String())
		}
		if len(cs.Asks().Resolved()) != 0 {
			t.Fatalf("isSecret question must not be recorded in AskRegistry, got %+v", cs.Asks().Resolved())
		}
	})

	t.Run("unattended free-text-only parks ask instead of fabricating text", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-ask-freetext", Options{Force: true, Interactive: false})
		promptDone := make(chan error, 1)
		var text strings.Builder
		evs := collectSubEvents(t, cs, func() {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, err := cs.Prompt(ctx, "ask freetext unattended")
				promptDone <- err
			}()
			open := waitForOpenAsk(t, cs, AskQuestion)
			if !open.Body.Question.Questions[0].AllowText || len(open.Body.Question.Questions[0].Options) != 0 {
				t.Fatalf("expected free-text question with AllowText=true, got %+v", open.Body.Question.Questions[0])
			}
			if _, err := cs.Asks().Answer("client/1", open.ID, AskAnswer{
				Text: map[string]string{"q1": "custom value"},
			}); err != nil {
				t.Fatalf("Answer(freetext): %v", err)
			}
			if err := <-promptDone; err != nil {
				t.Fatalf("Prompt: %v", err)
			}
		})
		for _, ev := range evs {
			if ev.Type == EventText {
				text.WriteString(ev.Text)
			}
		}
		if !strings.Contains(text.String(), `"custom value"`) {
			t.Fatalf("expected custom value echoed, got %q", text.String())
		}
	})
}

func TestCodexInterjectTurnSteerAndOrdering(t *testing.T) {
	cs, _ := startCodexFake(t, "codex-steer", Options{Force: true})

	// Idle Interject returns ErrPromptRequired so Engine queues cleanly.
	if err := cs.Interject(context.Background(), "too early"); !errors.Is(err, ErrPromptRequired) {
		t.Fatalf("idle Interject = %v, want ErrPromptRequired", err)
	}

	var promptErr error
	var steerErr error
	evs := collectSubEvents(t, cs, func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, promptErr = cs.Prompt(ctx, "start long work")
		}()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			cs.mu.Lock()
			ready := cs.activeTurn != nil && cs.activeTurn.phase == codexPhaseIdentified
			cs.mu.Unlock()
			if ready {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		steerErr = cs.Interject(context.Background(), "focus on tests")
		<-done
	})
	if promptErr != nil || steerErr != nil {
		t.Fatalf("Prompt=%v Interject=%v", promptErr, steerErr)
	}

	userIdx, doneIdx, userCount := -1, -1, 0
	var text strings.Builder
	for i, ev := range evs {
		switch ev.Type {
		case EventUser:
			userCount++
			if ev.Interjection && ev.Text == "focus on tests" {
				userIdx = i
			}
		case EventDone:
			doneIdx = i
		case EventText:
			text.WriteString(ev.Text)
		}
	}
	if userCount != 1 || userIdx < 0 || doneIdx < 0 || userIdx >= doneIdx {
		t.Fatalf("expected 1 interjection EventUser ahead of EventDone (count=%d userIdx=%d doneIdx=%d)", userCount, userIdx, doneIdx)
	}
	if !strings.Contains(text.String(), "done:focus on tests") {
		t.Fatalf("steered reply = %q, want done:focus on tests", text.String())
	}
}

func TestCodexForeignTurnRefusesBeginUntilCompleted(t *testing.T) {
	cs, _ := startCodexFake(t, "codex-foreign", Options{Force: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cs.Prompt(ctx, "TRIGGER_FOREIGN"); err != nil {
		t.Fatalf("Prompt(TRIGGER_FOREIGN): %v", err)
	}

	// Wait for autonomous foreign turn to start.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !cs.ForeignTurn() {
		time.Sleep(5 * time.Millisecond)
	}
	if !cs.ForeignTurn() {
		t.Fatal("expected ForeignTurn() == true during autonomous turn")
	}
	if _, err := cs.Begin("blocked")(ctx); !errors.Is(err, ErrForeignTurn) {
		t.Fatalf("Begin during foreign turn = %v, want ErrForeignTurn", err)
	}

	// Wait for foreign turn to finish, then Begin succeeds.
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && cs.ForeignTurn() {
		time.Sleep(5 * time.Millisecond)
	}
	if cs.ForeignTurn() {
		t.Fatal("ForeignTurn() did not clear after foreign turn/completed")
	}
	if _, err := cs.Prompt(ctx, "after foreign"); err != nil {
		t.Fatalf("Prompt after foreign turn: %v", err)
	}
}

func TestCodexChildThreadIsolationAndSubagentRows(t *testing.T) {
	cs, _ := startCodexFake(t, "codex-child-thread", Options{Force: true})
	var text strings.Builder
	var subEvs []Event
	evs := collectSubEvents(t, cs, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := cs.Prompt(ctx, "spawn child"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})
	for _, ev := range evs {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
		if ev.Type == EventSubagent && ev.Subagent != nil {
			subEvs = append(subEvs, ev)
		}
	}
	if strings.Contains(text.String(), "CHILD_SECRET_DO_NOT_BLEED") {
		t.Fatalf("child-thread text leaked into main transcript: %q", text.String())
	}
	if !strings.Contains(text.String(), "main-only") {
		t.Fatalf("main text missing: %q", text.String())
	}
	if len(subEvs) < 2 {
		t.Fatalf("expected Subagent events (spawned + finished), got %d: %+v", len(subEvs), subEvs)
	}
	snap := cs.Snapshot()
	if len(snap.Subagents) != 1 || snap.Subagents[0].ID != "sub-1" || snap.Subagents[0].Status != SubagentCompleted {
		t.Fatalf("Snapshot.Subagents = %+v, want 1 completed sub-1", snap.Subagents)
	}
}

func TestCodexPaginatedResumeReplayChronologicalAndStaleIDRefusal(t *testing.T) {
	t.Run("multi-page desc turns reversed chronologically under EventReplay brackets", func(t *testing.T) {
		cs, initEvs := startCodexFake(t, "codex-resume", Options{
			LoadSessionID: "thr-prev-1",
			Force:         true,
		})
		startIdx, endIdx := -1, -1
		var users, texts, thoughts []string
		var interruptedCmd *ToolEvent
		for i, ev := range initEvs {
			if ev.Type == EventReplay && ev.Replay != nil {
				switch ev.Replay.Phase {
				case ReplayStart:
					startIdx = i
				case ReplayEnd:
					endIdx = i
				}
				continue
			}
			if startIdx >= 0 && endIdx < 0 {
				if !ev.Replayed {
					t.Fatalf("event inside replay bracket missing Replayed=true: %+v", ev)
				}
			}
			switch ev.Type {
			case EventUser:
				users = append(users, ev.Text)
			case EventText:
				texts = append(texts, ev.Text)
			case EventThought:
				thoughts = append(thoughts, ev.Text)
			case EventTool:
				if ev.Tool != nil && ev.Tool.ID == "cmd-2" {
					cp := *ev.Tool
					interruptedCmd = &cp
				}
			}
		}
		if startIdx < 0 || endIdx <= startIdx {
			t.Fatalf("missing valid EventReplay brackets (start=%d end=%d)", startIdx, endIdx)
		}
		if !slices.Equal(users, []string{"first question", "second question"}) {
			t.Fatalf("replayed user order = %v, want [first question, second question]", users)
		}
		if !slices.Equal(texts, []string{"first answer", "second answer"}) {
			t.Fatalf("replayed assistant texts = %v, want [first answer, second answer]", texts)
		}
		if !slices.Equal(thoughts, []string{"thought about first"}) {
			t.Fatalf("replayed thoughts = %v, want [thought about first]", thoughts)
		}
		if interruptedCmd == nil || interruptedCmd.Status != "cancelled" {
			t.Fatalf("interrupted commandExecution on replay = %+v, want status=cancelled", interruptedCmd)
		}
		snap := cs.Snapshot()
		if snap.CurrentModel != "gpt-6.2-astra" {
			t.Fatalf("restored model = %q, want gpt-6.2-astra", snap.CurrentModel)
		}
		if opt := EffortOption(snap); opt == nil || opt.Current != "medium" {
			t.Fatalf("restored effort = %+v, want medium", opt)
		}
		if snap.Usage == nil || snap.Usage.Session.Input != 500 || snap.Usage.Turn.Input != 0 {
			t.Fatalf("resumed usage baseline = %+v, want Session.Input=500 and Turn.Input=0", snap.Usage)
		}
	})

	t.Run("truncation notice emitted when resume hits turn cap", func(t *testing.T) {
		prev := codexResumeMaxTurns
		codexResumeMaxTurns = 1
		t.Cleanup(func() { codexResumeMaxTurns = prev })

		_, initEvs := startCodexFake(t, "codex-resume", Options{
			LoadSessionID: "thr-prev-1",
			Force:         true,
		})
		sawNotice := false
		for _, ev := range initEvs {
			if ev.Type == EventNotice && ev.Notice != nil && ev.Notice.Title == "History truncated" {
				sawNotice = true
			}
		}
		if !sawNotice {
			t.Fatalf("expected History truncated notice when capped, got %+v", initEvs)
		}
	})

	t.Run("stale/missing thread ID fails Start directly without silent new session", func(t *testing.T) {
		bin := fakeAgentPath(t)
		prov := CodexProvider()
		cs := New(Options{
			Provider:      &prov,
			Binary:        bin,
			Workspace:     t.TempDir(),
			LoadSessionID: "thr-missing-999",
			Env:           append(os.Environ(), "CRAZE_FAKE_SCRIPT=codex-resume-missing"),
		}).(*codexSession)
		defer func() { _ = cs.Close() }()
		if err := cs.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "no rollout found") {
			t.Fatalf("Start(missing thread) = %v, want no rollout found error", err)
		}
	})
}

func TestCodexSkillsCatalogInvocationRescanAndBuiltinPrecedence(t *testing.T) {
	dumpPath := filepath.Join(t.TempDir(), "prompts.jsonl")
	cs, _ := startCodexFake(t, "codex-basic", Options{Force: true}, "CRAZE_FAKE_DUMP_PROMPTS="+dumpPath)

	// Enabled skill fake-echo appears in Snapshot.Slash; disabled-skill does NOT.
	snap := cs.Snapshot()
	foundSkill, foundDisabled := false, false
	for _, it := range snap.Slash {
		if it.Name == "fake-echo" && it.Source == SlashSourceSkill {
			foundSkill = true
		}
		if it.Name == "disabled-skill" {
			foundDisabled = true
		}
	}
	if !foundSkill || foundDisabled {
		t.Fatalf("Snapshot.Slash skills mismatch (foundSkill=%v foundDisabled=%v): %+v", foundSkill, foundDisabled, snap.Slash)
	}

	// Inject a synthetic skill colliding with Craze builtin /help: builtin must win!
	cs.mu.Lock()
	cs.codexSkills["help"] = codexrpc.SkillMetadata{Name: "help", Path: "/tmp/help/SKILL.md", Enabled: true}
	cs.mu.Unlock()
	if _, _, ok := cs.parseCodexSkillPrompt("/help me"); ok {
		t.Fatal("Craze builtin /help must win over colliding Codex skill")
	}

	// Prompting /fake-echo sends UserInput{Type: "skill"} + remaining text!
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var text strings.Builder
	evs := collectSubEvents(t, cs, func() {
		if _, err := cs.Prompt(ctx, "/fake-echo run check"); err != nil {
			t.Fatalf("Prompt(/fake-echo): %v", err)
		}
	})
	for _, ev := range evs {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
	}
	if !strings.Contains(text.String(), "skills=fake-echo") {
		t.Fatalf("expected skills=fake-echo echoed by fake server, got %q", text.String())
	}
	b, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read prompt dump: %v", err)
	}
	if !strings.Contains(string(b), `"type":"skill"`) || !strings.Contains(string(b), `"name":"fake-echo"`) {
		t.Fatalf("turn/start dump missing skill input: %s", string(b))
	}

	// RescanSkills must be non-blocking and safe.
	cs.RescanSkills()
}
