package tui

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
)

// Models picked up live, through a real native session in both transports and
// both gate modes (plan 034 §3.4 "TUI (C6)", A16, A17, A26): the session's
// table is the one it LOADED from the TUI's native directory — never one a
// test handed in, which no reload reads (nativeSessionTweak) — so a key /connect
// stores, or a sign-in's files, are what its refresh finds. Every provider is a
// fixture one, every model a scripted stand-in that says which model asked.

// liveModel is a scripted model for whichever alias a session builds: one turn
// of "hello from <alias>", so a frame says which model answered.
func liveModel(r modeltable.Resolved) (fantasy.LanguageModel, error) {
	return &nativeScriptedModel{
		provider: r.ProviderID, wire: r.WireModel,
		steps: [][]fantasy.StreamPart{append(nativeTextParts("hello from "+r.Alias), nativeFinishParts()...)},
	}, nil
}

// liveNativeSession is a native session on alpha/one that reads its models
// from home, and reloads them: the table is loaded from there (Options carries
// none), the environment is getenv's, and its models are liveModel's.
func liveNativeSession(t *testing.T, ws, home string, getenv func(string) string) agent.Session {
	t.Helper()
	at := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	return agent.NewNative(
		agent.Options{Workspace: ws, ContentHome: t.TempDir(), Model: "alpha/one", Interactive: true, NoPrimary: frameNoPrimary},
		func(o *harness.Options) {
			o.Home = home
			o.Getenv = getenv
			o.NewModel = liveModel
			o.Now = func() time.Time { return at }
		})
}

// liveConfig is the TUI over a live native session whose directory is the
// TUI's own (so a refresh's sameDir is true). dirOf builds the fixture.
func liveConfig(t *testing.T, fixture func() (string, func(string) string)) Config {
	t.Helper()
	ws := frameWorkspace(t)
	dir, getenv := fixture()
	return Config{Session: liveNativeSession(t, ws, dir, getenv), Theme: "tokyo-night", Workspace: ws, Yolo: true, NativeDir: dir, Getenv: getenv}
}

// runLiveFrame runs keys over cfg's builder in every run, screened for secrets.
func runLiveFrame(t *testing.T, build func() Config, cols, rows int, keys string, secrets ...string) string {
	t.Helper()
	isolateSkillsHome(t)
	got, _, err := runFrameModesScreened(t, build, cols, rows, keys, FrameOpts{Timeout: 30 * time.Second}, connectScreen(t, secrets...))
	if err != nil {
		t.Fatalf("run frame script: %v", err)
	}
	return got
}

// TestFrameNativeLiveKeySave (A16): a key /connect stores is a provider's
// models in /model at once — no /exit, no craze -c. Idle, /connect saves
// gamma's key; the notice says its models are in /model now; /model lists
// Gamma Big (and, every provider connected, no connect row); a switch to it
// works, and a turn on it answers as it. The same frame in process, over the
// socket, inline and asynchronous.
func TestFrameNativeLiveKeySave(t *testing.T) {
	const key = "sk-live-gamma-dummy-key"
	build := func() Config {
		return liveConfig(t, func() (string, func(string) string) { return connectFixture(t, nil) })
	}
	script := "<wait:idle>/connect<enter><wait:text:Gamma><enter><wait:text:Gamma API key><paste:" + key + "><enter>" +
		"<wait:text:Its models are in /model now.>"
	got := runLiveFrame(t, build, 100, 30, script+"/model<enter><wait:text:Gamma Big>", key)
	assertFrameGolden(t, "native-live-model-dialog-100x30", 100, 30, got,
		[]string{"Connected Gamma. Its models are in /model now.", "> Alpha One", "Gamma Big"},
		[]string{connectRowText, "/exit and run craze -c"})

	// The dialog's Enter applies the switch on a command of its own
	// (chainLock), not a gated call, so the frame after it is idle before the
	// switch has answered; its note is written when the answer lands. The
	// script waits for that note — the switch acknowledged — before it types,
	// as a person reads it: typed sooner, the prompt's row lands above the
	// note whenever the answer is slower than the keys (a socket's round trip,
	// one CPU), in either transport. The reply's text is on screen before its
	// turn has ended, so the frame is taken once the TUI is idle again: every
	// run's frame must be the same settled one (plan 036: the macOS runner and
	// a 5% CPU quota caught a run mid-turn, "✳ Working" against a blank line).
	got = runLiveFrame(t, build, 100, 30, script+"/model<enter><wait:text:Gamma Big>Gamma<enter><wait:text:model → gamma/big><wait:idle>"+
		"hi<enter><wait:text:hello from gamma/big><wait:idle>", key)
	if !strings.Contains(got, "hello from gamma/big") || !strings.Contains(got, "Gamma Big") {
		t.Fatalf("a switch to the model the key funded, and a turn on it, did not work:\n%s", got)
	}
}

