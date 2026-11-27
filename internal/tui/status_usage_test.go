package tui

import (
	"testing"

	"charm.land/lipgloss/v2"

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

// TestStatusRowClaudePlan (plan 052 § D2, AC13): a claude session's usage
// part — context, unpriced tokens, never `$`, then the plan and its window's
// use: five_hour reads 5h and every seven_day window 7d.
func TestStatusRowClaudePlan(t *testing.T) {
	claude := agent.UsageState{ContextTokens: 30_938, ContextWindow: 1_000_000,
		Turn:    agent.Spend{Input: 4, Output: 164, CacheRead: 28_511, CacheCreation: 30_907, Unpriced: true},
		Session: agent.Spend{Input: 8, Output: 442, CacheRead: 90_427, CacheCreation: 31_234, Unpriced: true},
		Plan:    "max", Window: "seven_day", WindowUtilization: 0.45}
	with := func(edit func(*agent.UsageState)) *agent.UsageState {
		u := claude
		edit(&u)
		return &u
	}
	for _, tc := range []struct {
		name string
		u    *agent.UsageState
		want string
	}{
		{"seven days", &claude, "3% ctx · 59.6k / 122k tok · max · 7d 45%"},
		{"five hours", with(func(u *agent.UsageState) { u.Window, u.WindowUtilization = "five_hour", 0.125 }), "3% ctx · 59.6k / 122k tok · max · 5h 13%"},
		{"a seven-day model window", with(func(u *agent.UsageState) { u.Window = "seven_day_sonnet" }), "3% ctx · 59.6k / 122k tok · max · 7d 45%"},
		{"a compact 24h window", with(func(u *agent.UsageState) { u.Plan, u.Window, u.WindowUtilization = "prolite", "24h", 0.37 }), "3% ctx · 59.6k / 122k tok · prolite · 24h 37%"},
		{"a compact 15m window", with(func(u *agent.UsageState) { u.Plan, u.Window, u.WindowUtilization = "pro", "15m", 0.5 }), "3% ctx · 59.6k / 122k tok · pro · 15m 50%"},
		{"a compact 1d window", with(func(u *agent.UsageState) { u.Plan, u.Window, u.WindowUtilization = "pro", "1d", 0.1 }), "3% ctx · 59.6k / 122k tok · pro · 1d 10%"},
		{"literal 5h window", with(func(u *agent.UsageState) { u.Plan, u.Window, u.WindowUtilization = "prolite", "5h", 0.25 }), "3% ctx · 59.6k / 122k tok · prolite · 5h 25%"},
		{"literal 7d window", with(func(u *agent.UsageState) { u.Plan, u.Window, u.WindowUtilization = "prolite", "7d", 0.8 }), "3% ctx · 59.6k / 122k tok · prolite · 7d 80%"},
		{"zero duration window ignored", with(func(u *agent.UsageState) { u.Window = "0h" }), "3% ctx · 59.6k / 122k tok · max"},
		{"multi-zero duration window ignored", with(func(u *agent.UsageState) { u.Window = "00h" }), "3% ctx · 59.6k / 122k tok · max"},
		{"negative duration window ignored", with(func(u *agent.UsageState) { u.Window = "-5h" }), "3% ctx · 59.6k / 122k tok · max"},
		{"multi-unit duration window ignored", with(func(u *agent.UsageState) { u.Window = "1h30m" }), "3% ctx · 59.6k / 122k tok · max"},
		{"non-ASCII digit duration window ignored", with(func(u *agent.UsageState) { u.Window = "５h" }), "3% ctx · 59.6k / 122k tok · max"},
		{"a window the row has no word for", with(func(u *agent.UsageState) { u.Window = "overage" }), "3% ctx · 59.6k / 122k tok · max"},
		{"no limit reported yet", with(func(u *agent.UsageState) { u.Window, u.WindowUtilization = "", 0 }), "3% ctx · 59.6k / 122k tok · max"},
		{"no plan known", with(func(u *agent.UsageState) { u.Plan = "" }), "3% ctx · 59.6k / 122k tok · 7d 45%"},
		{"a limit used up", with(func(u *agent.UsageState) { u.WindowUtilization = 1 }), "3% ctx · 59.6k / 122k tok · max · 7d 100%"},
	} {
		if got := usagePart(tc.u); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestStatusRowModelInUse (plan 052 L2): when the agent's usage reports name a
// model of another family than the one picked — claude's plan mode runs on
// Sonnet — the row says the one in use, and the picked one beside it.
func TestStatusRowModelInUse(t *testing.T) {
	m := statusFixture(t)
	m.snap.Models = []agent.ModelInfo{{ID: "haiku", Name: "Haiku 5.5"}, {ID: "default", Name: "Default (recommended)"}}
	m.snap.CurrentModel = "haiku"
	for _, tc := range []struct {
		model, inUse, want string
	}{
		{"haiku", "claude-sonnet-5-5", "sonnet (in use; picked Haiku 5.5 · high)"},
		{"haiku", "claude-haiku-5-5", "Haiku 5.5 (high)"},
		{"haiku", "", "Haiku 5.5 (high)"},
		// The adapter's default names no family: nothing to compare.
		{"default", "claude-sonnet-5-5", "Default (recommended) (high)"},
	} {
		m.snap.CurrentModel = tc.model
		m.snap.Usage = &agent.UsageState{Model: tc.inUse}
		if got := m.modelLabel(); got != tc.want {
			t.Errorf("%s in use under %s: %q, want %q", tc.inUse, tc.model, got, tc.want)
		}
	}
}

// TestStatusRowClaudePlanModeStates (plan 052 L3 F1, O1): a haiku pick at low
// effort, through plan mode, as the session reports it — the in-use model is
// cleared whenever the mode or model option changes (internal/agent), so the
// note shows only when a turn ran on another model than the option it ran
// under, and the picked model reads as the model dialog names it.
func TestStatusRowClaudePlanModeStates(t *testing.T) {
	m := statusFixture(t)
	m.snap.Models = nil
	options := func(model string) []agent.ConfigOption {
		return []agent.ConfigOption{
			{ID: "model", Name: "Model", Category: "model", Type: "select", Current: model,
				SelectValues: []agent.SelectValue{{Value: "haiku", Name: "Haiku 5.5"}, {Value: "sonnet", Name: "Sonnet 5.5"}}},
			{ID: "effort", Name: "Effort", Category: "thought_level", Type: "select", Current: "low",
				SelectValues: []agent.SelectValue{{Value: "low", Name: "low"}, {Value: "high", Name: "high"}}},
		}
	}
	for _, tc := range []struct {
		state, model, inUse, want string
	}{
		{"entering plan: the adapter moves the option to sonnet", "sonnet", "", "sonnet (low)"},
		{"a plan turn on sonnet", "sonnet", "claude-sonnet-5-5", "sonnet (low)"},
		{"leaving plan: the option back to haiku", "haiku", "", "haiku (low)"},
		{"/model haiku in plan mode, then a plan turn on sonnet", "haiku", "claude-sonnet-5-5", "sonnet (in use; picked Haiku 5.5 · low)"},
	} {
		m.snap.CurrentModel = tc.model
		m.snap.Config = options(tc.model)
		m.snap.Usage = &agent.UsageState{Model: tc.inUse}
		if got := m.modelLabel(); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.state, got, tc.want)
		}
	}
}
