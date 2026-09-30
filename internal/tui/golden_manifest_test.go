package tui

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The golden coverage manifest (plan 027 §3.16, A8; astra 31): every golden
// file in testdata/ with the transports its frame runs under. A8's claim —
// the full TUI runs unchanged over the socket, goldens included — is this
// list: every golden runs in process AND over the socket, byte-identical
// against the one file, except the six pre-start picker frames, which are
// direct-Update frames of the provider and resume pickers — dialogs that exist
// only in the host TUI and never in `craze attach` (Config.Backend has no
// picker) — and run in process alone; and the one frame of a restore whose
// last turn failed (plan 030 §3.7, restore-failed), whose model is a socket
// client of a host its test built — its session's turn failed before the
// attach, which the frame harness's socket host cannot make, since it
// attaches its model before its engine starts — and which the runner makes as
// a direct production, "in process" as the accounting reads it; and the
// session list's frames (plan 030 §3.10, §3.17, sessions-*), direct-Update
// frames of the client's own screen against a fake Config.Sessions — the
// list is drawn from the roster, never from the session's stream, so there
// is no transport to vary. A session opened from the list in place
// (sessions-opened-*, §3.11) runs under both: its script starts on either
// transport, and the session it opens is the fake list's own.
//
// It is held four ways:
//
//   - TestTheGoldenManifestIsEveryGolden fails on a golden file the manifest
//     does not list and on a listed one that does not exist;
//   - assertGolden fails when the invocation that produced the very frame it
//     is handed is not the manifest's set for that golden
//     (checkGoldenTransports): every RunFrameScript is logged in order
//     (frameProductions), and the frame is judged by the most recent
//     production of its exact bytes — a matrix run's (runFrameModes) carries
//     a credit, the transports that produced it, which the assertion of that
//     frame spends; a direct RunFrameScript's, a picker's direct-Update frame
//     (no production) and a second assertion of one frame are in process
//     alone as produced, and fail a golden the manifest runs under both;
//   - and a whole unnarrowed run of the package (goldenCoverage, from
//     TestMain) fails unless every golden in the manifest was asserted under
//     its whole set, and every test that asserted a golden did in each of the
//     run's -count iterations — so a golden whose socket run was skipped, or
//     whose test was, in any iteration, is caught even though no assertion of
//     it ran.

// goldenRuns is a manifest entry: the transports a golden's frame runs under.
type goldenRuns []frameTransport

var (
	inprocOnly     = goldenRuns{transportInproc}
	bothTransports = goldenRuns{transportInproc, transportSocket}
)

