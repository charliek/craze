package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
		srvToCliR, srvToCliW := io.Pipe()
		cliToSrvR, cliToSrvW := io.Pipe()
		defer srvToCliR.Close()
		defer srvToCliW.Close()
		defer cliToSrvR.Close()
		defer cliToSrvW.Close()

		prov := CodexProvider()
		cs := New(Options{
			Provider:    &prov,
			Workspace:   t.TempDir(),
			Force:       false,
			Interactive: true,
		}).(*codexSession)
		client := codexrpc.NewClient(srvToCliR, cliToSrvW)
		client.SetNotifyHandler(cs.onNotify)
		client.SetRequestHandler(cs.onServerRequest)
		client.Start()
		t.Cleanup(func() {
			_ = client.Close()
			_ = cs.Close()
		})

		// Drain any client writes (none expected for withdrawn ask) so pipe never blocks.
		go func() { _, _ = io.Copy(io.Discard, cliToSrvR) }()

		turn := &codexTurnState{
			gen:     1,
			phase:   codexPhaseIdentified,
			turnID:  "turn-1",
			token:   cs.asks.BeginTurn(),
			mode:    codexAgentMode,
			idReady: make(chan struct{}),
			done:    make(chan struct{}),
		}
		close(turn.idReady)
		cs.mu.Lock()
		cs.client = client
		cs.threadID = "thr-1"
		cs.activeTurn = turn
		cs.ready = true
		cs.mu.Unlock()

		// 1. Wire peer sends approval request id=0 -> parks ask in AskRegistry.
		if _, err := io.WriteString(srvToCliW, `{"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr-1","turnId":"turn-1","itemId":"cmd-1","command":"touch out.txt"}}`+"\n"); err != nil {
			t.Fatalf("write request 0: %v", err)
		}
		open := waitForOpenAsk(t, cs, AskPermission)

		// 2. Wire peer sends serverRequest/resolved for id=0 -> wire-ordered
		// cancellation in codexrpc.Client retires the parked ask as withdrawn.
		if _, err := io.WriteString(srvToCliW, `{"method":"serverRequest/resolved","params":{"threadId":"thr-1","requestId":0}}`+"\n"); err != nil {
			t.Fatalf("write serverRequest/resolved: %v", err)
		}

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if len(cs.Asks().Asks()) == 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if len(cs.Asks().Asks()) != 0 {
			t.Fatalf("parked ask was not retired on wire serverRequest/resolved: %+v", cs.Asks().Asks())
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

		// 3. Wire peer reuses id=0 for a new approval request -> a delayed
		// handleServerRequestResolved notification MUST NOT withdraw it.
		if _, err := io.WriteString(srvToCliW, `{"id":0,"method":"item/commandExecution/requestApproval","params":{"threadId":"thr-1","turnId":"turn-1","itemId":"cmd-2","command":"touch out2.txt"}}`+"\n"); err != nil {
			t.Fatalf("write reused request 0: %v", err)
		}
		open2 := waitForOpenAsk(t, cs, AskPermission)
		stalePayload, _ := json.Marshal(map[string]any{
			"threadId":  "thr-1",
			"requestId": 0,
		})
		cs.handleServerRequestResolved(stalePayload)
		time.Sleep(50 * time.Millisecond)
		stillOpen := cs.Asks().Asks()
		if len(stillOpen) != 1 || stillOpen[0].ID != open2.ID {
			t.Fatalf("stale delayed handleServerRequestResolved withdrew reused request %q: asks=%+v", open2.ID, stillOpen)
		}
		// Cleanly end the test turn so cleanup closes without lingering asks.
		close(turn.done)
		cs.mu.Lock()
		cs.activeTurn = nil
		cs.mu.Unlock()
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
		cs, _ := startCodexFake(t, "codex-ask-freetext", Options{Force: true, Interactive: false, Unattended: true})
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

func TestCodexM2bReviewRegressions(t *testing.T) {
	t.Run("M5: Probe 1 availableDecisions preserves Decline and empty slice invents no allow", func(t *testing.T) {
		// Exact Probe 1 frame shape: ["accept", {"acceptWithExecpolicyAmendment":...}, "cancel"]
		probe1 := []json.RawMessage{
			json.RawMessage(`"accept"`),
			json.RawMessage(`{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["touch","x"]}}`),
			json.RawMessage(`"cancel"`),
		}
		got := safeCodexCommandDecisions(probe1)
		if !slices.Equal(got, []string{"accept", "decline", "cancel"}) {
			t.Fatalf("Probe 1 decisions = %v, want [accept decline cancel]", got)
		}
		// Explicit empty slice [] must NOT invent accept or acceptForSession.
		empty := safeCodexCommandDecisions([]json.RawMessage{})
		if slices.Contains(empty, "accept") || slices.Contains(empty, "acceptForSession") || !slices.Contains(empty, "decline") {
			t.Fatalf("explicit empty decisions = %v, want only safe decline", empty)
		}
		// Nil slice uses legacy 4 defaults.
		nilDec := safeCodexCommandDecisions(nil)
		if !slices.Equal(nilDec, []string{"accept", "acceptForSession", "decline", "cancel"}) {
			t.Fatalf("nil decisions = %v", nilDec)
		}
	})

	t.Run("M7: collabAgentToolCall agentsStates running and subAgentActivity item shape", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-basic", Options{Force: true})
		// Spawn call status is "completed", but agentsStates says receiver child is still running.
		spawnRaw := json.RawMessage(`{"type":"collabAgentToolCall","id":"sub-x","tool":"spawn_agent","status":"completed","receiverThreadIds":["child-thr-9"],"prompt":"analyze","agentsStates":{"child-thr-9":{"status":"running"}}}`)
		cs.processItemCtx(context.Background(), spawnRaw, true, false)
		snap := cs.Snapshot()
		if len(snap.Subagents) != 1 || snap.Subagents[0].Status != SubagentRunning {
			t.Fatalf("expected SubagentRunning from agentsStates, got %+v", snap.Subagents)
		}
		// Upstream subAgentActivity as a ThreadItem correlated by agentThreadId.
		actRaw := json.RawMessage(`{"type":"subAgentActivity","id":"act-1","agentThreadId":"child-thr-9","kind":"tool","status":"completed","activity":"done reading"}`)
		cs.processItemCtx(context.Background(), actRaw, true, false)
		snap = cs.Snapshot()
		if len(snap.Subagents) != 1 || snap.Subagents[0].Status != SubagentCompleted || snap.Subagents[0].Activity != "done reading" {
			t.Fatalf("subAgentActivity item did not correlate/update row: %+v", snap.Subagents)
		}
	})

	t.Run("M8: compatibility fallback respects turn cap and keeps newest suffix", func(t *testing.T) {
		prev := codexResumeMaxTurns
		codexResumeMaxTurns = 2
		t.Cleanup(func() { codexResumeMaxTurns = prev })

		turns := []codexrpc.Turn{
			{ID: "t1"}, {ID: "t2"}, {ID: "t3"}, {ID: "t4"},
		}
		h := boundChronologicalTurns(turns)
		if !h.truncated || len(h.turns) != 2 || h.turns[0].ID != "t3" || h.turns[1].ID != "t4" {
			t.Fatalf("boundChronologicalTurns = %+v, want truncated newest suffix [t3 t4]", h)
		}
	})

	t.Run("L10: skills/changed before ready sets skillsDirty", func(t *testing.T) {
		prov := CodexProvider()
		cs := New(Options{Provider: &prov, Workspace: t.TempDir()}).(*codexSession)
		defer func() { _ = cs.Close() }()
		cs.handleSkillsChanged()
		cs.mu.Lock()
		dirty := cs.skillsDirty
		cs.mu.Unlock()
		if !dirty {
			t.Fatal("expected skillsDirty=true when skills/changed arrives before ready")
		}
	})
}

