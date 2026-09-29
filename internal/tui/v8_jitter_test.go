package tui

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/backend"
	"github.com/charliek/craze/internal/engine"
)

// V8, the jitter run (plan 027 §8, §3.12 (d)): the queue, interject, cancel,
// ask and native-subagent goldens — and H6 PR 3's wake golden — run with the
// asynchronous gate over a backend whose every answer and every read is
// delayed by a seeded random duration, once per seed, each against its golden
// file. Any diff is diagnosed, never retried: the seed is in the subtest's
// name.
//
// It is skipped unless CRAZE_V8_SEEDS names how many seeds to run, so the
// per-commit gate stays fast:
//
//	CRAZE_V8_SEEDS=20 go test ./internal/tui -run TestV8JitteredGoldens -timeout 60m

// v8JitterMax is the longest a jittered call's answer or read is delayed:
// long against the in-process backend's microseconds, so replies and events
// land in every relative order the gate must survive, and short against the
// goldens' per-wait timeout (15 s, the slowest native one 20 s): a golden reads
// a few hundred events, so even the worst seed adds a few seconds.
const v8JitterMax = 10 * time.Millisecond

// frameModesJittered makes runFrameModes run the asynchronous mode alone:
// V8's own switch, set only while its test runs.
var frameModesJittered bool

// jitter is one run's seeded delays, shared by every call the backend makes:
// which call draws which delay is the scheduler's, so a seed fixes the delays
// drawn, not their order.
type jitter struct {
	mu  sync.Mutex
	rng *rand.Rand
}

func (j *jitter) pause() {
	j.mu.Lock()
	d := time.Duration(j.rng.Int64N(int64(v8JitterMax) + 1))
	j.mu.Unlock()
	time.Sleep(d)
}

// jitterBackend is the in-process backend with each call's return and each
// Read delayed (jitter). Everything else is the backend's own; engine() names
// the engine it wraps, so the frame runner's capture boundary (streamHead) and
// the tests' engineOf still reach it.
type jitterBackend struct {
	backend.Backend
	j *jitter
}

func (b *jitterBackend) engine() *engine.Engine { return engineBehind(b.Backend) }

// wrapped is the backend the jitter wraps: a socket run's session, for the
// registry (hostEngineOf).
func (b *jitterBackend) wrapped() backend.Backend { return b.Backend }

func (b *jitterBackend) Start(ctx context.Context) error {
	defer b.j.pause()
	return b.Backend.Start(ctx)
}

func (b *jitterBackend) Read(ctx context.Context) (backend.Item, error) {
	defer b.j.pause()
	return b.Backend.Read(ctx)
}

func (b *jitterBackend) Submit(ctx context.Context, c engine.Command, text string, mode engine.SubmitMode, fromRow string) (engine.SubmitResult, error) {
	defer b.j.pause()
	return b.Backend.Submit(ctx, c, text, mode, fromRow)
}

func (b *jitterBackend) Answer(ctx context.Context, c engine.Command, id string, a agent.AskAnswer) error {
	defer b.j.pause()
	return b.Backend.Answer(ctx, c, id, a)
}

func (b *jitterBackend) Unqueue(ctx context.Context, c engine.Command, id string) (agent.QueuedPrompt, error) {
	defer b.j.pause()
	return b.Backend.Unqueue(ctx, c, id)
}

func (b *jitterBackend) EditQueued(ctx context.Context, c engine.Command, id, text string, v *int) error {
	defer b.j.pause()
	return b.Backend.EditQueued(ctx, c, id, text, v)
}

func (b *jitterBackend) ClearQueue(ctx context.Context, c engine.Command) ([]agent.QueuedPrompt, error) {
	defer b.j.pause()
	return b.Backend.ClearQueue(ctx, c)
}

func (b *jitterBackend) Disarm(ctx context.Context, c engine.Command) error {
	defer b.j.pause()
	return b.Backend.Disarm(ctx, c)
}

func (b *jitterBackend) Interject(ctx context.Context, c engine.Command, text string) error {
	defer b.j.pause()
	return b.Backend.Interject(ctx, c, text)
}