// goldenManifest is every golden file, by name, with its transports.
var goldenManifest = map[string]goldenRuns{
	"ask-100x30":                             bothTransports,
	"bash-80x24":                             bothTransports,
	"composer-five-lines-80x24":              bothTransports,
	"composer-full-line-100x30":              bothTransports,
	"composer-long-line-100x30":              bothTransports,
	"composer-nine-lines-100x30":             bothTransports,
	"composer-nine-lines-top-100x30":         bothTransports,
	"composer-one-line-100x30":               bothTransports,
	"composer-paste-100x30":                  bothTransports,
	"composer-six-lines-100x30":              bothTransports,
	"composer-two-lines-100x30":              bothTransports,
	"dblclick-word-100x30":                   bothTransports,
	"diff-80x24":                             bothTransports,
	"diff-expanded-120x40":                   bothTransports,
	"echo-80x24":                             bothTransports,
	"grok-ask-100x30":                        bothTransports,
	"grok-ask-80x24":                         bothTransports,
	"grok-echo-100x30":                       bothTransports,
	"grok-echo-80x24":                        bothTransports,
	"grok-interject-100x30":                  bothTransports,
	"grok-plan-100x30":                       bothTransports,
	"grok-plan-80x24":                        bothTransports,
	"grok-subagent-cancel-100x30":            bothTransports,
	"grok-subagent-fail-80x24":               bothTransports,
	"grok-subagent-late-80x24":               bothTransports,
	"grok-subagent-rows-100x30":              bothTransports,
	"grok-subagent-rows-80x24":               bothTransports,
	"grok-subagent-two-view-100x30":          bothTransports,
	"grok-subagent-view-100x30":              bothTransports,
	"grok-subagent-view-80x24":               bothTransports,
	"grok-subagent-view-done-100x30":         bothTransports,
	"help-100x30":                            bothTransports,
	"help-40x12":                             bothTransports,
	"help-80x24":                             bothTransports,
	"help-bottom-100x30":                     bothTransports,
	"help-plugins":                           bothTransports,
	"load-long-100x30":                       bothTransports,
	"markdown-120x40":                        bothTransports,
	"markdown-80x24":                         bothTransports,
	"model-dialog-100x30":                    bothTransports,
	"model-dialog-40x12":                     bothTransports,
	"model-dialog-80x24":                     bothTransports,
	"model-dialog-context-100x30":            bothTransports,
	"model-dialog-effort-100x30":             bothTransports,
	"model-dialog-fast-100x30":               bothTransports,
	"model-dialog-fast-focus-100x30":         bothTransports,
	"model-dialog-four-tabs-100x30":          bothTransports,
	"model-dialog-thinking-100x30":           bothTransports,
	"native-compaction-100x30":               bothTransports,
	"native-echo-80x24":                      bothTransports,
	"native-menu-100x30":                     bothTransports,
	"native-mode-100x30":                     bothTransports,
	"native-plan-denied-100x30":              bothTransports,
	"native-plan-offer-100x30":               bothTransports,
	"native-question-100x30":                 bothTransports,
	"native-resume-100x30":                   bothTransports,
	"native-subagent-background-rows-80x24":  bothTransports,
	"native-subagent-background-wake-100x30": bothTransports,
	"native-subagent-fail-80x24":             bothTransports,
	"native-subagent-rows-80x24":             bothTransports,
	"native-subagent-stop-rows-80x24":        bothTransports,
	"native-subagent-stop-view-100x30":       bothTransports,
	"native-subagent-two-view-100x30":        bothTransports,
	"native-subagent-view-100x30":            bothTransports,
	"native-todos-100x30":                    bothTransports,
	"native-tools-80x24":                     bothTransports,
	"native-usage-80x24":                     bothTransports,
	"permission-noforce-100x30":              bothTransports,
	"plan-100x30":                            bothTransports,
	"planmode-enter-100x30":                  bothTransports,
	"planmode-offer-100x30":                  bothTransports,
	"planmode-refine-100x30":                 bothTransports,
	"plugin-accept-qualified":                bothTransports,
	"plugin-filter-qualified":                bothTransports,
	"plugin-open":                            bothTransports,
	"plugin-provisional":                     bothTransports,
	"plugin-sent":                            bothTransports,
	"provider-picker-100x30":                 inprocOnly,
	"provider-picker-3rows-100x30":           inprocOnly,
	"provider-picker-3rows-40x12":            inprocOnly,
	"provider-picker-80x24":                  inprocOnly,
	"queue-degrade-40x12":                    bothTransports,
	"queue-drain-80x24":                      bothTransports,
	"queue-edit-100x30":                      bothTransports,
	"queue-hover-100x30":                     bothTransports,
	"queue-rows-100x30":                      bothTransports,
	"queue-rows-80x24":                       bothTransports,
	"queue-selected-80x24":                   bothTransports,
	"queue-sendnow-confirm-100x30":           bothTransports,
	"quick-not-quit":                         bothTransports,
	"rename-100x30":                          bothTransports,
	"replay-100x30":                          bothTransports,
	"restore-failed-80x24":                   inprocOnly,
	"resume-picker-100x30":                   inprocOnly,
	"resume-picker-short-100x12":             inprocOnly,
	"select-reverse-100x30":                  bothTransports,
	"sessions-100x30":                        inprocOnly,
	"sessions-80x24":                         inprocOnly,
	"sessions-armed-80x24":                   inprocOnly,
	"sessions-dirs-100x30":                   inprocOnly,
	"sessions-dirs-80x24":                    inprocOnly,
	"sessions-empty-80x24":                   inprocOnly,
	"sessions-new-100x30":                    inprocOnly,
	"sessions-new-80x24":                     inprocOnly,
	"sessions-new-at-100x30":                 inprocOnly,
	"sessions-new-at-80x24":                  inprocOnly,
	"sessions-new-bound-100x30":              inprocOnly,
	"sessions-new-bound-80x24":               inprocOnly,
	"sessions-new-browse-100x30":             inprocOnly,
	"sessions-new-browse-80x24":              inprocOnly,
	"sessions-new-empty-80x24":               inprocOnly,
	"sessions-older-host-100x30":             inprocOnly,
	"sessions-opened-100x30":                 bothTransports,
	"sessions-opened-80x24":                  bothTransports,
	"sessions-saved-100x30":                  inprocOnly,
	"sessions-saved-80x24":                   inprocOnly,
	"sessions-unreachable-80x24":             inprocOnly,
	"select-styled-row-100x30":               bothTransports,
	"select-two-lines-100x30":                bothTransports,
	"shell-composer-100x30":                  bothTransports,
	"shell-row-100x30":                       bothTransports,
	"slash-accept-100x30":                    bothTransports,
	"slash-click-100x30":                     bothTransports,
	"slash-crop-80x12":                       bothTransports,
	"slash-mid-80x24":                        bothTransports,
	"slash-multiline-desc-100x30":            bothTransports,
	"slash-open-100x30":                      bothTransports,
	"slash-scroll-100x30":                    bothTransports,
	"status-60x24":                           bothTransports,
	"task-80x24":                             bothTransports,
	"task-late-80x24":                        bothTransports,
	"task-rows-100x30":                       bothTransports,
	"tasks-80x24":                            bothTransports,
	"task-view-100x30":                       bothTransports,
	"theme-dialog-100x30":                    bothTransports,
	"thought-expanded-120x40":                bothTransports,
	"title-100x30":                           bothTransports,
	"todos-100x30":                           bothTransports,
	"todos-expanded-100x30":                  bothTransports,
	"todos-notes-80x24":                      bothTransports,
	"too-small-30x8":                         bothTransports,
}

