package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/host"
	"github.com/charliek/craze/internal/sessions"
	"github.com/charliek/craze/internal/tui"
)

// fakeHostEnv is an injected environment: a map for the gates and the same
// entries as KEY=VALUE for the child. Nothing here reads the real process
// environment, which inside a herdr pane has a live gate of its own.
func fakeHostEnv(vars map[string]string, extra ...string) hostEnv {
	var list []string
	for k, v := range vars {
		list = append(list, k+"="+v)
	}
	slices.Sort(list)
	list = append(list, extra...)
	return hostEnv{
		getenv:  func(k string) string { return vars[k] },
		environ: func() []string { return slices.Clone(list) },
	}
}

// herdrGate is herdr's documented gate, met, against a socket nothing listens
// on: nothing in these tests publishes, so nothing ever dials it.
func herdrGate(t *testing.T) map[string]string {
	return map[string]string{
		"HERDR_ENV":         "1",
		"HERDR_SOCKET_PATH": filepath.Join(t.TempDir(), "no-herdr.sock"),
		"HERDR_PANE_ID":     "w9:p9",
	}
}

func withoutKey(m map[string]string, key string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// TestHostChildEnvStripsTheHerdrGate is plan 015 §3.4's child env strip: with
// herdr active the child gets every entry except exactly HERDR_ENV — matched on
// the key, so a value holding "=" and a key that only starts with HERDR_ENV are
// both kept, and so are the pane id, socket and binary the herdr CLI needs.
// With no host active the answer is nil, which inherits craze's environment.
func TestHostChildEnvStripsTheHerdrGate(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin:/bin",
		"HERDR_ENV=1",
		"HERDR_SOCKET_PATH=/tmp/herdr.sock",
		"HERDR_PANE_ID=w4:p22",
		"HERDR_BIN_PATH=/usr/bin/herdr",
		"HERDR_ENVIRONMENT=kept",
		"CRAZE_ARGS=a=b=c",
		"NOTE=HERDR_ENV=1",
		"XAI_API_KEY=secret",
	}
	orig := slices.Clone(environ)

	got := hostSet{herdr: &host.Herdr{}}.childEnv(environ)
	want := slices.DeleteFunc(slices.Clone(environ), func(kv string) bool { return kv == "HERDR_ENV=1" })
	if !slices.Equal(got, want) {
		t.Fatalf("child env:\n got %q\nwant %q", got, want)
	}
	if !slices.Equal(environ, orig) {
		t.Fatalf("the caller's environ was modified: %q", environ)
	}

	if got := (hostSet{}).childEnv(environ); got != nil {
		t.Fatalf("no host active must inherit (nil Env), got %q", got)
	}
}

