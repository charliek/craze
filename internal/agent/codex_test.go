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
	// Turn 0 (ask, --no-force): readOnly + never + collaborationMode default + explicit developer_instructions: null.
	if d[0].TurnStart.SandboxPolicy.Type != "readOnly" || d[0].TurnStart.ApprovalPolicy != "never" || d[0].TurnStart.CollaborationMode.Mode != "default" {
		t.Fatalf("turn 0 (ask) = %+v", d[0].TurnStart)
	}
	for i, rawLine := range lines {
		if !strings.Contains(rawLine, `"developer_instructions":null`) {
			t.Fatalf("turn %d missing explicit raw developer_instructions:null key: %s", i, rawLine)
		}
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
	// Verify immutable copy was sent (NOT the original attachment path, M8 TOCTOU guard).
	var sentImgPath string
	foundText, foundImg := false, false
	for _, in := range dumped.TurnStart.Input {
		if in.Type == "text" && strings.Contains(in.Text, "inspect [Image #1]") && !strings.Contains(in.Text, attachmentsOpen) {
			foundText = true
		}
		if in.Type == "localImage" && in.Path != "" {
			foundImg = true
			sentImgPath = in.Path
		}
	}
	if !foundText || !foundImg {
		t.Fatalf("turn/start inputs = %+v, want clean text + localImage", dumped.TurnStart.Input)
	}
	if sentImgPath == ref.Path {
		t.Fatalf("turn/start sent original attachment path %q instead of host-owned immutable copy", sentImgPath)
	}
	// Temp copy must be cleaned up once the turn settles.
	if _, err := os.Stat(sentImgPath); !os.IsNotExist(err) {
		t.Fatalf("per-turn image copy %q still exists after settlement (err=%v)", sentImgPath, err)
	}
}

func f64Ptr(v float64) *float64 { return &v }
func intPtr(v int) *int         { return &v }

