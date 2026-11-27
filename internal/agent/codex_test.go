package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/codexrpc"
	"github.com/charliek/craze/internal/errkind"
)

func startCodexFake(t *testing.T, script string, opts Options, extraEnv ...string) (*codexSession, []Event) {
	t.Helper()
	bin := fakeAgentPath(t)
	prov := CodexProvider()
	if opts.Provider == nil {
		opts.Provider = &prov
	}
	if opts.Binary == "" {
		opts.Binary = bin
	}
	if opts.Workspace == "" {
		opts.Workspace = t.TempDir()
	}
	env := append([]string(nil), os.Environ()...)
	if script != "" {
		env = append(env, "CRAZE_FAKE_SCRIPT="+script)
	}
	env = append(env, extraEnv...)
	opts.Env = env

	sess := New(opts)
	cs, ok := sess.(*codexSession)
	if !ok {
		t.Fatalf("New(CodexProvider) returned %T, want *codexSession", sess)
	}
	t.Cleanup(func() {
		_ = cs.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := cs.Start(ctx); err != nil {
		t.Fatalf("Start(%s): %v", script, err)
	}
	if err := cs.EventLog().Flush(ctx, cs.done); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	return cs, drainCodexPrimary(cs)
}

func drainCodexPrimary(cs *codexSession) []Event {
	var evs []Event
	for {
		select {
		case ev, ok := <-cs.Events():
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		default:
			return evs
		}
	}
}

func collectSubEvents(t *testing.T, cs *codexSession, fn func()) []Event {
	t.Helper()
	// Drain any events already buffered on the primary channel before fn starts.
	_ = drainCodexPrimary(cs)
	var (
		mu   sync.Mutex
		evs  []Event
		stop = make(chan struct{})
		done = make(chan struct{})
	)
	go func() {
		defer close(done)
		for {
			select {
			case ev, ok := <-cs.Events():
				if !ok {
					return
				}
				mu.Lock()
				evs = append(evs, ev)
				mu.Unlock()
			case <-stop:
				for {
					select {
					case ev, ok := <-cs.Events():
						if !ok {
							return
						}
						mu.Lock()
						evs = append(evs, ev)
						mu.Unlock()
					default:
						return
					}
				}
			}
		}
	}()
	fn()
	_ = cs.EventLog().Flush(context.Background(), cs.done)
	close(stop)
	<-done
	mu.Lock()
	defer mu.Unlock()
	return evs
}

func TestCodexStartInstallDeltaAndBasicPrompt(t *testing.T) {
	cs, initEvs := startCodexFake(t, "codex-basic", Options{Force: true, Title: "My Codex", TitlePinned: true})

	// Install delta MUST carry non-nil StateDelta.Config so modelcache observes Codex models.
	sawConfig := false
	for _, ev := range initEvs {
		if ev.Type == EventMeta && ev.State != nil && ev.State.Config != nil {
			sawConfig = true
		}
	}
	if !sawConfig {
		t.Fatalf("Start install delta did not include non-nil StateDelta.Config: %+v", initEvs)
	}

	snap := cs.Snapshot()
	if snap.SessionID == "" || snap.CurrentModel != "gpt-6.2-sol" || snap.CurrentMode != "agent" {
		t.Fatalf("unexpected snapshot: id=%q model=%q mode=%q", snap.SessionID, snap.CurrentModel, snap.CurrentMode)
	}
	if snap.Title != "My Codex" {
		t.Fatalf("Title = %q, want My Codex", snap.Title)
	}
	if len(snap.Models) < 3 {
		t.Fatalf("expected >= 3 visible models, got %+v", snap.Models)
	}
	// Hidden model must not appear in visible catalog unless active.
	for _, m := range snap.Models {
		if m.ID == "gpt-hidden-beta" {
			t.Fatalf("hidden model leaked into visible Snapshot.Models: %+v", snap.Models)
		}
		if m.ID == "gpt-6.2-sol" {
			if !slices.Contains(m.Efforts, "ultra") || m.DefaultEffort == "" || m.SwitchEffort == "" {
				t.Fatalf("gpt-6.2-sol missing effort facts: %+v", m)
			}
		}
	}
	if opt := EffortOption(snap); opt == nil || opt.ID != "effort" {
		t.Fatalf("EffortOption = %+v, want id=effort", opt)
	}
	if snap.Usage == nil || snap.Usage.Window != "seven_day" {
		t.Fatalf("initial Usage = %+v, want Window=seven_day from rateLimits/read", snap.Usage)
	}

	var res Result
	var promptErr error
	evs := collectSubEvents(t, cs, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, promptErr = cs.Prompt(ctx, "hello world")
	})
	if promptErr != nil {
		t.Fatalf("Prompt: %v", promptErr)
	}
	if res.StopReason != acp.StopEndTurn {
		t.Fatalf("StopReason = %q, want %q", res.StopReason, acp.StopEndTurn)
	}

	var text strings.Builder
	for _, ev := range evs {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
		if ev.Type == EventUser {
			t.Fatalf("live userMessage echo must be suppressed, got EventUser: %+v", ev)
		}
	}
	if !strings.Contains(text.String(), "hello world") {
		t.Fatalf("streamed text = %q, want hello world", text.String())
	}

	// Verify token math: inputTokens (1100) - cachedInputTokens (600) = 500 uncached input.
	after := cs.Snapshot()
	if after.Usage == nil {
		t.Fatal("missing Usage after turn")
	}
	if after.Usage.Session.Input != 500 || after.Usage.Session.CacheRead != 600 ||
		after.Usage.Session.Output != 100 || after.Usage.Session.Reasoning != 25 ||
		!after.Usage.Session.Unpriced || after.Usage.ContextWindow != 475000 {
		t.Fatalf("unexpected Usage after turn: %+v", after.Usage)
	}
}

func TestCodexAuthPreflightLoggedOutAPIKeyAndCustom(t *testing.T) {
	bin := fakeAgentPath(t)
	prov := CodexProvider()

	t.Run("logged out fails fast with errkind.Auth", func(t *testing.T) {
		cs := New(Options{
			Provider:  &prov,
			Binary:    bin,
			Workspace: t.TempDir(),
			Env:       append(os.Environ(), "CRAZE_FAKE_SCRIPT=codex-loggedout"),
			NoPrimary: true,
		}).(*codexSession)
		defer func() { _ = cs.Close() }()
		start := time.Now()
		err := cs.Start(context.Background())
		if err == nil {
			t.Fatal("expected auth error when logged out")
		}
		if ErrorInfo(err).Kind != errkind.Auth {
			t.Fatalf("ErrorInfo(%v).Kind = %v, want auth", err, ErrorInfo(err).Kind)
		}
		if !strings.Contains(err.Error(), "codex login") {
			t.Fatalf("auth error missing codex login hint: %v", err)
		}
		if time.Since(start) > 3*time.Second {
			t.Fatalf("logged-out preflight took %v, expected fast refusal", time.Since(start))
		}
	})

	t.Run("apiKey auth succeeds", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-apikey", Options{Force: true})
		if cs.Snapshot().SessionID == "" {
			t.Fatal("expected session started with apiKey auth")
		}
	})

	t.Run("custom provider !requiresOpenaiAuth succeeds", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-custom-provider", Options{Force: true})
		if cs.Snapshot().SessionID == "" {
			t.Fatal("expected session started with custom provider auth")
		}
	})
}