// frameProductions is every frame RunFrameScript has produced in this test
// binary, in the order it produced them (astra r71 1): the invocation that
// produced a frame decides what made it, not the frame's bytes — identical
// golden frames exist (composer-six-lines and composer-nine-lines-top, the
// two selections, task and task-late), so a frame is judged by the most
// recent production of its exact bytes (judgeFrame).
//
//   - A run of a test's golden matrix (FrameOpts.matrix) is accounted for by
//     runFrameModes, once its runs agree: one production, the frame they all
//     ended on, owned by the test and carrying the transports its runs used —
//     a credit, spent by the one assertion of that frame, and gone when the
//     test ends (creditFrame).
//   - Every other RunFrameScript is a direct production, recorded by
//     frameProducedHook with no test and no credit: its frame was made in
//     process alone, and an assertion that finds it the most recent
//     production of its bytes fails a golden the manifest runs under both.
var frameProductions = struct {
	mu  sync.Mutex
	log []*frameProduction
}{}

// frameProduction is one frame produced: its digest, and for a matrix's, the
// test that owns it, its transports and whether an assertion has spent it.
type frameProduction struct {
	frame [sha256.Size]byte
	t     testing.TB
	ran   map[frameTransport]bool
	spent bool
}

// frameProductionsMax bounds the log: a direct production that old says what
// a frame no production is found for says too (in process alone), and no
// test produces a fraction as many frames.
const frameProductionsMax = 1 << 16

// installFrameProductions records every direct production (TestMain); a
// matrix's own runs are runFrameModes'.
func installFrameProductions() {
	frameProducedHook = func(opts FrameOpts, plain string) {
		if !opts.matrix {
			produced(&frameProduction{frame: sha256.Sum256([]byte(plain))})
		}
	}
}

// produced appends p to the log.
func produced(p *frameProduction) {
	frameProductions.mu.Lock()
	defer frameProductions.mu.Unlock()
	if len(frameProductions.log) >= frameProductionsMax {
		frameProductions.log = slices.Delete(frameProductions.log, 0, len(frameProductions.log)/2)
	}
	frameProductions.log = append(frameProductions.log, p)
}

// creditFrame records the frame runFrameModes' runs over ran all ended on, as
// t's production; the log lets go of it when t ends.
func creditFrame(t testing.TB, frame string, ran map[frameTransport]bool) {
	p := &frameProduction{frame: sha256.Sum256([]byte(frame)), t: t, ran: ran}
	produced(p)
	t.Cleanup(func() {
		frameProductions.mu.Lock()
		defer frameProductions.mu.Unlock()
		frameProductions.log = slices.DeleteFunc(frameProductions.log, func(q *frameProduction) bool { return q == p })
	})
}

// judgeFrame is what made the frame t asserts, by the most recent production
// of its exact bytes: a matrix production of t's that no assertion has spent
// yet is spent, and answers its transports; anything else — a direct
// production, a matrix production already spent, none at all (a picker's
// direct-Update frame) — is false: made in process alone.
func judgeFrame(t testing.TB, frame string) (map[frameTransport]bool, bool) {
	sum := sha256.Sum256([]byte(frame))
	frameProductions.mu.Lock()
	defer frameProductions.mu.Unlock()
	for i := len(frameProductions.log) - 1; i >= 0; i-- {
		p := frameProductions.log[i]
		if p.frame != sum {
			continue
		}
		if p.t != t || p.spent {
			return nil, false
		}
		p.spent = true
		return p.ran, true
	}
	return nil, false
}

