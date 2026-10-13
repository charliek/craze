package hub

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/hostspawn"
	"github.com/charliek/craze/internal/protocol"
	"github.com/charliek/craze/internal/version"
)

// Upgrading under a live hub (plan 046 §3.C). A hub outlives the craze that
// started it: an upgrade that replaces the binary in place (apt, macOS brew)
// leaves it spawning the new craze's hosts (SD-47 keeps that working), but
// one that removes the old binary (Linuxbrew, which deletes the old versioned
// directory) leaves it re-executing a path that is gone, and every session it
// would start fails before its host exists. Upgrades are rare, so the fix is
// words, not machinery: the hub says so and names its own pid (U1), a client
// says it for a hub from before that (U2, UpgradeRemedy), and nothing retires
// — the hub serves on, every other client keeps its connection, and a create
// after the path is back succeeds on the same hub.

// hubReplaced is a launch's answer when this hub's own craze is gone from its
// path (U1): the path, then this hub's pid. hubReplacedMark is the part of it
// no other answer says, by which a client knows the hub has named the remedy
// itself (UpgradeRemedy).
const (
	hubReplaced     = "this hub's craze (%s) was replaced by an upgrade: stop this hub with kill %d (no session ends) and run this again"
	hubReplacedMark = ") was replaced by an upgrade: stop this hub with kill "
)

// launchRefusal is the answer to a launch whose start failed (creator.launch,
// its closing aside), said in the hub's log under tag: unavailable, reason
// spawn_failed — naming the remedy when the start failed because this hub's
// craze is gone from its path (hostspawn.ExecutableGoneError, U1), and
// otherwise in words that name no path (the path is the log's alone). Only
// words: the hub does not retire, and goes on serving as it was.
func (h *hub) launchRefusal(tag string, err error) *protocol.Error {
	var gone *hostspawn.ExecutableGoneError
	if !errors.As(err, &gone) {
		h.logf("%s: start the session host: %v", tag, err)
		return spawnFailed("the session host cannot be started")
	}
	h.logf("%s: start the session host: %v (%v); the hub serves on, and says how to stop it", tag, gone, gone.Err)
	return spawnFailed(hubReplaced, gone.Path, h.pid)
}

// UpgradeRemedy is what a client adds to err, the refusal of the hub whose
// hello is hello (U2), "" for nothing: for a spawn_failed from a hub whose
// craze version is not this craze's, the hub's pid, both versions and how to
// stop it — a hub from before U1, still re-executing a craze an upgrade
// removed, says no more than that its host cannot be started. Never for a hub
// of this craze's version, which names the remedy itself, nor for an answer
// that already names it (hubReplacedMark: a later hub's); and never without
// the hub's pid. The words say "if": another failure of an older hub's spawn
// reads the same.
func UpgradeRemedy(err error, hello protocol.HubHelloResult) string {
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Data.Code != protocol.CodeUnavailable || perr.Data.Reason != protocol.ReasonSpawnFailed {
		return ""
	}
	ep := hello.Endpoint
	if ep.PID <= 0 || ep.CrazeVersion == "" || ep.CrazeVersion == version.Version || strings.Contains(perr.Message, hubReplacedMark) {
		return ""
	}
	return fmt.Sprintf("the hub (pid %d) runs craze %s and this is craze %s: if craze was upgraded, stop that hub with kill %d (no session ends) and run this again",
		ep.PID, ep.CrazeVersion, version.Version, ep.PID)
}