// writeSignedInPlan is what a finished sign-in leaves in dir, as the files
// chatgptauth writes them and the table reads: the registration with plan
// usage, a token record (dummy values: the table only looks at the file), and
// the account's model list bound to the registration.
func writeSignedInPlan(dir string) (*chatgptauth.Models, error) {
	const subject, client = "user-live-0001", "oaiapp_live000000000001"
	if err := os.MkdirAll(chatgptauth.AuthDir(dir), 0o700); err != nil {
		return nil, err
	}
	reg, err := json.Marshal(chatgptauth.Client{ClientID: client, Subject: subject, Email: signInEmail, PlanUsage: true, NoticeShown: true})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(chatgptauth.ClientFile(dir), reg, 0o600); err != nil {
		return nil, err
	}
	tok, err := json.Marshal(map[string]any{
		"version": 1, "client_id": client, "issuer": "https://auth.openai.com", "subject": subject, "email": signInEmail,
		"access_token": "live-access-token-dummy", "access_expires_at": time.Now().Add(time.Hour).UTC(),
		"refresh_token": "live-refresh-token-dummy", "id_token": "live-id-token-dummy", "incarnation": "live", "generation": 1,
	})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(chatgptauth.TokenFile(dir), tok, 0o600); err != nil {
		return nil, err
	}
	list := &chatgptauth.Models{Version: 1, Subject: subject, ClientID: client, ClientVersion: modeltable.ChatGPTModelsClientVersion(), FetchedAt: time.Now().UTC(),
		Models: []chatgptauth.Model{
			{Slug: "gpt-5.6-sol", MinClientVersion: "0.144.0", DisplayName: "GPT-5.6 Sol", ContextWindow: 272000, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium", InputModalities: []string{"text"}},
			{Slug: "gpt-6-astra", MinClientVersion: "0.144.0", DisplayName: "GPT-6 Astra", ContextWindow: 272000, Efforts: []string{"low", "medium", "high"}, DefaultEffort: "medium", InputModalities: []string{"text"}, Priority: 1},
		}}
	b, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	return list, os.WriteFile(chatgptauth.ModelsFile(dir), b, 0o600)
}

// noOpenAIForLive keeps a session's own background fetch of the plan's list
// off the network: both of chatgptauth's endpoints at a loopback port nothing
// listens on.
func noOpenAIForLive(t *testing.T) {
	t.Helper()
	t.Setenv(chatgptauth.APIEnv, "http://127.0.0.1:1/v1")
	t.Setenv(chatgptauth.IssuerEnv, "http://127.0.0.1:1")
}

// TestFrameNativeAnotherDirectorySaysSo (A26): a TUI whose native directory is
// not the one its session's host reads says so, over the wire as in process —
// and says that alone: the old note's "/exit and run craze -c" is wrong advice
// for a TUI attached to that host, whose /exit ends the shared session and
// whose craze -c finds nothing of it here (plan 034 C6r, from V2).
// nativePickerSession's home is its own, as a detached host's is another
// process's CRAZE_HOME.
func TestFrameNativeAnotherDirectorySaysSo(t *testing.T) {
	isolateSkillsHome(t)
	const key = "sk-live-other-dir-dummy-key"
	build := func() Config {
		ws := frameWorkspace(t)
		dir, getenv := connectFixture(t, nil)
		return Config{Session: nativePickerSession(t, ws), Theme: "tokyo-night", Workspace: ws, Yolo: true, NativeDir: dir, Getenv: getenv}
	}
	got := runLiveFrame(t, build, 100, 30, "<wait:idle>/connect<enter><wait:text:Gamma><enter><wait:text:Gamma API key><paste:"+key+"><enter>"+
		"<wait:text:Connected Gamma in this craze directory>", key)
	flat := strings.Join(strings.Fields(got), " ")
	const want = "Connected Gamma in this craze directory, but this session's host reads another one, so it does not see its models."
	if !strings.Contains(flat, want) {
		t.Fatalf("the frame is missing %q:\n%s", want, got)
	}
	for _, w := range []string{"/exit and run craze -c", "New sessions offer", "/model now"} {
		if strings.Contains(flat, w) {
			t.Fatalf("the frame says %q to a TUI whose session reads another directory:\n%s", w, got)
		}
	}
}