func TestCodexDogfoodReviewFixes(t *testing.T) {
	t.Run("H1: clearTurnLocked never double-closes a settling turn", func(t *testing.T) {
		prov := CodexProvider()
		cs := New(Options{Provider: &prov, Workspace: t.TempDir()}).(*codexSession)
		defer func() { _ = cs.Close() }()
		turn := &codexTurnState{
			gen:      1,
			phase:    codexPhaseClaimed,
			settling: true,
			idReady:  make(chan struct{}),
			done:     make(chan struct{}),
		}
		cs.mu.Lock()
		cs.activeTurn = turn
		cs.clearTurnLocked(turn)
		stillActive := cs.activeTurn == turn
		cs.mu.Unlock()
		if !stillActive {
			t.Fatal("clearTurnLocked cleared activeTurn while settleTurn owned it")
		}
		select {
		case <-turn.done:
			t.Fatal("clearTurnLocked closed turn.done while settling=true")
		default:
		}
		// Settlement can now close turn.done without panicking.
		close(turn.done)
	})

	t.Run("H3: Close does not deadlock when primary channel is saturated during settlement", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-basic", Options{Force: true})
		// Fill the primary Events() channel completely without draining it.
		for len(cs.events) < cap(cs.events) {
			if !cs.log.Publish(context.Background(), cs.done, Event{Type: EventText, Text: "fill"}) {
				break
			}
		}
		turn := &codexTurnState{
			gen:     1,
			phase:   codexPhaseIdentified,
			turnID:  "turn-sat",
			idReady: make(chan struct{}),
			done:    make(chan struct{}),
		}
		cs.mu.Lock()
		cs.activeTurn = turn
		cs.mu.Unlock()

		// Start settlement BEFORE Close so closing==false initially; settlement
		// blocks trying to emit EventDone onto the full primary channel.
		settleDone := make(chan struct{})
		go func() {
			_, _ = cs.settleTurn(turn, acp.StopEndTurn, nil, true)
			close(settleDone)
		}()
		time.Sleep(30 * time.Millisecond)

		closeDone := make(chan error, 1)
		go func() { closeDone <- cs.Close() }()
		select {
		case <-closeDone:
		case <-time.After(3 * time.Second):
			t.Fatal("Close deadlocked behind saturated primary channel")
		}
		select {
		case <-settleDone:
		case <-time.After(3 * time.Second):
			t.Fatal("settleTurn did not unblock on Close")
		}
	})

	t.Run("M1: standalone headless (craze prompt) declines free-text-only question instead of hanging", func(t *testing.T) {
		// Interactive: false, Unattended: false mirrors `craze prompt`.
		cs, _ := startCodexFake(t, "codex-ask-freetext", Options{
			Force:       true,
			Interactive: false,
			Unattended:  false,
			Approval: &ApprovalPolicy{
				Permission: ApprovalAllow,
				Question:   ApprovalFirstOption,
				Plan:       ApprovalAccept,
			},
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := cs.Prompt(ctx, "ask free text"); err != nil {
			t.Fatalf("Prompt hung or failed on free-text question in standalone headless mode: %v", err)
		}
	})

	t.Run("M3: expired Cancel ctx allows subsequent Cancel to send turn/interrupt", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-hang", Options{Force: true})
		promptDone := make(chan error, 1)
		go func() {
			_, err := cs.Prompt(context.Background(), "hang forever")
			promptDone <- err
		}()
		// Wait until turnID is bound.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			cs.mu.Lock()
			ready := cs.activeTurn != nil && cs.activeTurn.turnID != ""
			cs.mu.Unlock()
			if ready {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		// First Cancel with an already-expired ctx must NOT latch interruptSent.
		expCtx, expCancel := context.WithCancel(context.Background())
		expCancel()
		_, err := cs.Cancel(expCtx)
		if err == nil {
			t.Fatal("expected expired Cancel to return error")
		}
		// Second Cancel with a fresh ctx must succeed and settle the turn.
		freshCtx, freshCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer freshCancel()
		out, err := cs.Cancel(freshCtx)
		if err != nil || !out.Settled {
			t.Fatalf("second Cancel failed: outcome=%+v err=%v", out, err)
		}
		select {
		case <-promptDone:
		case <-time.After(3 * time.Second):
			t.Fatal("Prompt did not settle after retry Cancel")
		}
	})

	t.Run("M4: collabAgentToolCall follow-up updates existing row and subAgentActivity kind maps status", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-basic", Options{Force: true})
		// Initial spawn creates 1 row mapped to child-1.
		spawnRaw := json.RawMessage(`{"type":"collabAgentToolCall","id":"spawn-1","tool":"spawn_agent","status":"completed","receiverThreadIds":["child-1"],"prompt":"inspect","agentsStates":{"child-1":{"status":"running"}}}`)
		cs.processItemCtx(context.Background(), spawnRaw, true, false)

		// Follow-up wait call referencing child-1 must update spawn-1 in place without creating a second row.
		waitRaw := json.RawMessage(`{"type":"collabAgentToolCall","id":"wait-1","tool":"wait","status":"completed","receiverThreadIds":["child-1"],"agentsStates":{"child-1":{"status":"running"}}}`)
		cs.processItemCtx(context.Background(), waitRaw, true, false)

		snap := cs.Snapshot()
		if len(snap.Subagents) != 1 || snap.Subagents[0].ID != "spawn-1" || snap.Subagents[0].Status != SubagentRunning {
			t.Fatalf("follow-up collabAgentToolCall created duplicate or wrong status: %+v", snap.Subagents)
		}

		// Upstream subAgentActivity with kind="completed" and empty status must transition spawn-1 to SubagentCompleted.
		actRaw := json.RawMessage(`{"type":"subAgentActivity","id":"act-done","agentThreadId":"child-1","kind":"completed","activity":"finished analysis"}`)
		cs.processItemCtx(context.Background(), actRaw, true, false)
		snap = cs.Snapshot()
		if len(snap.Subagents) != 1 || snap.Subagents[0].ID != "spawn-1" || snap.Subagents[0].Status != SubagentCompleted {
			t.Fatalf("subAgentActivity kind=completed did not settle spawn-1: %+v", snap.Subagents)
		}
	})

	t.Run("L1: expired quota bucket does not mask fresh active bucket", func(t *testing.T) {
		now := time.Unix(1_800_000_000, 0).UTC()
		past := now.Add(-10 * time.Minute).Unix()
		future := now.Add(2 * time.Hour).Unix()
		pct100 := 100.0
		pct25 := 25.0
		w300 := 300
		w10080 := 10080

		var u codexUsage
		u.mergeRateLimits(&codexrpc.RateLimitsSnapshot{
			LimitID:  "codex",
			PlanType: "prolite",
			Primary: &codexrpc.RateLimitWindow{
				UsedPercent:        &pct100,
				WindowDurationMins: &w300,
				ResetsAt:           &past,
			},
			Secondary: &codexrpc.RateLimitWindow{
				UsedPercent:        &pct25,
				WindowDurationMins: &w10080,
				ResetsAt:           &future,
			},
		})
		st := u.stateAt(now)
		if st.Window != "seven_day" || st.WindowUtilization != 0.25 {
			t.Fatalf("expired bucket masked active bucket: got window=%q util=%v", st.Window, st.WindowUtilization)
		}
	})
}

