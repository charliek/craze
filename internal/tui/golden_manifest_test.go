package tui

import (
	"crypto/sha256"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
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
// picker) — and run in process alone.
//
// It is held four ways:
//
//   - TestTheGoldenManifestIsEveryGolden fails on a golden file the manifest
//     does not list and on a listed one that does not exist;
//   - assertGolden fails when the runs that produced the very frame it is
//     handed are not the manifest's set for that golden
//     (checkGoldenTransports): each frame runFrameModes returns carries a
//     credit — the transports that produced it — which the assertion of that
//     frame spends, so a frame made any other way (a direct RunFrameScript, a
//     picker's direct Update, a second assertion of one frame) is in process
//     alone as produced, and fails a golden the manifest runs under both;
//   - and a whole unnarrowed run of the package (goldenCoverage, from
//     TestMain) fails unless every golden in the manifest was asserted under
//     its whole set — so a golden whose socket run was skipped, or whose test
//     was, is caught even though no assertion of it ever ran.

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
	"resume-picker-100x30":                   inprocOnly,
	"resume-picker-short-100x12":             inprocOnly,
	"select-reverse-100x30":                  bothTransports,
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

// frameCredits is, per test, every frame runFrameModes returned that no
// assertion has spent yet, oldest first, each with the transports that
// produced it (astra r69 1): the credit belongs to the frame, not the test.
var frameCredits = struct {
	mu sync.Mutex
	m  map[testing.TB][]frameCredit
}{m: map[testing.TB][]frameCredit{}}

// frameCredit is one frame's credit: the frame, by its digest, and the
// transports whose runs all ended on it.
type frameCredit struct {
	frame [sha256.Size]byte
	ran   map[frameTransport]bool
}

// creditFrame records that runFrameModes' runs over ran all ended on frame,
// for t's assertion of it; what t leaves unspent goes when t ends.
func creditFrame(t testing.TB, frame string, ran map[frameTransport]bool) {
	frameCredits.mu.Lock()
	defer frameCredits.mu.Unlock()
	if _, ok := frameCredits.m[t]; !ok {
		t.Cleanup(func() {
			frameCredits.mu.Lock()
			delete(frameCredits.m, t)
			frameCredits.mu.Unlock()
		})
	}
	frameCredits.m[t] = append(frameCredits.m[t], frameCredit{frame: sha256.Sum256([]byte(frame)), ran: ran})
}

// spendFrame takes the oldest of t's credits for frame: the transports that
// produced it, and false when no run of runFrameModes' returned it — a frame
// made in process some other way.
func spendFrame(t testing.TB, frame string) (map[frameTransport]bool, bool) {
	sum := sha256.Sum256([]byte(frame))
	frameCredits.mu.Lock()
	defer frameCredits.mu.Unlock()
	credits := frameCredits.m[t]
	for i, c := range credits {
		if c.frame == sum {
			frameCredits.m[t] = append(credits[:i:i], credits[i+1:]...)
			return c.ran, true
		}
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

// frameTransportsErr spends frame's credit and says whether the runs that
// produced it are exactly golden name's (expectedTransports): a frame with no
// credit was made in process alone.
func frameTransportsErr(t testing.TB, name, frame string) (map[frameTransport]bool, error) {
	expect, err := expectedTransports(name)
	if err != nil {
		return nil, err
	}
	ran, ok := spendFrame(t, frame)
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

// goldensAsserted is every golden a passing assertion held a frame against in
// this run, with the transports that produced its frames: what goldenCoverage
// judges the run by.
var goldensAsserted = struct {
	mu sync.Mutex
	m  map[string]map[frameTransport]bool
}{m: map[string]map[frameTransport]bool{}}

// noteGoldenAsserted records that golden name's frame, produced under ran,
// matched its file.
func noteGoldenAsserted(name string, ran map[frameTransport]bool) {
	goldensAsserted.mu.Lock()
	defer goldensAsserted.mu.Unlock()
	set := goldensAsserted.m[name]
	if set == nil {
		set = map[frameTransport]bool{}
		goldensAsserted.m[name] = set
	}
	for tr := range ran {
		set[tr] = true
	}
}

// goldenCoverage is the suite-level check (astra r69 1), run by TestMain once
// every test has passed: on a whole run of the package — no -run or -skip
// narrowing it, no -list or -update — every golden in the manifest was
// asserted, as its file, under every transport it runs under
// (expectedTransports). A socket run skipped, or a golden's test skipped
// outright, fails here though no assertion of it ever ran. The one golden a
// run may lack is the one its test may skip by the repo's rule: native tools'
// without ripgrep, where CI requires it (requireFrameRG).
func goldenCoverage() error {
	for _, f := range []string{"test.run", "test.skip", "test.list"} {
		if fl := flag.Lookup(f); fl != nil && fl.Value.String() != "" {
			return nil
		}
	}
	if *updateGoldens {
		return nil
	}
	goldensAsserted.mu.Lock()
	defer goldensAsserted.mu.Unlock()
	var missing []string
	for name := range goldenManifest {
		if name == "native-tools-80x24" && frameRGMissing() != nil {
			continue
		}
		expect, err := expectedTransports(name)
		if err != nil {
			return err
		}
		got := goldensAsserted.m[name]
		for tr := range expect {
			if !got[tr] {
				missing = append(missing, fmt.Sprintf("%s (asserted under %s)", name, transportList(got)))
				break
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	slices.Sort(missing)
	return fmt.Errorf("golden coverage (plan 027 §3.16, A8): %d goldens were not asserted under every transport the manifest runs them under — a test or a run was skipped:\n  %s",
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
// states — 113 goldens in process and over the socket, the six picker frames
// in process only.
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
	if both != 113 || inprocAlone != 6 {
		t.Errorf("the manifest lists %d goldens under both transports and %d in process alone, want 113 and 6 (A8)", both, inprocAlone)
	}
}

// TestATransportCreditIsTheFramesOwn (astra r69 1): the transports that made a
// frame are that frame's, not its test's. After a matrix run in a test, a
// second frame the same test ran directly in process carries no credit —
// asserted against a golden that runs under both, it fails — while the matrix
// frame's credit holds, once: its assertion spends it, and a second assertion
// of the same frame has none left. The knob is pinned to both here, so the
// check is the gate's.
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
}
