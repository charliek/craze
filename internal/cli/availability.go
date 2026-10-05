package cli

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/charliek/craze/internal/agent"
	"github.com/charliek/craze/internal/engine"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/paths"
	"github.com/charliek/craze/internal/rundir"
	"github.com/charliek/craze/internal/tui"
)

// The provider availability check (plan 036 §3.1): one check, three states,
// each state but ready with a one-line reason and a fix. It is served in
// process to the TUI's two pickers and to `craze providers`, and over the hub
// (sessions.createOptions) to any client, each from its own inputs
// (availInputs): the TUI and the CLI resolve binaries as their own sessions
// would (processAvailInputs), the hub as the hosts it creates would
// (hubAvailInputs).
//
// A state marks; it refuses only a choice made in a TUI picker. An explicit
// --provider and the hub's session.create start as they always have, so a
// misjudged state never locks anyone out (plan 036 decision 3) — a start this
// process makes with no picker says the not-GUI verdict first (startWarning);
// $CRAZE_PROVIDER only chooses the picker's default, which the picker gates
// like any row (X24).
//
// Nothing is cached: each call re-reads config.toml, looks the binaries up,
// loads native's model table and asks the login session again, all of it file
// I/O and one syscall on darwin, so a binary installed or a key stored since
// the last listing shows in the next one.

// availState is a provider's availability: ready, needs_setup or unavailable
// — the wire's words (sessions.createOptions' providerState).
type availState string

const (
	// availReady: the provider can start here.
	availReady availState = "ready"
	// availNeedsSetup: it cannot start until something is set up that
	// craze can lead the user through — native with no funded model, whose
	// key /connect stores.
	availNeedsSetup availState = "needs_setup"
	// availUnavailable: it cannot start, and nothing in craze can make it:
	// a binary that is not there, a login session cursor may not work in, a
	// native model table that does not load.
	availUnavailable availState = "unavailable"
)

// human is the state as craze providers' table prints it: the wire's word,
// with needs_setup spelled as words.
func (s availState) human() string {
	if s == availNeedsSetup {
		return "needs setup"
	}
	return string(s)
}

// providerAvail is one provider's availability: its state, and for any state
// but ready why (Reason) and what to do about it (Fix), each one line of at
// most engine.RowTextCells cells (engine.RowLine). Both are "" when ready.
type providerAvail struct {
	P      agent.Provider
	State  availState
	Reason string
	Fix    string
	// notGUI says the state is the not-GUI rule's — cursor outside the macOS
	// login session — the one verdict a start hangs on rather than fails
	// (startWarning). It is never on the wire.
	notGUI bool
}

// The places an agent binary comes from (binResolution.Source), each named in
// a missing binary's reason by the knob that set it (missingBinary).
const (
	binFromFlag   = "flag"   // the launch's --agent-bin
	binFromEnv    = "env"    // $CRAZE_AGENT_BIN
	binFromAgents = "agents" // config.toml's [agents].<provider>
	binFromPath   = "path"   // the provider's own PATH candidates
)

// binResolution is how a provider's agent binary resolved: whether it did,
// where the lookup took it from, and the override it tried ("" for PATH).
type binResolution struct {
	OK     bool
	Source string
	Path   string
}

// availInputs is what one availability answer is computed from (plan 036
// §3.1's input table), so the TUI's, the CLI's and the hub's answers are the
// same rules over their own facts, and a test injects any of them.
type availInputs struct {
	// resolve is how p's agent binary resolves, for an ACP provider.
	resolve func(p agent.Provider) binResolution
	// native is native's state, reason and fix (nativeAvailability).
	native func() (state availState, reason, fix string)
	// gui is the login session of the process the answer is for
	// (rundir.GUISession): this craze's, or the hub's, whose hosts inherit
	// it.
	gui func() (flags uint32, gui, known bool)
	// hubPID is the hub's pid when the answer is the hub's, else 0: the
	// not-GUI fix then names the hub to stop.
	hubPID int
	// configPath is config.toml's path, for the fixes that name [agents].
	configPath string
	// pickerDefault is the provider the TUI's startup picker keeps as its
	// default row even when its binary is missing, "" for none: an optional
	// provider (gx) that does not resolve is left out of every answer, except
	// as this default, where it is unavailable instead (plan 036 §3.1, the
	// gx row). The hub and craze providers set none.
	pickerDefault string
}