func TestCodexModesHardReadOnlyAndPlanTransitions(t *testing.T) {
	dumpPath := filepath.Join(t.TempDir(), "prompts.jsonl")
	// Start in --ask with --no-force: approvalPolicy MUST still be "never" and sandboxPolicy "readOnly".
	cs, _ := startCodexFake(t, "codex-basic", Options{Mode: "ask", Force: false}, "CRAZE_FAKE_DUMP_PROMPTS="+dumpPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := cs.Prompt(ctx, "turn-ask"); err != nil {
		t.Fatalf("ask prompt: %v", err)
	}
	if _, err := cs.SetMode(ctx, "", "plan"); err != nil {
		t.Fatalf("SetMode(plan): %v", err)
	}
	var planText strings.Builder
	evs := collectSubEvents(t, cs, func() {
		if _, err := cs.Prompt(ctx, "turn-plan"); err != nil {
			t.Fatalf("plan prompt: %v", err)
		}
	})
	for _, ev := range evs {
		if ev.Type == EventText {
			planText.WriteString(ev.Text)
		}
	}
	// Plan delta dedup: "Plan for: turn-plan" must appear ONCE, not duplicated on item/completed.
	if strings.Count(planText.String(), "Plan for: turn-plan") != 1 {
		t.Fatalf("plan text duplicated or missing: %q", planText.String())
	}

	if _, err := cs.SetMode(ctx, "", "agent"); err != nil {
		t.Fatalf("SetMode(agent): %v", err)
	}
	if _, err := cs.Prompt(ctx, "turn-agent"); err != nil {
		t.Fatalf("agent prompt: %v", err)
	}

	b, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read prompt dump: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 dumped turns, got %d:\n%s", len(lines), string(b))
	}
	type dumped struct {
		TurnStart struct {
			ApprovalPolicy string `json:"approvalPolicy"`
			SandboxPolicy  struct {
				Type string `json:"type"`
			} `json:"sandboxPolicy"`
			CollaborationMode struct {
				Mode     string `json:"mode"`
				Settings struct {
					Model                 string  `json:"model"`
					ReasoningEffort       *string `json:"reasoning_effort"`
					DeveloperInstructions *string `json:"developer_instructions"`
				} `json:"settings"`
			} `json:"collaborationMode"`
		} `json:"turnStart"`
	}
	var d [3]dumped
	for i := range 3 {
		if err := json.Unmarshal([]byte(lines[i]), &d[i]); err != nil {
			t.Fatalf("unmarshal line %d: %v", i, err)
		}
	}
	// Turn 0 (ask, --no-force): readOnly + never + collaborationMode default.
	if d[0].TurnStart.SandboxPolicy.Type != "readOnly" || d[0].TurnStart.ApprovalPolicy != "never" || d[0].TurnStart.CollaborationMode.Mode != "default" {
		t.Fatalf("turn 0 (ask) = %+v", d[0].TurnStart)
	}
	// Turn 1 (plan, --no-force): readOnly + never + collaborationMode plan.
	if d[1].TurnStart.SandboxPolicy.Type != "readOnly" || d[1].TurnStart.ApprovalPolicy != "never" || d[1].TurnStart.CollaborationMode.Mode != "plan" {
		t.Fatalf("turn 1 (plan) = %+v", d[1].TurnStart)
	}
	// Turn 2 (agent, --no-force): workspaceWrite + on-request + explicit collaborationMode default.
	if d[2].TurnStart.SandboxPolicy.Type != "workspaceWrite" || d[2].TurnStart.ApprovalPolicy != "on-request" || d[2].TurnStart.CollaborationMode.Mode != "default" {
		t.Fatalf("turn 2 (agent) = %+v", d[2].TurnStart)
	}
	if d[2].TurnStart.CollaborationMode.Settings.Model != "gpt-6.2-sol" {
		t.Fatalf("turn 2 collaborationMode model = %q", d[2].TurnStart.CollaborationMode.Settings.Model)
	}
}

