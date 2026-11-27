package routing

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/charliek/craze/internal/errkind"
)

func loadText(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestStrictMerge(t *testing.T) {
	cfg, err := loadText(t, `[agents]
other = "ignored"
[routing]
allow_paid_fallback = true
[targets.sol]
cap = "1m"
routes = []
[roles.review]
candidates = []
`)
	if err != nil || !cfg.Routing.AllowPaidFallback || cfg.Targets["sol"].Maker != "openai" || cfg.Targets["sol"].Cap != "1m" || len(cfg.Targets["sol"].Routes) != 0 || len(cfg.Roles["review"].Candidates) != 0 || len(cfg.Targets["astra"].Routes) != 4 {
		t.Fatalf("%+v %v", cfg, err)
	}
	for _, bad := range []string{"invalid=[", "[roles.review]\n\"candidateſ\"=['opus']", "[\"targetſ\".sol]\nroutes=[]", "[routing]\nALLOW_PAID_FALLBACK=true", "[routing]\nallow_paid_fallbac = true", "[routing]\nallow_paid_fallback = 1", "[targets.sol]\ncap='-1m'", "[targets.sol]\ncap='none'", "[targets.sol]\nunknown=0", "[[targets.sol.routes]]\nprovider='codex'\nmodel='m'\nbilling='cheap'", "[[targets.sol.routes]]\nprovider='codex'\nmodel='m'\nversion_templat='abc'", "[roles.mine]\ncandidates=['no-such-target']", "[roles.review]\nseats=[]\ncandidates=[]"} {
		_, err := loadText(t, bad)
		if info, ok := errkind.Of(err); !ok || info.Kind != errkind.Config {
			t.Errorf("accepted %q: %v", bad, err)
		}
	}
}

func TestVersionsAndPaidGuard(t *testing.T) {
	funded := func(p, _ string) Facts {
		if p == "codex" {
			return Facts{Efforts: []string{"low", "high", "xhigh"}}
		}
		return Facts{}
	}
	cfg := Defaults()
	for _, tc := range []struct {
		target, want string
		pin          bool
	}{{"sol@6.1", "gpt-6.1-sol", true}, {"opus", "opus", false}, {"fable", "fable", false}, {"fable@5.1", "claude-fable-5-1", true}} {
		res, ok, err := cfg.Resolve(Request{Target: tc.target, Effort: "max"}, funded)
		if err != nil || !ok || res.Routes[0].Model != tc.want || (res.Routes[0].Version != nil) != tc.pin {
			t.Fatalf("%s %+v %v", tc.target, res, err)
		}
		if tc.target == "sol@6.1" && (res.Routes[0].Effort != "xhigh" || len(res.Warnings) != 1 || len(res.Routes) != 2 || res.Skipped[1].Reason != NoVersionTemplate) {
			t.Errorf("%+v", res)
		}
	}
	for _, req := range []Request{{Target: "unknown"}, {Target: "sol@abc"}, {Target: "sol@"}, {Target: "sol@6", Version: "6"}, {Target: "sol", Role: "panel"}, {Version: "3"}} {
		if _, _, err := cfg.Resolve(req, funded); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}
	cfg.Targets["key"] = Target{Maker: "openai", Effort: "high", Cap: "1m", Routes: []Route{{Provider: "native", Model: "custom", Billing: Subscription}}}
	probe := func(string, string) Facts { return Facts{Billing: APIKey} }
	res, ok, _ := cfg.Resolve(Request{Target: "key"}, probe)
	if ok || res.Skipped[0].Reason != PaidBlocked {
		t.Fatal(res)
	}
	res, ok, _ = cfg.Resolve(Request{Target: "key", AllowPaid: true}, probe)
	if !ok || res.Routes[0].Billing != APIKey {
		t.Fatal(res)
	}
}
func TestCallerAndPanel(t *testing.T) {
	env := map[string]string{"CODEX_THREAD_ID": "1", "CLAUDECODE": "1"}
	get := func(k string) string { return env[k] }
	c, err := DetectCaller(get, "", "")
	if err != nil || !slices.Equal(c.Providers, []string{"claude", "codex"}) || c.Immediate != "" {
		t.Fatal(c, err)
	}
	cfg := Defaults()
	probe := func(string, string) Facts { return Facts{} }
	res, ok, _ := cfg.Resolve(Request{Role: "panel", Caller: c}, probe)
	if !ok || res.Seats[0].Mode != "native_subagent" || res.Seats[1].Mode != "craze_review" || res.Seats[2].Mode != "native_subagent" {
		t.Fatal(res)
	}
	res, ok, _ = cfg.Resolve(Request{Role: "review", Caller: c}, probe)
	if !ok || res.Routes[0].Target != "neon" || res.Skipped[0].Reason != SameMaker {
		t.Fatal(res)
	}
	c, err = DetectCaller(get, "", "codex")
	if err != nil || !slices.Equal(c.Providers, []string{"codex"}) || c.Immediate != "codex" {
		t.Fatal(c, err)
	}
	res, ok, _ = cfg.Resolve(Request{Target: "sol", Caller: c}, probe)
	if !ok || res.Routes[0].Provider != "native" || res.Skipped[0].Reason != SelfDelegation {
		t.Fatal(res)
	}
	res, ok, _ = cfg.Resolve(Request{Role: "panel", Caller: c}, func(string, string) Facts { return Facts{Reason: BinaryMissing} })
	if ok || len(res.Seats) != 3 || res.Seats[0].Status != "unresolvable" {
		t.Fatal(res)
	}
}