// availability is one entry per row, in rows' order (agent.Providers()'s),
// by §3.1's rules: an ACP provider is ready when its binary resolves, and
// unavailable when it does not — an optional one is then left out, unless
// it is in.pickerDefault — and cursor is unavailable besides when the
// process is known to run outside the macOS login session ("not GUI"),
// checked only once its binary resolves, so a missing binary is the reason
// whenever it applies. A login session that is not known never marks anything.
// Native's state is in.native's. Every reason and fix is one line of at most
// engine.RowTextCells cells.
func availability(in availInputs, rows []agent.Provider) []providerAvail {
	out := make([]providerAvail, 0, len(rows))
	for _, p := range rows {
		a, listed := providerAvailability(in, p)
		if !listed {
			continue
		}
		a.Reason, a.Fix = engine.RowLine(a.Reason), engine.RowLine(a.Fix)
		out = append(out, a)
	}
	return out
}

// providerAvailability is p's entry (availability), and false when p is left
// out: an optional provider whose binary is missing.
func providerAvailability(in availInputs, p agent.Provider) (providerAvail, bool) {
	a := providerAvail{P: p, State: availReady}
	if p.InProcess() {
		// Native is the one in-process provider any listing has; another —
		// a hidden one a test plants — has nothing to resolve or set up.
		if p.Name() == agent.NativeProvider().Name() {
			a.State, a.Reason, a.Fix = in.native()
		}
		return a, true
	}
	r := in.resolve(p)
	if !r.OK {
		if p.Optional() && p.Name() != in.pickerDefault {
			return providerAvail{}, false
		}
		a.State = availUnavailable
		a.Reason, a.Fix = missingBinary(p, r, in.configPath)
		return a, true
	}
	if p.Name() == agent.CursorProvider().Name() {
		if _, gui, known := in.gui(); known && !gui {
			a.State, a.Reason, a.Fix = availUnavailable, notGUIReason, notGUIFix
			a.notGUI = true
			if in.hubPID > 0 {
				a.Fix = fmt.Sprintf(notGUIHubFix, in.hubPID)
			}
		}
	}
	return a, true
}

// startWarning is what a start of p this process is about to make with no
// picker says before it starts (LM-2(a), plan 037 §3.5): the not-GUI
// verdict's reason and fix under in, each one line, and "" for every other
// verdict. That verdict is the one a start sits on — cursor blocked on the
// login keychain, at starting… with nothing to say why — where a missing
// binary or an unreadable native table fails on its own, in its own words.
// Nothing is refused: the start goes on (plan 036 decision 3).
func startWarning(in availInputs, p agent.Provider) (reason, fix string) {
	a, listed := providerAvailability(in, p)
	if !listed || !a.notGUI {
		return "", ""
	}
	return engine.RowLine(a.Reason), engine.RowLine(a.Fix)
}

// startCheckFor is tui.Config.StartCheck (LM-2(a)): startWarning for a start
// of p made by this process, whose binary it resolves as that session would
// (processAvailInputs) — p is the launch's own provider in every start the
// TUI checks (an explicit --provider, a load's row), so --agent-bin,
// explicitBin, is its — in this process's login session, which a host it
// spawns inherits.
func startCheckFor(explicitBin string) func(agent.Provider) (string, string) {
	return func(p agent.Provider) (string, string) {
		return startWarning(processAvailInputs(p, explicitBin), p)
	}
}