func (b *jitterBackend) SetTitle(ctx context.Context, c engine.Command, title string) error {
	defer b.j.pause()
	return b.Backend.SetTitle(ctx, c, title)
}

func (b *jitterBackend) Set(ctx context.Context, c engine.Command, s engine.Setting) (engine.SetResult, error) {
	defer b.j.pause()
	return b.Backend.Set(ctx, c, s)
}

func (b *jitterBackend) Cancel(ctx context.Context, c engine.Command, turn string) (engine.CancelResult, error) {
	defer b.j.pause()
	return b.Backend.Cancel(ctx, c, turn)
}

func (b *jitterBackend) CancelSubagent(ctx context.Context, c engine.Command, id string) error {
	defer b.j.pause()
	return b.Backend.CancelSubagent(ctx, c, id)
}

func (b *jitterBackend) Stop(ctx context.Context, c engine.Command) error {
	defer b.j.pause()
	return b.Backend.Stop(ctx, c)
}

func (b *jitterBackend) Ask(ctx context.Context, id string) (agent.AskRecord, bool, error) {
	defer b.j.pause()
	return b.Backend.Ask(ctx, id)
}

func (b *jitterBackend) Settings(ctx context.Context) (backend.Settings, error) {
	defer b.j.pause()
	return b.Backend.Settings(ctx)
}

// jittered installs the jitter for the rest of t: every backend setSession
// builds is wrapped, seeded by seed and the golden's index, and every frame
// golden runs asynchronously alone.
func jittered(t *testing.T, seed uint64, golden int) {
	t.Helper()
	j := &jitter{rng: rand.New(rand.NewPCG(seed, uint64(golden)))}
	prevHook, prevModes := sessionBackendHook, frameModesJittered
	sessionBackendHook = func(b backend.Backend) backend.Backend { return &jitterBackend{Backend: b, j: j} }
	frameModesJittered = true
	t.Cleanup(func() { sessionBackendHook, frameModesJittered = prevHook, prevModes })
}

// v8Goldens is V8's set, by the plan's groups.
var v8Goldens = []struct {
	name string
	run  func(*testing.T)
}{
	// The queue.
	{"TestFrameGoldenQueueRows", TestFrameGoldenQueueRows},
	{"TestFrameGoldenQueueHover100x30", TestFrameGoldenQueueHover100x30},
	{"TestFrameGoldenQueueSelected80x24", TestFrameGoldenQueueSelected80x24},
	{"TestFrameGoldenQueueEdit100x30", TestFrameGoldenQueueEdit100x30},
	{"TestFrameGoldenQueueSendNowConfirm100x30", TestFrameGoldenQueueSendNowConfirm100x30},
	{"TestFrameGoldenQueueDrain80x24", TestFrameGoldenQueueDrain80x24},
	{"TestFrameGoldenQueueDegrade40x12", TestFrameGoldenQueueDegrade40x12},
	// The interjection.
	{"TestFrameGoldenGrokInterject100x30", TestFrameGoldenGrokInterject100x30},
	// The cancels and Esc.
	{"TestFrameGoldenGrokSubagentCancel100x30", TestFrameGoldenGrokSubagentCancel100x30},
	{"TestFramePlanEscCancels", TestFramePlanEscCancels},
	{"TestFrameGoldenNativeSubagentStopRows80x24", TestFrameGoldenNativeSubagentStopRows80x24},
	{"TestFrameGoldenNativeSubagentStopView100x30", TestFrameGoldenNativeSubagentStopView100x30},
	{"TestFrameEscStopsARunningWake", TestFrameEscStopsARunningWake},
	{"TestFrameEscWhileIdleLeavesABackgroundChildRunning", TestFrameEscWhileIdleLeavesABackgroundChildRunning},
	// The asks: the cards and their answers.
	{"TestFrameGoldenAsk100x30", TestFrameGoldenAsk100x30},
	{"TestFrameAskAnswersBothQuestions", TestFrameAskAnswersBothQuestions},
	{"TestFrameAskEscSkips", TestFrameAskEscSkips},
	{"TestFrameGoldenPlan100x30", TestFrameGoldenPlan100x30},
	{"TestFramePlanDecisions", TestFramePlanDecisions},
	{"TestFrameGoldenPermissionNoForce100x30", TestFrameGoldenPermissionNoForce100x30},
	{"TestFramePermissionAlwaysPicksTheRequestsOwnID", TestFramePermissionAlwaysPicksTheRequestsOwnID},
	{"TestFrameGoldenGrokAsk", TestFrameGoldenGrokAsk},
	{"TestFrameGoldenGrokPlan", TestFrameGoldenGrokPlan},
	{"TestFrameGoldenPlanMode", TestFrameGoldenPlanMode},
	{"TestFrameGoldenNativeQuestion", TestFrameGoldenNativeQuestion},
	{"TestFrameGoldenNativePlanOffer", TestFrameGoldenNativePlanOffer},
	{"TestFrameGoldenNativePlanDenied", TestFrameGoldenNativePlanDenied},
	// The native sub-agents, and H6 PR 3's wake.
	{"TestFrameGoldenNativeSubagentRows80x24", TestFrameGoldenNativeSubagentRows80x24},
	{"TestFrameGoldenNativeSubagentView100x30", TestFrameGoldenNativeSubagentView100x30},
	{"TestFrameGoldenNativeSubagentTwoView100x30", TestFrameGoldenNativeSubagentTwoView100x30},
	{"TestFrameGoldenNativeSubagentFail80x24", TestFrameGoldenNativeSubagentFail80x24},
	{"TestFrameGoldenNativeSubagentBackgroundRows80x24", TestFrameGoldenNativeSubagentBackgroundRows80x24},
	{"TestFrameGoldenNativeSubagentBackgroundWake100x30", TestFrameGoldenNativeSubagentBackgroundWake100x30},
}