func TestCodexEditsDiffsAndMovePathStamp(t *testing.T) {
	ws := t.TempDir()
	cs, _ := startCodexFake(t, "codex-edits", Options{Force: true, Workspace: ws})
	evs := collectSubEvents(t, cs, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := cs.Prompt(ctx, "make edits"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})

	var okTool, failTool *ToolEvent
	for _, ev := range evs {
		if ev.Type == EventTool && ev.Tool != nil {
			switch ev.Tool.ID {
			case "fc-ok":
				cp := *ev.Tool
				okTool = &cp
			case "fc-fail":
				cp := *ev.Tool
				failTool = &cp
			}
		}
	}
	if okTool == nil || okTool.Edited == nil {
		t.Fatalf("fc-ok missing ToolEdited stamp: %+v", okTool)
	}
	for _, want := range []string{"added.txt", "updated.txt", "moved.txt"} {
		if !slices.Contains(okTool.Edited.Files, want) {
			t.Fatalf("fc-ok Edited.Files = %v, missing %q", okTool.Edited.Files, want)
		}
	}
	if len(okTool.Diffs) != 2 {
		t.Fatalf("fc-ok Diffs len = %d, want 2: %+v", len(okTool.Diffs), okTool.Diffs)
	}
	if okTool.Diffs[0].OldText != "" || okTool.Diffs[0].NewText != "hello\n" {
		t.Fatalf("add diff = %+v", okTool.Diffs[0])
	}
	if okTool.Diffs[1].OldText != "old\n" || okTool.Diffs[1].NewText != "new\n" || okTool.Diffs[1].Added != 1 || okTool.Diffs[1].Removed != 1 {
		t.Fatalf("update/move diff = %+v", okTool.Diffs[1])
	}
	if failTool == nil || failTool.Status != "cancelled" || failTool.Edited != nil {
		t.Fatalf("declined fc-fail must have status=cancelled and nil Edited: %+v", failTool)
	}
}