// The not-GUI rule's reason, the TUI's and CLI's fix, and the hub's, a
// format of the hub's pid (plan 036 §3.1). The reason says "may": being
// outside the login session does not prove cursor cannot reach the keychain
// (plan 035's hint says "if" for the same reason). The hub's fix names the
// hub to stop: "craze ps" in the login session's terminal then starts one
// there, which a plain craze does not.
const (
	notGUIReason = "this craze runs outside the macOS login session (over ssh), where cursor may not reach the login keychain"
	notGUIFix    = "run craze from a terminal on the Mac"
	notGUIHubFix = `kill %d (no session ends), then run "craze ps" in a terminal on the Mac`
)

// missingBinary is the reason and fix for p's binary not resolving under r:
// an override names its knob — the flag, the variable, or p's [agents] key —
// and the path it tried, and its fix points that knob at a binary — in
// config.toml for [agents], or unset for the variable (plan 036 X2); a PATH
// miss names p's first PATH candidate. An optional provider's PATH miss is
// shorter (§3.1's gx row): it is only ever listed as the TUI picker's default.
func missingBinary(p agent.Provider, r binResolution, configPath string) (reason, fix string) {
	var knob, tail string
	switch r.Source {
	case binFromFlag:
		knob = "--agent-bin"
	case binFromEnv:
		knob, tail = envAgentBin, ", or unset it"
	case binFromAgents:
		knob, tail = "[agents]."+p.Name(), " in "+configPath
	}
	if knob != "" {
		return knob + " " + r.Path + " not found", "point " + knob + " at an existing binary" + tail
	}
	bin := p.Name()
	if bins := p.Bins(); len(bins) > 0 {
		bin = bins[0]
	}
	if p.Optional() {
		return bin + " not found", "install " + bin + ", or set [agents]." + p.Name()
	}
	return bin + " not found on PATH", "install " + bin + ", or set [agents]." + p.Name() + " in " + configPath
}

// Native's reasons and fixes (plan 036 §3.1, decision 9). A model table that
// does not load names the file and never the error, whose text can carry a
// key's name — an unknown key, a pasted secret — and craze auth list shows the
// detail locally.
const (
	nativeNoDirReason = "no home directory for native's keys"
	nativeUnreadFix   = `run "craze auth list" for the error`
	nativeNoKeyReason = "no model provider has a key"
	nativeNoKeyFix    = `craze auth login (an API key, or "craze auth login chatgpt" for a ChatGPT plan)`
	// nativeNoKeyFixTUI is needs_setup's fix where the TUI shows it — its
	// pickers' detail line and /provider (pickerAvailability) — leading with
	// what the TUI offers: picking native opens the connect dialog (plan 036
	// §3.6, X23). It fits the picker's box whole; craze providers and the
	// hub keep nativeNoKeyFix.
	nativeNoKeyFixTUI = `pick it to connect one, or run "craze auth login"`
)

// nativeAvailability is native's state from its directory dir and its
// environment getenv, by the pair native's own start runs (agent/native.go):
// modeltable.Load — an error is unavailable, naming the file — then
// StartModel with no model memory — an error, nothing funded, is
// needs_setup. Neither reaches the network or the keychain. A dir of "" (no
// home directory) is unavailable: Load would refuse it too, with no file to
// name.
func nativeAvailability(dir string, getenv func(string) string) (availState, string, string) {
	if dir == "" {
		return availUnavailable, nativeNoDirReason, nativeUnreadFix
	}
	table, err := modeltable.Load(dir)
	if err != nil {
		return availUnavailable, loadErrFile(err, dir) + " could not be read", nativeUnreadFix
	}
	if _, _, err := table.StartModel(nil, getenv); err != nil {
		return availNeedsSetup, nativeNoKeyReason, nativeNoKeyFix
	}
	return availReady, "", ""
}

