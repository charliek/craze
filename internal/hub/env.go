package hub

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/paths"
)

// The environment contract (plan 032 §3.5, P13): what a hub is handed by its
// spawner (Ensure), and what every host the hub spawns is handed by the hub
// (session.create, C15). Without it everything is inherited from whichever
// process happened to spawn first — a relative CRAZE_HOME that the child,
// started in another directory, would read as another craze directory (a
// second namespace), a provider or an agent binary chosen for one launch, the
// marks of a ready pipe that is not the child's, the multiplexer of the pane
// the first spawner ran in.
//
//   - CRAZE_HOME and the removed config-file variable are made absolute
//     against the spawner's working directory when relative, the way the
//     spawner itself resolves them (paths.CrazeDir: a leading "~" or "~/" is
//     the home directory, whitespace around the value is not part of it); a
//     value craze takes for unset (empty, or blank) is left as it is, and so
//     is an unset variable — it stays unset.
//   - Removed: the ready pipe's descriptor and both children's marks
//     (CRAZE_READY_FD, CRAZE_HOST_CHILD, CRAZE_HUB_CHILD — the hub's is set
//     again for the hub child alone, by its spawner, as hostspawn.StartCmd
//     sets a host's), one launch's choices (CRAZE_PROVIDER, CRAZE_DETACH,
//     CRAZE_CONTROL_SOCKET, CRAZE_AGENT_BIN: P7 scopes an agent binary to the
//     launch that named it), and the terminal the first spawner ran in:
//     every HERDR_* and ROOST_* variable, TMUX, TMUX_PANE and STY.
//   - Everything else is kept: PATH, HOME, XDG_*, CRAZE_RUNTIME_DIR (the
//     socket base, and the tests' process marker), CRAZE_JOURNAL, CRAZE_FAKE_*,
//     the providers' API keys (the native harness reads them from the
//     environment), SSH_AUTH_SOCK — stale for a hub first spawned over ssh
//     once that ssh session ends, which nothing here can repair.
//
// A variable the contract does not name is kept, so a variable a later craze
// reads is handed on until the contract says otherwise (an X-amendment, never
// silently: plan 032 §9).

// HubChildEnv marks a hub its spawner started (Ensure, through
// hostspawn.StartCmd): 1 in the hub child's environment alone. The hub takes
// it out of its own environment as it starts (TakeReadyPipe), and the
// contract removes it from whatever it hands on.
const HubChildEnv = "CRAZE_HUB_CHILD"

// crazeHomeEnv is the craze directory's variable (paths.CrazeDir).
const crazeHomeEnv = "CRAZE_HOME"

// removedEnv is every variable the contract removes by its exact name.
var removedEnv = []string{
	hostspawn.ReadyFDEnv, hostspawn.HostChildEnv, HubChildEnv,
	"CRAZE_PROVIDER", "CRAZE_DETACH", "CRAZE_CONTROL_SOCKET", "CRAZE_AGENT_BIN",
	"TMUX", "TMUX_PANE", "STY",
}

// removedPrefixes is every prefix whose variables the contract removes: the
// session-status hosts craze reports to (internal/host's herdr and roost).
var removedPrefixes = []string{"HERDR_", "ROOST_"}

// ChildEnv is environ — a spawner's environment, KEY=value entries — as the
// contract hands it on (the comment above): each entry in its place, less the
// removed ones, CRAZE_HOME and the removed config-file variable made absolute
// against cwd, the spawner's working directory. The home directory a leading
// "~" stands for is environ's own HOME, which the child gets too; with none,
// a "~" value is left for the child to expand as the spawner would have. An
// entry with no "=" is kept as it is. It reads nothing but its arguments.
func ChildEnv(environ []string, cwd string) []string {
	home := lookup(environ, "HOME")
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case !ok:
		case removedVar(k):
			continue
		case k == crazeHomeEnv:
			kv = k + "=" + absCrazeHome(v, cwd, home)
		case k == paths.RemovedConfigEnv:
			kv = k + "=" + absValue(v, cwd)
		}
		out = append(out, kv)
	}
	return out
}

// removedVar reports whether the contract removes the variable named k.
func removedVar(k string) bool {
	if slices.Contains(removedEnv, k) {
		return true
	}
	for _, p := range removedPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// lookup is the value environ gives key — its last entry, as exec takes it —
// or "".
func lookup(environ []string, key string) string {
	v := ""
	for _, kv := range environ {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// absCrazeHome is a CRAZE_HOME value made absolute as paths.CrazeDir reads
// it: trimmed; "~" or a "~/" prefix is home (left as it is with no home);
// then made absolute against cwd. A blank value is craze's unset, and is left
// as it is.
func absCrazeHome(v, cwd, home string) string {
	t := strings.TrimSpace(v)
	if t == "" {
		return v
	}
	if t == "~" || strings.HasPrefix(t, "~/") {
		h := strings.TrimSpace(home)
		if h == "" {
			return v
		}
		t = filepath.Join(h, t[1:])
	}
	return absValue(t, cwd)
}

// absValue is a path value made absolute against cwd, cleaned: as it is when
// blank, or when it is relative and there is no cwd to resolve it against.
func absValue(v, cwd string) string {
	t := strings.TrimSpace(v)
	switch {
	case t == "":
		return v
	case filepath.IsAbs(t):
		return filepath.Clean(t)
	case cwd == "":
		return v
	}
	return filepath.Join(cwd, t)
}