// expectedTransports is the transports golden name must be asserted under:
// the manifest's — for a golden that runs under both, narrowed by
// CRAZE_GOLDEN_TRANSPORT, the local fast loop's knob (the gate pins it to
// both: the Makefile, CI), never by anything else.
func expectedTransports(name string) (map[frameTransport]bool, error) {
	want, ok := goldenManifest[name]
	if !ok {
		return nil, fmt.Errorf("golden %s is not in the manifest (golden_manifest_test.go): list it with the transports it runs under", name)
	}
	expect := map[frameTransport]bool{}
	inproc, socket, err := goldenTransportSet()
	if err != nil {
		return nil, err
	}
	for _, tr := range want {
		if len(want) == 1 || tr == transportInproc && inproc || tr == transportSocket && socket {
			expect[tr] = true
		}
	}
	return expect, nil
}

// frameTransportsErr judges frame by its production (judgeFrame) and says
// whether the runs that produced it are exactly golden name's
// (expectedTransports): a frame with no credit was made in process alone.
func frameTransportsErr(t testing.TB, name, frame string) (map[frameTransport]bool, error) {
	expect, err := expectedTransports(name)
	if err != nil {
		return nil, err
	}
	ran, ok := judgeFrame(t, frame)
	if !ok {
		ran = map[frameTransport]bool{transportInproc: true}
	}
	if !sameTransports(ran, expect) {
		return ran, fmt.Errorf("golden %s was produced under %s, and the manifest runs it under %s", name, transportList(ran), transportList(expect))
	}
	return ran, nil
}

// checkGoldenTransports fails t unless frame, which t holds against golden
// name, was produced by exactly the manifest's runs (frameTransportsErr), and
// answers them.
func checkGoldenTransports(t *testing.T, name, frame string) map[frameTransport]bool {
	t.Helper()
	ran, err := frameTransportsErr(t, name, frame)
	if err != nil {
		t.Fatal(err)
	}
	return ran
}

// goldenAssertions is, for each test and golden a passing assertion held a
// frame against — its frame produced under the golden's whole set, as
// checkGoldenTransports requires — the iterations that did: each *testing.T
// is one iteration of its test under -count (astra r71, the major).
var goldenAssertions = struct {
	mu sync.Mutex
	m  map[goldenPair]map[testing.TB]bool
}{m: map[goldenPair]map[testing.TB]bool{}}

// goldenPair is a test, by name — every iteration's the same — and a golden.
type goldenPair struct{ test, golden string }

// noteGoldenAsserted records that t's frame for golden name matched its file.
func noteGoldenAsserted(t testing.TB, name string) {
	goldenAssertions.mu.Lock()
	defer goldenAssertions.mu.Unlock()
	k := goldenPair{t.Name(), name}
	if goldenAssertions.m[k] == nil {
		goldenAssertions.m[k] = map[testing.TB]bool{}
	}
	goldenAssertions.m[k][t] = true
}