func TestCodexUsageSparseMergeAndWindowPick(t *testing.T) {
	var u codexUsage
	r1 := int64(1796000000)
	evalTime := time.Unix(r1-3600, 0).UTC()
	u.mergeRateLimits(&codexrpc.RateLimitsSnapshot{
		LimitID:             "codex",
		PlanType:            "prolite",
		Primary:             &codexrpc.RateLimitWindow{UsedPercent: f64Ptr(25), WindowDurationMins: intPtr(300), ResetsAt: &r1},
		Secondary:           &codexrpc.RateLimitWindow{UsedPercent: f64Ptr(60), WindowDurationMins: intPtr(10080), ResetsAt: &r1},
		SpendControlReached: boolPtr(false),
	})
	// Sparse update touching only Primary.UsedPercent (WindowDurationMins & ResetsAt nil -> MUST preserve!).
	u.mergeRateLimits(&codexrpc.RateLimitsSnapshot{
		LimitID:             "codex",
		Primary:             &codexrpc.RateLimitWindow{UsedPercent: f64Ptr(80)},
		SpendControlReached: boolPtr(true),
	})
	st := u.stateAt(evalTime)
	if st.Window != "five_hour" || st.WindowUtilization != 0.80 || st.Plan != "prolite" || st.ResetsAt.Unix() != r1 {
		t.Fatalf("UsageState after sparse merge = %+v, want five_hour @ 0.80 with preserved resetsAt=%d", st, r1)
	}
	bucket := u.buckets["codex"]
	if bucket == nil || bucket.Secondary == nil || bucket.Secondary.UsedPercent == nil || *bucket.Secondary.UsedPercent != 60 {
		t.Fatalf("sparse update clobbered Secondary: %+v", bucket)
	}
	if bucket.SpendControlReached == nil || !*bucket.SpendControlReached {
		t.Fatalf("SpendControlReached was not merged: %+v", bucket.SpendControlReached)
	}
	// A push for a DIFFERENT limitId must not inherit Secondary from "codex".
	u.mergeRateLimits(&codexrpc.RateLimitsSnapshot{
		LimitID: "codex_other",
		Primary: &codexrpc.RateLimitWindow{UsedPercent: f64Ptr(95), WindowDurationMins: intPtr(300)},
	})
	if other := u.buckets["codex_other"]; other == nil || other.Secondary != nil {
		t.Fatalf("second bucket inherited foreign Secondary: %+v", other)
	}
	if got := u.stateAt(evalTime); got.WindowUtilization != 0.95 {
		t.Fatalf("expected highest window across buckets (0.95), got %+v", got)
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

// --- M2a Review Regression Tests (H1-H5, M6-M14, L1-L4) ---

func TestCodexApprovalDeclinedAfterMidTurnModeChange(t *testing.T) {
	// H1: Start a turn in read-only ask mode (with Force: true), then switch
	// next-turn mode to agent while the ask turn is running. An approval
	// arriving for that running ask turn MUST still be declined because
	// decisions bind to the immutable mode captured when the turn started.
	cs, _ := startCodexFake(t, "codex-approval", Options{Mode: "ask", Force: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start the ask turn and switch mode to agent as soon as the turn begins.
	var text strings.Builder
	evs := collectSubEvents(t, cs, func() {
		go func() {
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				cs.mu.Lock()
				inFlight := cs.activeTurn != nil && (cs.activeTurn.phase == codexPhaseSent || cs.activeTurn.phase == codexPhaseIdentified)
				cs.mu.Unlock()
				if inFlight {
					_, _ = cs.SetMode(ctx, "", "agent")
					return
				}
				time.Sleep(2 * time.Millisecond)
			}
		}()
		if _, err := cs.Prompt(ctx, "try command in ask"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})
	for _, ev := range evs {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
	}
	if !strings.Contains(text.String(), "decision=decline") {
		t.Fatalf("expected ask-turn approval to decline even after mid-turn SetMode(agent), got %q", text.String())
	}
}

func TestCodexPermissionsDenialHasTypedWireShape(t *testing.T) {
	// M7: item/permissions/requestApproval must reply {"permissions":{},"scope":"turn"}.
	cs, _ := startCodexFake(t, "codex-permissions-req", Options{Force: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var text strings.Builder
	evs := collectSubEvents(t, cs, func() {
		if _, err := cs.Prompt(ctx, "request perms"); err != nil {
			t.Fatalf("Prompt: %v", err)
		}
	})
	for _, ev := range evs {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
	}
	got := text.String()
	if !strings.Contains(got, `"permissions":{}`) || !strings.Contains(got, `"scope":"turn"`) {
		t.Fatalf("permissions denial wire reply = %q, want typed permissions:{} and scope:turn", got)
	}
}

func TestCodexCompleteThenExitPreservesSuccessOverEOF(t *testing.T) {
	// H4: Fake emits final edit, text, and turn/completed, then exits immediately.
	// Ordered notification drain must let completion win over stdout EOF.
	cs, _ := startCodexFake(t, "codex-complete-then-exit", Options{Force: true})
	var (
		res Result
		err error
	)
	evs := collectSubEvents(t, cs, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err = cs.Prompt(ctx, "finish and exit")
	})
	if err != nil || res.StopReason != acp.StopEndTurn {
		t.Fatalf("Prompt = (%+v, %v), want StopEndTurn nil error", res, err)
	}
	var text strings.Builder
	sawEdit := false
	for _, ev := range evs {
		if ev.Type == EventText {
			text.WriteString(ev.Text)
		}
		if ev.Type == EventTool && ev.Tool != nil && ev.Tool.ID == "fc-eof" && ev.Tool.Edited != nil {
			sawEdit = true
		}
	}
	if !strings.Contains(text.String(), "finished right before exit") || !sawEdit {
		t.Fatalf("expected final text & edit stamp before EOF; text=%q sawEdit=%v", text.String(), sawEdit)
	}
}

func TestCodexUnexpectedExitZeroAndNonZeroClassifyAsAgentExited(t *testing.T) {
	// M12: Both unexpected exit 0 and unexpected non-zero exit mid-turn must
	// surface ErrAgentExited and round-trip as agent_exited through eventcodec.
	for _, tc := range []struct {
		name     string
		script   string
		wantCode int
	}{
		{"exit 0 mid-turn", "codex-exit-zero-midturn", 0},
		{"exit 7 mid-turn", "codex-exit-nonzero-midturn", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, _ := startCodexFake(t, tc.script, Options{Force: true})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := cs.Prompt(ctx, "crash now")
			if err == nil {
				t.Fatal("expected error on unexpected child exit")
			}
			if !errors.Is(err, ErrAgentExited) {
				t.Fatalf("err = %v, want errors.Is(err, ErrAgentExited)", err)
			}
			if got := ErrorInfo(err).Kind; got != errkind.Agent {
				t.Fatalf("ErrorInfo(err).Kind = %v, want agent", got)
			}
			body, encErr := EncodeEvent(Event{Type: EventError, Err: err})
			if encErr != nil {
				t.Fatalf("EncodeEvent: %v", encErr)
			}
			dec, decErr := DecodeEvent(body)
			if decErr != nil {
				t.Fatalf("DecodeEvent: %v", decErr)
			}
			var rem *RemoteError
			if !errors.As(dec.Err, &rem) || rem.Class != EventErrAgentExited || rem.Code != tc.wantCode {
				t.Fatalf("decoded err = %#v, want RemoteError{Class: agent_exited, Code: %d}", dec.Err, tc.wantCode)
			}
			if !errors.Is(dec.Err, ErrAgentExited) {
				t.Fatalf("decoded err = %v, want errors.Is(ErrAgentExited)", dec.Err)
			}
		})
	}
}

func TestCodexFastFailUsageLimitAndContextWindowWithWillRetry(t *testing.T) {
	// M13: usageLimitExceeded and contextWindowExceeded must fast-fail even when willRetry: true.
	for _, tc := range []struct {
		name   string
		script string
		want   errkind.Kind
	}{
		{"usageLimitExceeded", "codex-retry-usagelimit", errkind.Quota},
		{"contextWindowExceeded", "codex-retry-context", errkind.ContextTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, _ := startCodexFake(t, tc.script, Options{Force: true})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_, err := cs.Prompt(ctx, "trigger retryable terminal error")
			if err == nil {
				t.Fatal("expected fast-fail error")
			}
			if got := ErrorInfo(err).Kind; got != tc.want {
				t.Fatalf("ErrorInfo(%v).Kind = %v, want %v", err, got, tc.want)
			}
		})
	}
}

func TestCodexSlowRateLimitsDoesNotBlockStartup(t *testing.T) {
	// H5: An unresponsive account/rateLimits/read uses its own shorter deadline
	// and does not hang startup.
	prev := codexRateLimitStartTimeout
	codexRateLimitStartTimeout = 120 * time.Millisecond
	t.Cleanup(func() { codexRateLimitStartTimeout = prev })

	start := time.Now()
	cs, _ := startCodexFake(t, "codex-slow-ratelimits", Options{Force: true})
	if time.Since(start) > 3*time.Second {
		t.Fatalf("Start blocked %v on slow rateLimits/read", time.Since(start))
	}
	if cs.Snapshot().SessionID == "" {
		t.Fatal("expected session ready after rateLimits timeout")
	}
}

func TestCodexModelListMultipageAndRepeatedCursor(t *testing.T) {
	// L2: Multi-page model catalog succeeds; repeated cursor fails instead of silently truncating.
	t.Run("multipage", func(t *testing.T) {
		cs, _ := startCodexFake(t, "codex-model-multipage", Options{Force: true})
		var ids []string
		for _, m := range cs.Snapshot().Models {
			ids = append(ids, m.ID)
		}
		if !slices.Contains(ids, "gpt-6.2-sol") || !slices.Contains(ids, "gpt-6.2-astra") {
			t.Fatalf("multipage models = %v, want both pages", ids)
		}
	})
	t.Run("repeated cursor errors", func(t *testing.T) {
		bin := fakeAgentPath(t)
		prov := CodexProvider()
		cs := New(Options{
			Provider:  &prov,
			Binary:    bin,
			Workspace: t.TempDir(),
			Env:       append(os.Environ(), "CRAZE_FAKE_SCRIPT=codex-model-loop-cursor"),
		}).(*codexSession)
		defer func() { _ = cs.Close() }()
		err := cs.Start(context.Background())
		if err == nil || !strings.Contains(err.Error(), "repeated pagination cursor") {
			t.Fatalf("Start(loop cursor) = %v, want repeated pagination cursor error", err)
		}
	})
}

func TestCodexCancelHungInterruptTimesOutWithError(t *testing.T) {
	// H2: Missing turn/interrupt reply + missing turn/completed must NOT hang
	// forever or pretend StopCancelled succeeded — it aborts the hung transport
	// and returns an error.
	prev := codexCancelFallbackGrace
	codexCancelFallbackGrace = 150 * time.Millisecond
	t.Cleanup(func() { codexCancelFallbackGrace = prev })

	cs, _ := startCodexFake(t, "codex-hang-interrupt", Options{Force: true})
	promptDone := make(chan error, 1)
	go func() {
		_, err := cs.Prompt(context.Background(), "hang forever")
		promptDone <- err
	}()
	// Wait until the turn is identified.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cs.mu.Lock()
		identified := cs.activeTurn != nil && cs.activeTurn.phase == codexPhaseIdentified
		cs.mu.Unlock()
		if identified {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	out, cancelErr := cs.Cancel(context.Background())
	if cancelErr == nil || out.Settled {
		t.Fatalf("Cancel on hung interrupt = (%+v, %v), want error and Settled=false", out, cancelErr)
	}
	select {
	case pErr := <-promptDone:
		if pErr == nil {
			t.Fatal("Prompt returned nil on hung cancel, want error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Prompt did not unblock after hung cancel grace timeout")
	}
}

func TestCodexWritableResumeEnforcesReadOnlyInAskAndPlan(t *testing.T) {
	// Resuming a writable thread in --ask or --plan must still send readOnly + never.
	for _, mode := range []string{"ask", "plan"} {
		t.Run(mode, func(t *testing.T) {
			dumpPath := filepath.Join(t.TempDir(), "prompts.jsonl")
			cs, _ := startCodexFake(t, "codex-basic", Options{
				LoadSessionID: "thr-writable-1",
				Mode:          mode,
				Force:         false,
			}, "CRAZE_FAKE_DUMP_PROMPTS="+dumpPath)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := cs.Prompt(ctx, "check read only"); err != nil {
				t.Fatalf("Prompt(%s): %v", mode, err)
			}
			b, err := os.ReadFile(dumpPath)
			if err != nil {
				t.Fatalf("read prompt dump: %v", err)
			}
			line := strings.TrimSpace(string(b))
			if !strings.Contains(line, `"type":"readOnly"`) || !strings.Contains(line, `"approvalPolicy":"never"`) {
				t.Fatalf("resumed %s turnStart = %s, want readOnly + never", mode, line)
			}
		})
	}
}

func TestCodexUnifiedDiffDeleteMultiHunkAndNewlineOnly(t *testing.T) {
	// L4: Verify delete, multi-hunk count preservation, and newline-only edits.
	del := codexFileChangeToToolDiff(codexrpc.FileUpdateChange{
		Path: "gone.txt",
		Kind: codexrpc.FileChangeKind{Type: "delete"},
		Diff: "line1\nline2\n",
	})
	if del.OldText != "line1\nline2\n" || del.NewText != "" || del.Removed != 2 {
		t.Fatalf("delete diff = %+v", del)
	}

	multi := codexFileChangeToToolDiff(codexrpc.FileUpdateChange{
		Path: "multi.txt",
		Kind: codexrpc.FileChangeKind{Type: "update"},
		Diff: "@@ -1,2 +1,2 @@\n-a\n+A\n ctx1\n@@ -10,2 +10,3 @@\n ctx2\n+b1\n+b2\n-c\n",
	})
	if multi.Added != 3 || multi.Removed != 2 {
		t.Fatalf("multi-hunk counts = +%d/-%d, want +3/-2 (%+v)", multi.Added, multi.Removed, multi)
	}

	nlOnly := codexFileChangeToToolDiff(codexrpc.FileUpdateChange{
		Path: "nl.txt",
		Kind: codexrpc.FileChangeKind{Type: "update"},
		Diff: "@@ -1,1 +1,1 @@\n-hello\n\\ No newline at end of file\n+hello\n",
	})
	if nlOnly.OldText != "hello" || nlOnly.NewText != "hello\n" || nlOnly.OldText == nlOnly.NewText {
		t.Fatalf("newline-only diff lost edit: old=%q new=%q", nlOnly.OldText, nlOnly.NewText)
	}
}

func TestCodexSubscriptionGuard(t *testing.T) {
	for _, tc := range []struct {
		script string
		kind   errkind.Kind
	}{{"codex-apikey", errkind.Auth}, {"codex-loggedout", errkind.Auth}, {"codex-custom-provider", errkind.Auth}, {"codex-paid-endpoint", errkind.Auth}} {
		t.Run(tc.script, func(t *testing.T) {
			prov := CodexProvider()
			s := New(Options{Provider: &prov, Binary: fakeAgentPath(t), Workspace: t.TempDir(), Env: append(os.Environ(), "CRAZE_FAKE_SCRIPT="+tc.script), NoPrimary: true, SubscriptionOnly: true})
			defer s.Close()
			if err := s.Start(t.Context()); err == nil || ErrorInfo(err).Kind != tc.kind {
				t.Fatalf("start = %v, want %s", err, tc.kind)
			}
		})
	}
	s, _ := startCodexFake(t, "codex-basic", Options{Force: true, SubscriptionOnly: true})
	if s.Snapshot().SessionID == "" {
		t.Fatal("no subscription thread")
	}
}