func TestCodexCancelSettlesInFlightToolAndWithdrawsPreWire(t *testing.T) {
	cs, _ := startCodexFake(t, "codex-hang", Options{Force: true})

	// 1. Pre-wire withdraw: Begin + Cancel before calling continuation.
	run := cs.Begin("withdraw me")
	out, err := cs.Cancel(context.Background())
	if err != nil || !out.Withdrew || !out.Settled {
		t.Fatalf("pre-wire Cancel = %+v, %v; want Withdrew=true Settled=true", out, err)
	}
	if _, err := run(context.Background()); !errors.Is(err, ErrPromptCancelled) {
		t.Fatalf("withdrawn Begin continuation = %v, want ErrPromptCancelled", err)
	}

	// 2. Mid-turn cancel settles in-flight commandExecution tool row to cancelled.
	sub, err := cs.Subscribe(SubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	toolRunning := make(chan struct{}, 1)
	toolSettled := make(chan string, 1)
	go func() {
		for rec := range sub.Records() {
			ev, decErr := DecodeEvent(rec.Body)
			if decErr != nil {
				continue
			}
			if ev.Type == EventTool && ev.Tool != nil && ev.Tool.ID == "cmd-hang" {
				if ev.Tool.Status == "in_progress" {
					select {
					case toolRunning <- struct{}{}:
					default:
					}
				} else {
					select {
					case toolSettled <- ev.Tool.Status:
					default:
					}
				}
			}
		}
	}()

	promptDone := make(chan Result, 1)
	promptErr := make(chan error, 1)
	go func() {
		r, e := cs.Prompt(context.Background(), "hang")
		promptDone <- r
		promptErr <- e
	}()

	select {
	case <-toolRunning:
	case <-time.After(5 * time.Second):
		t.Fatal("cmd-hang never started")
	}

	cOut, cErr := cs.Cancel(context.Background())
	if cErr != nil || !cOut.Wrote || !cOut.Settled {
		t.Fatalf("Cancel = %+v, %v; want Wrote=true Settled=true", cOut, cErr)
	}
	if err := <-promptErr; err != nil {
		t.Fatalf("Prompt after cancel err = %v", err)
	}
	if r := <-promptDone; r.StopReason != acp.StopCancelled {
		t.Fatalf("StopReason = %q, want cancelled", r.StopReason)
	}
	select {
	case st := <-toolSettled:
		if st != "cancelled" {
			t.Fatalf("cmd-hang settled status = %q, want cancelled", st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cmd-hang tool row was not settled on cancel")
	}
}

func TestCodexCompletedBeforeTurnStartReply(t *testing.T) {
	cs, _ := startCodexFake(t, "codex-completed-before-reply", Options{Force: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := cs.Prompt(ctx, "fast")
	if err != nil {
		t.Fatalf("Prompt(codex-completed-before-reply): %v", err)
	}
	if res.StopReason != acp.StopEndTurn {
		t.Fatalf("StopReason = %q, want end_turn", res.StopReason)
	}
}

func TestCodexFastFail401WillRetry(t *testing.T) {
	cs, _ := startCodexFake(t, "codex-error-401", Options{Force: true})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cs.Prompt(ctx, "hi")
	if err == nil {
		t.Fatal("expected error on 401 willRetry")
	}
	if ErrorInfo(err).Kind != errkind.Auth {
		t.Fatalf("ErrorInfo(%v).Kind = %v, want auth", err, ErrorInfo(err).Kind)
	}
}

func TestCodexAgentGroupFailureAndKeepLogOnFailedStart(t *testing.T) {
	bin := fakeAgentPath(t)
	prov := CodexProvider()

	t.Run("AgentGroup error kills child and fails Start", func(t *testing.T) {
		var sawPGID int
		cs := New(Options{
			Provider:  &prov,
			Binary:    bin,
			Workspace: t.TempDir(),
			Env:       append(os.Environ(), "CRAZE_FAKE_SCRIPT=codex-basic"),
			NoPrimary: true,
			AgentGroup: func(pgid int) error {
				sawPGID = pgid
				return errors.New("record failed")
			},
		}).(*codexSession)
		defer func() { _ = cs.Close() }()
		if err := cs.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "record failed") {
			t.Fatalf("Start = %v, want AgentGroup error", err)
		}
		if sawPGID <= 0 {
			t.Fatalf("AgentGroup never received valid pgid: %d", sawPGID)
		}
	})

	t.Run("KeepLogOnFailedStart keeps EventLog open until Close", func(t *testing.T) {
		cs := New(Options{
			Provider:             &prov,
			Binary:               bin,
			Workspace:            t.TempDir(),
			Env:                  append(os.Environ(), "CRAZE_FAKE_SCRIPT=codex-loggedout"),
			NoPrimary:            true,
			KeepLogOnFailedStart: true,
		}).(*codexSession)
		if err := cs.Start(context.Background()); err == nil {
			t.Fatal("expected loggedout failure")
		}
		// Log must still accept subscriptions until Close() is called.
		sub, err := cs.Subscribe(SubscribeOptions{})
		if err != nil {
			t.Fatalf("Subscribe after failed Start with KeepLogOnFailedStart: %v", err)
		}
		sub.Close()
		_ = cs.Close()
	})

	t.Run("StartErrAgentStderr decorates crash on start", func(t *testing.T) {
		cs := New(Options{
			Provider:            &prov,
			Binary:              bin,
			Workspace:           t.TempDir(),
			Env:                 append(os.Environ(), "CRAZE_FAKE_SCRIPT=codex-exit-two-lines"),
			NoPrimary:           true,
			StartErrAgentStderr: true,
			StartErrExitHint:    " (check login)",
		}).(*codexSession)
		defer func() { _ = cs.Close() }()
		err := cs.Start(context.Background())
		if err == nil {
			t.Fatal("expected crash-on-start error")
		}
		if !strings.Contains(err.Error(), "KEYCHAIN LOCKED") || !strings.Contains(err.Error(), "(check login)") {
			t.Fatalf("decorated start err = %q, want stderr tail and hint", err.Error())
		}
	})
}

func TestCodexSettingsSwitchEffortAndForModel(t *testing.T) {
	cs, _ := startCodexFake(t, "codex-basic", Options{Force: true})
	ctx := context.Background()

	// SetConfig with mismatched forModel must fail with ErrStaleModel.
	if _, err := cs.SetConfig(ctx, "", "effort", "high", "gpt-6-luna"); !errors.Is(err, ErrStaleModel) {
		t.Fatalf("SetConfig(wrong forModel) = %v, want ErrStaleModel", err)
	}

	// Set effort=high first: gpt-6.2-astra and gpt-6-luna both support high -> SwitchEffort=high.
	if _, err := cs.SetConfig(ctx, "c0", "effort", "high", "gpt-6.2-sol"); err != nil {
		t.Fatalf("SetConfig(effort=high): %v", err)
	}
	for _, m := range cs.Snapshot().Models {
		if m.ID == "gpt-6.2-astra" && m.SwitchEffort != "high" {
			t.Fatalf("gpt-6.2-astra SwitchEffort at high = %q, want high", m.SwitchEffort)
		}
	}

	revBefore := cs.Snapshot().CatalogRevision
	out, err := cs.SetConfig(ctx, "c1", "effort", "ultra", "gpt-6.2-sol")
	if err != nil || out.Value != "ultra" {
		t.Fatalf("SetConfig(effort=ultra) = %+v, %v", out, err)
	}
	snap := cs.Snapshot()
	if snap.CatalogRevision <= revBefore {
		t.Fatalf("CatalogRevision did not bump on effort change: %d -> %d", revBefore, snap.CatalogRevision)
	}
	// Neither gpt-6.2-astra (default low) nor gpt-6-luna (default medium) supports ultra -> SwitchEffort falls back to each model's DefaultEffort.
	for _, m := range snap.Models {
		switch m.ID {
		case "gpt-6.2-astra":
			if m.SwitchEffort != "low" {
				t.Fatalf("gpt-6.2-astra SwitchEffort = %q, want low (default)", m.SwitchEffort)
			}
		case "gpt-6-luna":
			if m.SwitchEffort != "medium" {
				t.Fatalf("gpt-6-luna SwitchEffort = %q, want medium (default)", m.SwitchEffort)
			}
		}
	}

	// Switching to gpt-6-luna falls back effort from unsupported ultra -> medium.
	mOut, err := cs.SetModel(ctx, "c2", "gpt-6-luna")
	if err != nil || mOut.Value != "gpt-6-luna" {
		t.Fatalf("SetModel(gpt-6-luna) = %+v, %v", mOut, err)
	}
	if opt := EffortOption(cs.Snapshot()); opt == nil || opt.Current != "medium" {
		t.Fatalf("effort after switch to luna = %+v, want medium", opt)
	}
}

func TestCodexPromptWithAttachmentEnvelopeSendsLocalImage(t *testing.T) {
	imageHome(t)
	ref, _ := savedImage(t, 1, 16, 16)
	dumpPath := filepath.Join(t.TempDir(), "prompts.jsonl")
	cs, _ := startCodexFake(t, "codex-basic", Options{Force: true}, "CRAZE_FAKE_DUMP_PROMPTS="+dumpPath)

	prompt := AttachmentBlock([]AttachmentRef{ref}) + "inspect " + ImageLabel(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cs.Prompt(ctx, prompt); err != nil {
		t.Fatalf("Prompt with image: %v", err)
	}

	b, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read prompt dump: %v", err)
	}
	var dumped struct {
		TurnStart struct {
			Input []codexrpc.UserInput `json:"input"`
		} `json:"turnStart"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &dumped); err != nil {
		t.Fatalf("unmarshal prompt dump: %v", err)
	}
	foundText, foundImg := false, false
	for _, in := range dumped.TurnStart.Input {
		if in.Type == "text" && strings.Contains(in.Text, "inspect [Image #1]") && !strings.Contains(in.Text, attachmentsOpen) {
			foundText = true
		}
		if in.Type == "localImage" && in.Path == ref.Path {
			foundImg = true
		}
	}
	if !foundText || !foundImg {
		t.Fatalf("turn/start inputs = %+v, want clean text + localImage(%q)", dumped.TurnStart.Input, ref.Path)
	}
}

func TestCodexUsageSparseMergeAndWindowPick(t *testing.T) {
	var u codexUsage
	r1 := int64(1796000000)
	u.mergeRateLimits(&codexrpc.RateLimitsSnapshot{
		PlanType:  "prolite",
		Primary:   &codexrpc.RateLimitWindow{UsedPercent: 25, WindowDurationMins: 300, ResetsAt: &r1},
		Secondary: &codexrpc.RateLimitWindow{UsedPercent: 60, WindowDurationMins: 10080, ResetsAt: &r1},
	})
	// Sparse update touching only Primary (Secondary is nil — must NOT wipe Secondary!).
	u.mergeRateLimits(&codexrpc.RateLimitsSnapshot{
		Primary: &codexrpc.RateLimitWindow{UsedPercent: 80, WindowDurationMins: 300, ResetsAt: &r1},
	})
	st := u.state()
	if st.Window != "five_hour" || st.WindowUtilization != 0.80 || st.Plan != "prolite" {
		t.Fatalf("UsageState after sparse merge = %+v, want five_hour @ 0.80", st)
	}
	if u.rl.Secondary == nil || u.rl.Secondary.UsedPercent != 60 {
		t.Fatalf("sparse update clobbered Secondary: %+v", u.rl.Secondary)
	}
}

func TestCodexCommandOutputStderrHeadOnNonZeroExit(t *testing.T) {
	code := 2
	out := codexCommandOutput("fatal: bad flag\nmore details\n", &code)
	if out == nil || out.ExitCode == nil || *out.ExitCode != 2 {
		t.Fatalf("out = %+v", out)
	}
	if out.StdoutHead == "" || out.StderrHead == "" {
		t.Fatalf("non-zero exit must populate both StdoutHead and StderrHead: %+v", out)
	}
}
