package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/charliek/craze/internal/harness/tool"
	"github.com/charliek/craze/internal/harness/tool/opencode"
)

// spinTool is a parallel tool that reports progress every millisecond until
// its call is cancelled: the tool goroutines' side of the race test.
type spinTool struct{}

func (spinTool) Spec() tool.Spec {
	return tool.Spec{ID: "spin", Description: "Spins, reporting progress, until cancelled.",
		Parameters: map[string]any{"n": map[string]any{"type": "integer"}}, Required: []string{},
		Kind: tool.KindSearch, ReadOnly: true, Parallel: true, Truncate: tool.Head}
}

func (spinTool) Prepare(_ tool.Env, c tool.Call) (tool.Prepared, error) {
	var in struct{ N int }
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return nil, err
	}
	return spinRun(in.N), nil
}

type spinRun int

func (r spinRun) Request() tool.Request { return tool.Request{Title: fmt.Sprintf("spin %d", r)} }

func (r spinRun) Run(ctx context.Context, env tool.Env) tool.Result {
	for i := 0; ; i++ {
		env.Progress(fmt.Sprintf("spin %d tick %d", r, i))
		select {
		case <-ctx.Done():
			return tool.Result{Text: tool.AbortedText, IsError: true, Class: tool.ClassAborted}
		case <-time.After(time.Millisecond):
		}
	}
}

// TestParallelToolsProgressAndCancel (§7.6, run under -race): six spinning
// calls — more than Fantasy's five parallel slots — with reads and a grep
// alongside, all reporting or running at once, cancelled once progress
// flows. Every call gets exactly one ToolFinished, and no progress after it;
// the step persists with exactly one result per call; the dispatcher holds
// nothing afterwards; and nothing reaches the sink once Run has returned,
// though snapshots are still in flight as it does. The control is the
// progress itself: it did arrive, from several calls, before the cancel.
func TestParallelToolsProgressAndCancel(t *testing.T) {
	f := newFixture(t, "http://127.0.0.1:1/v1")
	opts := f.options()
	opts.tools.profiles = func() (*tool.Registry, error) {
		p, err := opencode.Profile()
		if err != nil {
			return nil, err
		}
		p.Tools = append(p.Tools, spinTool{})
		var reg tool.Registry
		return &reg, reg.Register(p)
	}
	s := f.open(opts)
	f.put("a.txt", "alpha\n")
	f.put("b.txt", "beta\n")

	var parts [][]fantasy.StreamPart
	for i := range 6 {
		parts = append(parts, callParts(fmt.Sprintf("s%d", i), "spin", fmt.Sprintf(`{"n":%d}`, i)))
	}
	parts = append(parts,
		callParts("r1", "read", `{"filePath":"a.txt"}`),
		callParts("r2", "read", `{"filePath":"b.txt"}`),
		callParts("g1", "grep", `{"pattern":"alpha"}`),
	)
	f.models["test/a"].push(callStep(parts...), answerWith("never asked for"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu       sync.Mutex
		evs      []Event
		returned atomic.Bool
		late     atomic.Int32
		spinning = map[string]bool{}
	)
	sink := func(ev Event) {
		if returned.Load() {
			late.Add(1)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		evs = append(evs, ev)
		if p, ok := ev.(ToolProgress); ok {
			spinning[p.ID] = true
			if len(spinning) >= 3 {
				cancel()
			}
		}
	}
	res, err := s.Run(ctx, "spin", sink)
	returned.Store(true)
	if err != nil || res.StopReason != StopCancelled {
		t.Fatalf("Run = %+v, %v; want cancelled", res, err)
	}
	time.Sleep(200 * time.Millisecond) // any snapshot still in flight arrives now, or never
	if n := late.Load(); n != 0 {
		t.Fatalf("%d events reached the sink after Run returned", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(spinning) < 3 {
		t.Fatalf("control: progress came from %d calls before the cancel, want at least 3", len(spinning))
	}
	finished := map[string]int{}
	called := map[string]bool{}
	for _, ev := range evs {
		switch e := ev.(type) {
		case ToolCalled:
			called[e.ID] = true
		case ToolFinished:
			finished[e.ID]++
		case ToolProgress:
			if finished[e.ID] > 0 {
				t.Errorf("progress for %s after its ToolFinished", e.ID)
			}
		}
	}
	if len(called) != 9 {
		t.Fatalf("%d calls announced, want 9", len(called))
	}
	for id := range called {
		if finished[id] != 1 {
			t.Errorf("%s finished %d times, want once", id, finished[id])
		}
	}
	lines := entries(transcript(t, s))
	if len(lines) != 3 || strings.Count(lines[2], "[result ")+strings.Count(lines[2], "[error ") != 9 {
		t.Fatalf("transcript:\n%s", strings.Join(lines, "\n"))
	}
	if n := strings.Count(lines[2], "[error s"); n != 6 {
		t.Errorf("%d spins answered aborted, want all 6:\n%s", n, lines[2])
	}
	if n := len(f.models["test/a"].requests()); n != 1 {
		t.Errorf("%d requests, want 1: a cancelled turn asks nothing more", n)
	}
	if n := s.tools.d.Pending(); n != 0 {
		t.Errorf("the dispatcher still holds %d prepared calls", n)
	}
}
