package harness

import (
	"context"
	"testing"
	"time"

	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// Tests of the default output ceiling (D-74): what Resolve supplies reaches
// every request and every reserve computed from it.

// dropCeiling removes the file's max_output_tokens from alias, as a models.toml
// without the key loads.
func dropCeiling(o Options, alias string) {
	m := o.Table.Models[alias]
	m.MaxOutputTokens = 0
	o.Table.Models[alias] = m
}

// A model with no max_output_tokens puts the default on the wire, on both
// drivers, in a turn's request and in the aligned summarizer's.
func TestDefaultCeilingReachesTheWire(t *testing.T) {
	for _, driver := range []string{modeltable.DriverOpenAICompat, modeltable.DriverOpenRouter} {
		t.Run(driver, func(t *testing.T) {
			w := newWire(t,
				sseReply(textChunk("hi"), finishChunk("stop", true)),
				sseReply(textChunk("there"), finishChunk("stop", true)),
				sseReply(textChunk(longSummary("1. Request and intent\nSaid hello.")), finishChunk("stop", true)),
			)
			opts := driverOptions(t, w, driver, modeAgent)
			dropCeiling(opts, opts.Model)
			s, err := Open(opts)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			run(t, s, "hello")
			run(t, s, "world")
			if _, err := s.compact(context.Background(), s.cur, 3, store.CompactionManual, "", "", nil); err != nil {
				t.Fatalf("compact: %v", err)
			}
			reqs := w.requests()
			if len(reqs) != 3 {
				t.Fatalf("the server saw %d requests, want 3", len(reqs))
			}
			for i, body := range reqs {
				f := fields(t, body)
				if got := string(f["max_tokens"]); got != "32000" {
					t.Errorf("request %d sent max_tokens %s, want 32000", i+1, got)
				}
				if _, ok := f["max_completion_tokens"]; ok {
					t.Errorf("request %d sent max_completion_tokens %s", i+1, f["max_completion_tokens"])
				}
			}
		})
	}
}

// Fantasy sends a reasoning model's ceiling as max_completion_tokens and drops
// max_tokens; a wire id containing "gpt-5" is one.
func TestDefaultCeilingIsMaxCompletionTokensForGPT5(t *testing.T) {
	w := newWire(t, sseReply(textChunk("hi"), finishChunk("stop", true)))
	opts := driverOptions(t, w, modeltable.DriverOpenAICompat, modeAgent)
	m := opts.Table.Models[opts.Model]
	m.MaxOutputTokens, m.WireModel, m.Efforts, m.DefaultEffort = 0, "openai/gpt-5.6-luna", nil, ""
	opts.Table.Models[opts.Model] = m
	s, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	run(t, s, "hello")
	f := fields(t, w.requests()[0])
	if got := string(f["max_completion_tokens"]); got != "32000" {
		t.Errorf("max_completion_tokens = %s, want 32000", got)
	}
	if _, ok := f["max_tokens"]; ok {
		t.Errorf("max_tokens = %s, want none", f["max_tokens"])
	}
}

// The text-form summarizer carries the default ceiling too.
func TestTextFormSummarizerCarriesTheDefaultCeiling(t *testing.T) {
	f := newFixture(t, "http://unused")
	opts := f.options()
	opts.Model = "other/c" // no max_output_tokens
	s := f.open(opts)
	s.sleep = func(context.Context, time.Duration) {}
	c := f.models["other/c"]
	c.push(answerWith("hi"))
	run(t, s, "hello")
	m := s.cur
	m.r.ContextWindow = 1_000_000 // known, so the reserve is the default's, whole
	c.push(answerWith(longSummary("1. Request and intent\nFrom the text form.")))
	if _, err := s.compact(context.Background(), m, 2, store.CompactionOverflow, "", "", nil); err != nil {
		t.Fatalf("compact: %v", err)
	}
	calls := c.requests()
	last := calls[len(calls)-1]
	if len(last.Tools) != 0 || len(last.Prompt) != 2 {
		t.Fatalf("the last request has %d tools and %d messages; want the text form's request", len(last.Tools), len(last.Prompt))
	}
	if last.MaxOutputTokens == nil || *last.MaxOutputTokens != modeltable.DefaultMaxOutputTokens {
		t.Fatalf("the text form's ceiling = %v, want %d", last.MaxOutputTokens, modeltable.DefaultMaxOutputTokens)
	}
}

// The reserves are computed from the ceiling that is sent: a table model with
// no max_output_tokens reserves the default (held to a quarter of a small
// window), and an explicit one is reserved as it is. The turn's own request
// carries the same number the threshold and the text-form budget subtract.
func TestReservesFollowTheDefaultCeiling(t *testing.T) {
	for _, tc := range []struct {
		name           string
		window, maxOut int
		ceiling        int64 // sent, and reserved
		threshold      int64 // at 85%
		textBudget     int64
	}{
		{"100k window, none: a quarter", 100_000, 0, 25_000, 75_000, 70_000 - 25_000},
		{"1M window, none: 85% unchanged", 1_048_576, 0, 32_000, 1_048_576 * 85 / 100, 1_048_576*70/100 - 32_000},
		{"1M window, explicit 4096", 1_048_576, 4096, 4096, 1_048_576 * 85 / 100, 1_048_576*70/100 - 4096},
		{"100k window, explicit 4096", 100_000, 4096, 4096, 85_000, 70_000 - 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "http://unused")
			windowed(f, "test/a", tc.window, tc.maxOut)
			s := f.open(f.options())
			a := f.models["test/a"]
			a.push(answerWith("ok"))
			run(t, s, "hi")
			call := a.requests()[0]
			if call.MaxOutputTokens == nil || *call.MaxOutputTokens != tc.ceiling {
				t.Errorf("the turn's ceiling = %v, want %d", call.MaxOutputTokens, tc.ceiling)
			}
			r := s.cur.r
			if got := compactionThreshold(r, 85); got != tc.threshold {
				t.Errorf("compactionThreshold = %d, want %d", got, tc.threshold)
			}
			if got := textFormBudget(r, 0); got != tc.textBudget {
				t.Errorf("textFormBudget = %d, want %d", got, tc.textBudget)
			}
		})
	}
}

// A child opens through the same Resolve: the default when its model has no
// max_output_tokens, its own when it has one.
func TestChildRequestCarriesTheCeiling(t *testing.T) {
	for _, tc := range []struct {
		alias string
		want  int64
	}{
		{"other/c", modeltable.DefaultMaxOutputTokens},
		{"test/a", 4096},
	} {
		t.Run(tc.alias, func(t *testing.T) {
			f := newFixture(t, "http://127.0.0.1:1/v1")
			parent := f.open(f.options())
			copts := childOf(f, parent, ChildOptions{AllTools: true})
			copts.Model = tc.alias
			child := f.open(copts)
			m := f.models[tc.alias]
			m.push(answerWith("ok"))
			run(t, child, "go")
			call := m.requests()[0]
			if call.MaxOutputTokens == nil || *call.MaxOutputTokens != tc.want {
				t.Fatalf("the child's ceiling = %v, want %d", call.MaxOutputTokens, tc.want)
			}
		})
	}
}
