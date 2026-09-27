package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/charliek/craze/internal/agent"
)

// usd is whole cents in picodollars (10⁻¹² $; a cent is 10¹⁰).
func usd(cents int64) int64 { return cents * 10_000_000_000 }

// TestStatusRowUsageFormats (plan 028 §3.14, §7 A33): row 1's usage part in
// each of its states — priced, partly unpriced, nothing priced, an unknown
// window, no usage at all — the money's rounding (to the cent, half up) and
// its `<$0.01` edge, the context's percentage, and the part's place in the
// row's pinned drop order: elapsed, branch, usage, provider, model.
func TestStatusRowUsageFormats(t *testing.T) {
	priced := agent.UsageState{ContextTokens: 340_000, ContextWindow: 1_000_000,
		Turn: agent.Spend{Input: 1_200, Output: 800, CostPicoUSD: usd(4)}, Session: agent.Spend{Input: 90_000, Output: 6_000, CostPicoUSD: usd(120)}}
	with := func(edit func(*agent.UsageState)) *agent.UsageState {
		u := priced
		edit(&u)
		return &u
	}
	for _, tc := range []struct {
		name string
		u    *agent.UsageState
		want string
	}{
		{"no usage: an ACP session, or native before its first step", nil, ""},
		{"priced", &priced, "34% ctx · $0.04 / $1.20"},
		{"some of the session's usage unpriced", with(func(u *agent.UsageState) { u.Session.Unpriced = true }),
			"34% ctx · $0.04 / $1.20+"},
		{"some of the turn's too", with(func(u *agent.UsageState) { u.Turn.Unpriced, u.Session.Unpriced = true, true }),
			"34% ctx · $0.04+ / $1.20+"},
		{"nothing priced: the billed tokens, input, cache and output", &agent.UsageState{ContextTokens: 340_000, ContextWindow: 1_000_000,
			Turn:    agent.Spend{Input: 10_000, CacheRead: 1_500, CacheCreation: 500, Output: 300, Reasoning: 200, Unpriced: true},
			Session: agent.Spend{Input: 200_000, CacheRead: 1_000_000, Output: 10_000, Unpriced: true}},
			"34% ctx · 12.3k / 1.21M tok"},
		{"nothing priced, a small session", &agent.UsageState{Turn: agent.Spend{Input: 10, Output: 5, Unpriced: true},
			Session: agent.Spend{Input: 10, Output: 5, Unpriced: true}}, "15 / 15 tok"},
		{"the window unknown", with(func(u *agent.UsageState) { u.ContextWindow = 0 }), "$0.04 / $1.20"},
		{"nothing spent yet", &agent.UsageState{ContextTokens: 2_000, ContextWindow: 200_000}, "1% ctx · $0.00 / $0.00"},
		{"under half a cent, above nothing", with(func(u *agent.UsageState) { u.Turn.CostPicoUSD = usd(1)/2 - 1 }),
			"34% ctx · <$0.01 / $1.20"},
		{"one picodollar", with(func(u *agent.UsageState) { u.Turn.CostPicoUSD = 1 }), "34% ctx · <$0.01 / $1.20"},
		{"half a cent rounds up", with(func(u *agent.UsageState) { u.Turn.CostPicoUSD = usd(1) / 2 }), "34% ctx · $0.01 / $1.20"},
		{"just under a cent and a half rounds down", with(func(u *agent.UsageState) { u.Turn.CostPicoUSD = usd(3)/2 - 1 }),
			"34% ctx · $0.01 / $1.20"},
		{"a cent and a half rounds up", with(func(u *agent.UsageState) { u.Turn.CostPicoUSD = usd(3) / 2 }), "34% ctx · $0.02 / $1.20"},
		{"dollars", with(func(u *agent.UsageState) { u.Session.CostPicoUSD = usd(123_456) + usd(1)/2 }), "34% ctx · $0.04 / $1234.57"},
		{"the context's percentage rounds half up", with(func(u *agent.UsageState) { u.ContextTokens = 345_000 }),
			"35% ctx · $0.04 / $1.20"},
		{"and down under the half", with(func(u *agent.UsageState) { u.ContextTokens = 344_999 }), "34% ctx · $0.04 / $1.20"},
		{"a context past its window", with(func(u *agent.UsageState) { u.ContextTokens = 1_050_000 }), "105% ctx · $0.04 / $1.20"},
	} {
		if got := usagePart(tc.u); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}

	// The part in row 1, after the model, and the pinned drop order as the
	// terminal narrows: at each row's own width, that row is what fits.
	m := statusFixture(t)
	m.snap.Usage = &priced
	for _, want := range []string{
		"craze │ feature/status-rows │ cursor │ Cursor Grok 4.6 (high) │ 34% ctx · $0.04 / $1.20 │ 12m",
		"craze │ feature/status-rows │ cursor │ Cursor Grok 4.6 (high) │ 34% ctx · $0.04 / $1.20", // elapsed
		"craze │ cursor │ Cursor Grok 4.6 (high) │ 34% ctx · $0.04 / $1.20",                       // branch
		"craze │ cursor │ Cursor Grok 4.6 (high)",                                                 // usage
		"craze │ Cursor Grok 4.6 (high)",                                                          // provider
		"craze",                                                                                   // model
	} {
		m.width = lipgloss.Width(want)
		if got := statusText(m.statusRow1()); got != want {
			t.Fatalf("%d cols:\n got %q\nwant %q", m.width, got, want)
		}
	}
	// No usage is no part, and no separator for one.
	m.snap.Usage = nil
	m.width = 120
	if got, want := statusText(m.statusRow1()), "craze │ feature/status-rows │ cursor │ Cursor Grok 4.6 (high) │ 12m"; got != want {
		t.Fatalf("with no usage:\n got %q\nwant %q", got, want)
	}
}