// loadErrFile is the base name of the file a modeltable.Load error is about
// — never anything else of the error — or "native's model table" when the
// error names no file: a *modeltable.FileError's File; a file that cannot be
// read, an *fs.PathError's Path; else one of dir's two files when the error
// begins with it, as modeltable writes a value of the wrong type ("modeltable:
// <path>: …") and a file missing beside the other ("modeltable: <path> is
// missing …"). Only the error's head is matched, never its body: a key in one
// file can spell the other file's path (plan 036 r1).
func loadErrFile(err error, dir string) string {
	var fe *modeltable.FileError
	if errors.As(err, &fe) {
		return filepath.Base(fe.File)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return filepath.Base(pe.Path)
	}
	msg := err.Error()
	for _, name := range []string{modeltable.ProvidersFile, modeltable.ModelsFile} {
		head := "modeltable: " + filepath.Join(dir, name)
		if rest, ok := strings.CutPrefix(msg, head); ok && (strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, " ")) {
			return name
		}
	}
	return "native's model table"
}

// availConfigPath is config.toml's path for a fix to name, the bare file name
// when there is no craze directory.
func availConfigPath() string {
	return cmp.Or(paths.ConfigPath(), "config.toml")
}

// processAvailInputs is §3.1's TUI and craze providers column: binaries
// resolved exactly as this process's sessions would resolve them
// (agentBinary), launch being the provider the command line resolved and
// explicitBin its --agent-bin ("" for none) — so CRAZE_AGENT_BIN, like the
// flag, is the launch provider's alone — native from this process's craze
// directory and environment, and this process's login session.
func processAvailInputs(launch agent.Provider, explicitBin string) availInputs {
	return availInputs{
		resolve: func(p agent.Provider) binResolution {
			return processResolution(p, launch, explicitBin)
		},
		native:     processNative,
		gui:        rundir.GUISession,
		configPath: availConfigPath(),
	}
}

// processNative is native's availability in this process: its craze
// directory, its environment.
func processNative() (availState, string, string) {
	return nativeAvailability(paths.NativeDir(), os.Getenv)
}

// processResolution is p's binary resolution as a session of p started by
// this process would make it (agentBinary).
func processResolution(p, launch agent.Provider, explicitBin string) binResolution {
	return resolutionOf(p, agentBinary(p, launch, explicitBin, nil))
}

// resolutionOf is p's binary resolution under b, with where it came from:
// when b leaves CRAZE_AGENT_BIN in play (p is the launch's provider), the
// flag, else the variable, which agentBinary leaves to the agent's own
// lookup; otherwise p's [agents] key, else PATH. p is an ACP provider: an
// in-process one, whose agentBin is empty too, never reaches it
// (providerAvailability).
func resolutionOf(p agent.Provider, b agentBin) binResolution {
	r := binResolution{OK: b.resolves(p), Source: binFromPath, Path: b.path}
	switch {
	case !b.noEnv && b.path != "":
		r.Source = binFromFlag
	case !b.noEnv:
		r.Source, r.Path = binFromEnv, os.Getenv(envAgentBin)
	case b.path != "":
		r.Source = binFromAgents
	}
	return r
}

// hubAvailInputs is §3.1's hub column, hubPID being the hub's own pid: a
// binary resolved as a host the hub creates resolves it — [agents], then
// PATH, never CRAZE_AGENT_BIN, which the hub strips from its hosts
// (internal/hub/env.go) — native from the hub's craze directory and
// environment, which its hosts inherit, and the hub's login session, which
// they inherit too. sessions.createOptions answers from it (plan 036 §3.4).
func hubAvailInputs(hubPID int) availInputs {
	return availInputs{
		resolve:    hubResolution,
		native:     processNative,
		gui:        rundir.GUISession,
		hubPID:     hubPID,
		configPath: availConfigPath(),
	}
}

// hubResolution is p's binary resolution as a hub-created host makes it:
// `[agents].<p>`, else p's PATH candidates, with CRAZE_AGENT_BIN left out
// (noEnv, BinaryResolvesNoEnv).
func hubResolution(p agent.Provider) binResolution {
	path, _ := tui.ConfigAgentBin(p.Name())
	return resolutionOf(p, agentBin{path: path, noEnv: true})
}
