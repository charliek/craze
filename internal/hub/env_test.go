package hub

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/paths"
)

// TestTheEnvironmentContract (plan 032 §3.5, P13) is the contract as a table:
// what a hub — and every host it spawns — is handed of its spawner's
// environment.
func TestTheEnvironmentContract(t *testing.T) {
	removedConfig := paths.RemovedConfigEnv
	const cwd = "/work/space"
	for _, tc := range []struct {
		name string
		in   []string
		want []string
	}{
		{"kept as they are",
			[]string{"PATH=/usr/bin", "HOME=/home/u", "XDG_RUNTIME_DIR=/run/user/1", "XDG_CONFIG_HOME=/c",
				"CRAZE_RUNTIME_DIR=/tmp/czt-1", "CRAZE_JOURNAL=0", "CRAZE_FAKE_SCRIPT=echo", "XAI_API_KEY=k",
				"SSH_AUTH_SOCK=/tmp/ssh/agent", "CRAZE_HUB_IDLE=2s", "TERM=xterm", "LANG=C"},
			[]string{"PATH=/usr/bin", "HOME=/home/u", "XDG_RUNTIME_DIR=/run/user/1", "XDG_CONFIG_HOME=/c",
				"CRAZE_RUNTIME_DIR=/tmp/czt-1", "CRAZE_JOURNAL=0", "CRAZE_FAKE_SCRIPT=echo", "XAI_API_KEY=k",
				"SSH_AUTH_SOCK=/tmp/ssh/agent", "CRAZE_HUB_IDLE=2s", "TERM=xterm", "LANG=C"}},
		{"one launch's choices and the pipes' marks removed",
			[]string{"A=1", "CRAZE_READY_FD=3", "CRAZE_HOST_CHILD=1", "CRAZE_HUB_CHILD=1", "CRAZE_PROVIDER=grok",
				"CRAZE_DETACH=0", "CRAZE_CONTROL_SOCKET=off", "CRAZE_AGENT_BIN=/bin/agent", "B=2"},
			[]string{"A=1", "B=2"}},
		{"the first spawner's terminal removed",
			[]string{"HERDR_ENV=1", "HERDR_PANE_ID=p", "ROOST_AGENT_HOOK=h", "ROOST_SOCKET=/s", "TMUX=/tmp/tmux,1,0",
				"TMUX_PANE=%1", "STY=1.pts", "C=3"},
			[]string{"C=3"}},
		{"only the names themselves",
			[]string{"CRAZE_PROVIDER_X=1", "TMUX_TMPDIR=/t", "HERDR=1", "ROOSTER=1", "XCRAZE_AGENT_BIN=1", "STYLE=1"},
			[]string{"CRAZE_PROVIDER_X=1", "TMUX_TMPDIR=/t", "HERDR=1", "ROOSTER=1", "XCRAZE_AGENT_BIN=1", "STYLE=1"}},
		{"a relative CRAZE_HOME made absolute",
			[]string{"CRAZE_HOME=.craze-b"}, []string{"CRAZE_HOME=/work/space/.craze-b"}},
		{"an absolute one cleaned",
			[]string{"CRAZE_HOME=/a/b/../c/"}, []string{"CRAZE_HOME=/a/c"}},
		{"a tilde one is HOME's",
			[]string{"HOME=/home/u", "CRAZE_HOME=~/craze"}, []string{"HOME=/home/u", "CRAZE_HOME=/home/u/craze"}},
		{"a bare tilde",
			[]string{"CRAZE_HOME=~", "HOME=/home/u"}, []string{"CRAZE_HOME=/home/u", "HOME=/home/u"}},
		{"a tilde with no HOME is the child's to expand",
			[]string{"CRAZE_HOME=~/craze"}, []string{"CRAZE_HOME=~/craze"}},
		{"~user is a relative name",
			[]string{"HOME=/home/u", "CRAZE_HOME=~bob"}, []string{"HOME=/home/u", "CRAZE_HOME=/work/space/~bob"}},
		{"whitespace around the value",
			[]string{"CRAZE_HOME= rel "}, []string{"CRAZE_HOME=/work/space/rel"}},
		{"a blank one is craze's unset",
			[]string{"CRAZE_HOME=", "D=4", "CRAZE_HOME=  "}, []string{"CRAZE_HOME=", "D=4", "CRAZE_HOME=  "}},
		{"unset stays unset",
			[]string{"E=5"}, []string{"E=5"}},
		{"the removed config variable made absolute too",
			[]string{removedConfig + "=conf.toml", removedConfig + "2=x"}, []string{removedConfig + "=/work/space/conf.toml", removedConfig + "2=x"}},
		{"an entry with no =",
			[]string{"JUNK", "F=6"}, []string{"JUNK", "F=6"}},
		{"a value holding =",
			[]string{"G=a=b", "CRAZE_HOME=x=y"}, []string{"G=a=b", "CRAZE_HOME=/work/space/x=y"}},
		{"duplicates each handled in place",
			[]string{"CRAZE_HOME=a", "TMUX=1", "CRAZE_HOME=b"}, []string{"CRAZE_HOME=/work/space/a", "CRAZE_HOME=/work/space/b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := slices.Clone(tc.in)
			got := ChildEnv(tc.in, cwd)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("ChildEnv(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
			if !slices.Equal(tc.in, in) {
				t.Fatalf("ChildEnv changed its input: %q", tc.in)
			}
		})
	}
	t.Run("no cwd", func(t *testing.T) {
		if got := ChildEnv([]string{"CRAZE_HOME=rel"}, ""); !slices.Equal(got, []string{"CRAZE_HOME=rel"}) {
			t.Fatalf("a relative CRAZE_HOME with no cwd to resolve it against: %q", got)
		}
	})
}

// TestChildEnvResolvesCrazeHomeAsCrazeDoes: the CRAZE_HOME a child is handed
// names the very directory its spawner's own paths.CrazeDir resolves to, from
// the spawner's working directory, whatever its form — so the child, started
// in HOME, keys the same namespace.
func TestChildEnvResolvesCrazeHomeAsCrazeDoes(t *testing.T) {
	home := t.TempDir()
	t.Chdir(t.TempDir())
	// As Ensure takes it: the working directory as this process reads it
	// (on macOS a temp directory's /var is /private/var).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(paths.RemovedConfigEnv, "")
	for _, v := range []string{"rel", "./a/../b", "~", "~/x", " spaced ", "/abs/dir", "~bob"} {
		t.Setenv("CRAZE_HOME", v)
		want, err := filepath.Abs(paths.CrazeDir())
		if err != nil {
			t.Fatal(err)
		}
		got := ChildEnv([]string{"HOME=" + home, "CRAZE_HOME=" + v}, cwd)
		if got[1] != "CRAZE_HOME="+want {
			t.Errorf("CRAZE_HOME=%q: the child is handed %q, craze resolves %q", v, got[1], want)
		}
		if !strings.HasPrefix(strings.TrimPrefix(got[1], "CRAZE_HOME="), "/") {
			t.Errorf("CRAZE_HOME=%q is handed on relative: %q", v, got[1])
		}
	}
}