func TestCodexPR156ReviewFixes(t *testing.T) {
	t.Run("isCodexTerminalTurnStatus excludes in-progress turns", func(t *testing.T) {
		for _, active := range []string{"inProgress", "in_progress", "IN_PROGRESS", "running", " Running "} {
			if isCodexTerminalTurnStatus(active) {
				t.Fatalf("isCodexTerminalTurnStatus(%q) = true, want false", active)
			}
		}
		for _, done := range []string{"completed", "interrupted", "failed", ""} {
			if !isCodexTerminalTurnStatus(done) {
				t.Fatalf("isCodexTerminalTurnStatus(%q) = false, want true", done)
			}
		}
	})

	t.Run("abortHungTurn after settlement never closes next turn client", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-basic", Options{Force: true})
		settled := &codexTurnState{
			gen:     1,
			phase:   codexPhaseSettled,
			turnID:  "turn-old",
			idReady: make(chan struct{}),
			done:    make(chan struct{}),
		}
		close(settled.done)

		cs.mu.Lock()
		client := cs.client
		cs.mu.Unlock()

		if cs.abortHungTurn(settled, client, errors.New("late grace timeout")) {
			t.Fatal("abortHungTurn returned true for already-settled turn")
		}
		// Transport must still be completely healthy for a subsequent prompt.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := cs.Prompt(ctx, "still healthy"); err != nil {
			t.Fatalf("Prompt after late abortHungTurn failed: %v", err)
		}
	})

	t.Run("checkResumeAliveLocked rejects dead transport", func(t *testing.T) {
		prov := CodexProvider()
		cs := New(Options{Provider: &prov, Workspace: t.TempDir()}).(*codexSession)
		defer func() { _ = cs.Close() }()

		cs.mu.Lock()
		if err := cs.checkResumeAliveLocked(nil); err != nil {
			cs.mu.Unlock()
			t.Fatalf("expected alive session, got %v", err)
		}
		cs.transportDead = true
		err := cs.checkResumeAliveLocked(nil)
		cs.mu.Unlock()
		if err == nil {
			t.Fatal("expected checkResumeAliveLocked to reject transportDead=true")
		}
	})
}
