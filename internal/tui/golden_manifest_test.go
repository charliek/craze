package tui

import (
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
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
// It is held three ways: TestTheGoldenManifestIsEveryGolden fails on a golden
// file the manifest does not list and on a listed one that does not exist;
// and assertGolden fails when the runs that produced the frame it is handed
// are not the manifest's set for that golden (checkGoldenTransports) — a
// golden whose socket run was skipped, or a picker frame run over a socket.

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

// frameTransportsRan is the transports runFrameModes last ran for each test:
// what produced the frame the test's next assertGolden is handed.
var frameTransportsRan = struct {
	mu sync.Mutex
	m  map[testing.TB]map[frameTransport]bool
}{m: map[testing.TB]map[frameTransport]bool{}}

// noteFrameTransports records ran as what produced t's frames, until t ends.
func noteFrameTransports(t testing.TB, ran map[frameTransport]bool) {
	frameTransportsRan.mu.Lock()
	defer frameTransportsRan.mu.Unlock()
	if _, ok := frameTransportsRan.m[t]; !ok {
		t.Cleanup(func() {
			frameTransportsRan.mu.Lock()
			delete(frameTransportsRan.m, t)
			frameTransportsRan.mu.Unlock()
		})
	}
	frameTransportsRan.m[t] = ran
}

// checkGoldenTransports fails t unless the frame it holds against golden name
// was produced by exactly the manifest's runs: for a golden that runs under
// both, both — narrowed by CRAZE_GOLDEN_TRANSPORT, the local fast loop's knob,
// never by anything else — and for an in-process-only frame, in process alone
// (a frame made without runFrameModes, as a picker's direct-Update frame is,
// is in process by construction).
func checkGoldenTransports(t *testing.T, name string) {
	t.Helper()
	want, ok := goldenManifest[name]
	if !ok {
		t.Fatalf("golden %s is not in the manifest (golden_manifest_test.go): list it with the transports it runs under", name)
	}
	frameTransportsRan.mu.Lock()
	ran, viaRuns := frameTransportsRan.m[t]
	frameTransportsRan.mu.Unlock()
	if !viaRuns {
		ran = map[frameTransport]bool{transportInproc: true}
	}
	expect := map[frameTransport]bool{}
	inproc, socket := goldenTransports(t)
	for _, tr := range want {
		if len(want) == 1 || tr == transportInproc && inproc || tr == transportSocket && socket {
			expect[tr] = true
		}
	}
	if !sameTransports(ran, expect) {
		t.Fatalf("golden %s was produced under %s, and the manifest runs it under %s", name, transportList(ran), transportList(expect))
	}
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