// TestV8JitteredGoldens is V8: every golden in v8Goldens, asynchronously, over
// the jitter backend, once per seed, each against its own golden file and
// assertions — the tests themselves, run unchanged.
func TestV8JitteredGoldens(t *testing.T) {
	raw := os.Getenv("CRAZE_V8_SEEDS")
	if raw == "" {
		t.Skip("V8 runs only when asked: CRAZE_V8_SEEDS=<seeds>")
	}
	seeds, err := strconv.Atoi(raw)
	if err != nil || seeds <= 0 {
		t.Fatalf("CRAZE_V8_SEEDS=%q: want a positive number of seeds", raw)
	}
	for seed := 1; seed <= seeds; seed++ {
		for i, g := range v8Goldens {
			t.Run(fmt.Sprintf("seed-%d/%s", seed, g.name), func(t *testing.T) {
				jittered(t, uint64(seed), i)
				g.run(t)
			})
		}
	}
}

// TestTheJitterBackendWrapsEverySession: V8 is real only if the jitter reaches
// the backend a frame drives — through setSession's test hook, and nowhere
// else — and the capture's boundary still reads the engine behind it.
func TestTheJitterBackendWrapsEverySession(t *testing.T) {
	jittered(t, 1, 0)
	isolateSkillsHome(t)
	stub := NewStub()
	t.Cleanup(func() { _ = stub.Close() })
	m := New(Config{Session: stub, Theme: "tokyo-night", Workspace: t.TempDir(), Yolo: true})
	jb, ok := m.eng.(*jitterBackend)
	if !ok {
		t.Fatalf("the session's backend is %T, want the jitter wrapper", m.eng)
	}
	if jb.engine() == nil || engineOf(t, m) != jb.engine() {
		t.Fatal("the wrapper does not name the engine it wraps")
	}
	if m.owner.current() != m.eng {
		t.Fatal("the owner holds another backend than the model's")
	}
	owner := m.owner
	if _, err := streamHead(owner, time.Second); err != nil {
		t.Fatalf("the capture boundary through the wrapper: %v", err)
	}
}