// goldenCoverage is the suite-level check (astra r69 1, r71), run by TestMain
// once every test has passed. It is off unless the run is a whole run of the
// package: no -run or -skip narrowing it, no -list, no -update, and a count
// that runs anything (-count=0 runs nothing). Then:
//
//   - every golden in the manifest was asserted, as its file, under every
//     transport it runs under (expectedTransports) — so a golden's socket run
//     skipped, or its test skipped outright, fails here though no assertion
//     of it ever ran; the one golden a run may lack is the one its test may
//     skip by the repo's rule: native tools' without ripgrep, where CI
//     requires it (requireFrameRG);
//   - and every test that asserted a golden did in every one of the run's
//     -count iterations — so a socket run skipped in a later iteration fails
//     here too.
func goldenCoverage() error {
	for _, f := range []string{"test.run", "test.skip", "test.list"} {
		if fl := flag.Lookup(f); fl != nil && fl.Value.String() != "" {
			return nil
		}
	}
	if *updateGoldens {
		return nil
	}
	n := 1
	if fl := flag.Lookup("test.count"); fl != nil {
		c, err := strconv.Atoi(fl.Value.String())
		if err != nil {
			return fmt.Errorf("golden coverage: -count=%q: %w", fl.Value.String(), err)
		}
		n = c
	}
	if n <= 0 {
		return nil
	}
	goldenAssertions.mu.Lock()
	defer goldenAssertions.mu.Unlock()
	asserted := map[string]bool{}
	var missing []string
	for k, iterations := range goldenAssertions.m {
		asserted[k.golden] = true
		if len(iterations) != n {
			missing = append(missing, fmt.Sprintf("%s: %s asserted in %d of %d iterations", k.golden, k.test, len(iterations), n))
		}
	}
	for name := range goldenManifest {
		if name == "native-tools-80x24" && frameRGMissing() != nil {
			continue
		}
		if !asserted[name] {
			expect, err := expectedTransports(name)
			if err != nil {
				return err
			}
			missing = append(missing, fmt.Sprintf("%s: never asserted under %s", name, transportList(expect)))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("golden coverage (plan 027 §3.16, A8): %d goldens were not asserted under every transport the manifest runs them under, in every iteration — a test or a run was skipped:\n  %s",
		len(missing), strings.Join(missing, "\n  "))
}

func sameTransports(a, b map[frameTransport]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for tr := range a {
		if !b[tr] {
			return false
		}
	}
	return true
}

func transportList(set map[frameTransport]bool) string {
	var names []string
	for tr := range set {
		names = append(names, tr.String())
	}
	slices.Sort(names)
	if len(names) == 0 {
		return "no transport"
	}
	return strings.Join(names, " and ")
}

// TestTheGoldenManifestIsEveryGolden (astra 31): the manifest lists every
// golden file in testdata/ and no file that is not there, with the counts A8
// states — 115 goldens in process and over the socket (plan 030's two opened
// sessions among them), the six picker frames, plan 030's restore-failed, its
// ten session-list frames and the nine of the list's input (C14) in process
// only.
func TestTheGoldenManifestIsEveryGolden(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.golden"))
	if err != nil {
		t.Fatal(err)
	}
	onDisk := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".golden")
		onDisk[name] = true
		if _, ok := goldenManifest[name]; !ok {
			t.Errorf("golden %s is not in the manifest: list it with the transports it runs under", name)
		}
	}
	both, inprocAlone := 0, 0
	for name, runs := range goldenManifest {
		if !onDisk[name] {
			t.Errorf("the manifest lists %s, and testdata holds no such golden", name)
		}
		switch {
		case slices.Equal(runs, bothTransports):
			both++
		case slices.Equal(runs, inprocOnly):
			inprocAlone++
		default:
			t.Errorf("the manifest runs %s under %v: a golden runs under both transports, or in process alone", name, runs)
		}
	}
	if both != 115 || inprocAlone != 26 {
		t.Errorf("the manifest lists %d goldens under both transports and %d in process alone, want 115 and 26 (A8)", both, inprocAlone)
	}
}

// TestATransportCreditIsTheFramesOwn (astra r69 1, r71 1): the transports
// that made a frame are its producing invocation's, not its test's and not
// its bytes'. After a matrix run in a test, a second frame the same test ran
// directly in process carries no credit — asserted against a golden that runs
// under both, it fails — while the matrix frame's credit holds, once: its
// assertion spends it, and a second assertion of the same frame has none
// left. And a matrix run left unasserted, then a direct run drawing the SAME
// bytes: the direct run is the frame's most recent production, and its
// assertion fails — the matrix run's unspent credit is not its to spend. The
// knob is pinned to both here, so the check is the gate's.
func TestATransportCreditIsTheFramesOwn(t *testing.T) {
	t.Setenv("CRAZE_GOLDEN_TRANSPORT", "both")
	isolateSkillsHome(t)
	build := func() Config {
		return Config{Session: frameStub(), Theme: "tokyo-night", Workspace: frameWorkspace(t), Model: "grok", Yolo: true}
	}
	matrix, _, err := runFrameModes(t, build, 60, 24, "<wait:idle>", FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	direct, _, err := RunFrameScript(build(), 60, 24, "<wait:idle>a draft", FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if direct == matrix {
		t.Fatal("fixture: the direct run drew the matrix run's frame")
	}
	const name = "status-60x24"
	if ran, err := frameTransportsErr(t, name, direct); err == nil {
		t.Fatalf("a directly run frame passed as made under %s: it inherited the matrix run's credit", transportList(ran))
	}
	if _, err := frameTransportsErr(t, name, matrix); err != nil {
		t.Fatalf("the matrix frame's own credit: %v", err)
	}
	if _, err := frameTransportsErr(t, name, matrix); err == nil {
		t.Fatal("a frame's credit was spent twice")
	}

	unasserted, _, err := runFrameModes(t, build, 60, 24, "<wait:idle>", FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	same, _, err := RunFrameScript(build(), 60, 24, "<wait:idle>", FrameOpts{Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if same != unasserted {
		t.Fatal("fixture: the direct run did not draw the matrix run's bytes")
	}
	if ran, err := frameTransportsErr(t, name, same); err == nil {
		t.Fatalf("a directly run frame identical to an unasserted matrix frame passed as made under %s: it spent the matrix run's credit", transportList(ran))
	}
}
