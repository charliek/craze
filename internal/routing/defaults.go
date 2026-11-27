package routing

import "strings"

// Defaults returns a fresh ladder for each target, including the aliases and
// pinned model spellings verified with claude-agent-acp 0.107.3.
func Defaults() Config {
	targets := map[string]Target{}
	for _, name := range []string{"sol", "astra"} {
		cap := "20m"
		if name == "astra" {
			cap = "25m"
		}
		m := "gpt-6.2-" + name
		tpl := "gpt-{v}-" + name
		targets[name] = Target{"openai", "high", cap, "6.2", []Route{
			{"codex", m, Subscription, tpl}, {"native", "chatgpt/" + m, Subscription, "chatgpt/" + tpl},
			{"native", "openai/" + m, APIKey, "openai/" + tpl}, {"native", "openrouter/gpt-" + name + "-latest", Metered, ""},
		}}
	}
	targets["neon"] = Target{"google", "high", "20m", "4", []Route{
		{"antigravity", "gemini-4-neon", Subscription, "gemini-{v}-neon"},
		{"native", "openrouter/gemini-4-neon", Metered, "openrouter/gemini-{v}-neon"},
	}}
	for _, name := range []string{"opus", "fable"} {
		version, cap := "5.5", "20m"
		if name == "fable" {
			version, cap = "5.2", "25m"
		}
		tpl := "claude-" + name + "-{v_dash}"
		targets[name] = Target{"anthropic", "high", cap, version, []Route{
			{"claude", name, Subscription, tpl}, {"native", "anthropic/claude-" + name + "-" + strings.ReplaceAll(version, ".", "-"), APIKey, "anthropic/" + tpl},
		}}
	}
	return Config{Targets: targets, Roles: map[string]Role{
		"review":             {Candidates: []string{"sol", "opus", "neon"}},
		"adversarial-review": {Candidates: []string{"astra", "fable", "opus"}},
		"panel":              {Seats: []string{"opus", "neon", "sol"}},
	}}
}