// TestResolveHostsGates: herdr is active only with all three of its variables
// set and HERDR_ENV exactly "1", and never when --no-host-status or
// `host_status = false` turned reporting off, whatever the environment says.
func TestResolveHostsGates(t *testing.T) {
	gate := herdrGate(t)
	cases := []struct {
		name   string
		argv   []string
		config string
		vars   map[string]string
		want   bool
	}{
		{"gate met", nil, "", gate, true},
		{"HERDR_ENV unset", nil, "", withoutKey(gate, "HERDR_ENV"), false},
		{"HERDR_ENV not 1", nil, "", map[string]string{"HERDR_ENV": "true", "HERDR_SOCKET_PATH": gate["HERDR_SOCKET_PATH"], "HERDR_PANE_ID": "w9:p9"}, false},
		{"HERDR_SOCKET_PATH unset", nil, "", withoutKey(gate, "HERDR_SOCKET_PATH"), false},
		{"HERDR_PANE_ID unset", nil, "", withoutKey(gate, "HERDR_PANE_ID"), false},
		{"no environment", nil, "", nil, false},
		{"--no-host-status", []string{"--no-host-status"}, "", gate, false},
		{"host_status = false", nil, "host_status = false\n", gate, false},
		{"host_status = true", nil, "host_status = true\n", gate, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			indexHome(t)
			if tc.config != "" {
				if err := os.WriteFile(os.Getenv("CRAZE_CONFIG"), []byte(tc.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, f := parseTUIFlags(t, tc.argv...)
			s := resolveHosts(f, fakeHostEnv(tc.vars))
			if got := s.herdr != nil; got != tc.want {
				t.Fatalf("herdr active = %v, want %v", got, tc.want)
			}
			if got := len(s.reporters()) == 1; got != tc.want {
				t.Fatalf("reporters %d, want herdr active %v", len(s.reporters()), tc.want)
			}
		})
	}
}

// TestAttachHostBuildsAHubOnlyForAnActiveHost is the construction half of plan
// 015 §3.5: Config.Host stays nil with no host active — including when
// --no-host-status turned a met gate off — and is a hub when herdr's gate is
// met. The child env the same run would spawn with agrees with it.
func TestAttachHostBuildsAHubOnlyForAnActiveHost(t *testing.T) {
	gate := herdrGate(t)
	cases := []struct {
		name string
		argv []string
		vars map[string]string
		want bool
	}{
		{"no host", nil, nil, false},
		{"--no-host-status", []string{"--no-host-status"}, gate, false},
		{"herdr gate met", nil, gate, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			indexHome(t)
			_, f := parseTUIFlags(t, tc.argv...)
			env := fakeHostEnv(tc.vars, "PATH=/usr/bin")
			hosts := resolveHosts(f, env)

			var cfg tui.Config
			attachHost(&cfg, hosts, &bytes.Buffer{})
			if (cfg.Host != nil) != tc.want {
				t.Fatalf("Config.Host = %v, want set %v", cfg.Host, tc.want)
			}
			if cfg.Host != nil {
				if _, ok := cfg.Host.(*host.Hub); !ok {
					t.Fatalf("Config.Host is %T, want *host.Hub", cfg.Host)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				cfg.Host.Close(ctx)
			}

			childEnv := hosts.childEnv(env.list())
			opts := sessionOptions(f, t.TempDir(), "", io.Discard, childEnv, agent.CursorProvider(), sessions.Row{})
			switch {
			case !tc.want && opts.Env != nil:
				t.Fatalf("no host active, but the child env is replaced: %q", opts.Env)
			case tc.want && (opts.Env == nil || slices.Contains(opts.Env, "HERDR_ENV=1") || !slices.Contains(opts.Env, "HERDR_PANE_ID=w9:p9")):
				t.Fatalf("herdr active, child env %q", opts.Env)
			}
		})
	}
}

// TestHostWarnIsOneLineOnDiag: a reporter failure reaches the deferred stderr
// buffer as exactly one line. The listener accepts and hangs up without a
// reply, so every send fails; the test waits for the first dial before it
// closes, so a report was attempted and the hub owes a release. Whichever of
// the report and the release fails first is the one warning.
func TestHostWarnIsOneLineOnDiag(t *testing.T) {
	indexHome(t)
	// Not t.TempDir(): a unix socket path is capped at 104 bytes on macOS.
	dir, err := os.MkdirTemp("", "h")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	dialled := make(chan struct{}, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
			select {
			case dialled <- struct{}{}:
			default:
			}
		}
	}()

	_, f := parseTUIFlags(t)
	diag := &deferredStderr{}
	var cfg tui.Config
	gate := map[string]string{"HERDR_ENV": "1", "HERDR_SOCKET_PATH": sock, "HERDR_PANE_ID": "w9:p9"}
	attachHost(&cfg, resolveHosts(f, fakeHostEnv(gate)), diag)
	if cfg.Host == nil {
		t.Fatal("setup: herdr gate met but no hub")
	}
	cfg.Host.Publish(host.Status{Kind: host.Working, Detail: host.DetailPrompt})
	select {
	case <-dialled:
	case <-time.After(5 * time.Second):
		t.Fatal("the hub never dialled the socket")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cfg.Host.Close(ctx)

	var out bytes.Buffer
	diag.flush(&out)
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "host status: herdr: ") {
		t.Fatalf("diag = %q, want one host status: herdr: line", out.String())
	}
}
