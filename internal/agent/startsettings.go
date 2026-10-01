package agent

import (
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/journal"
)

// The start settings (plan 032 §3.11, P6): --effort and --fast/--no-fast
// (Options.Effort, Options.Fast), which a session applies at the end of its
// own Start — after --model, so against the catalog of the model it starts
// on, and before Start returns. That is what keeps any prompt from overtaking
// them: the engine admits nothing until Session.Start has returned
// (engine.Start → Started), so a first prompt — the TUI's, a dispatch's, a
// client's on a host — is refused or held until the settings are in. The
// live session sets them over ACP (live.go's startSettings), the native one
// on its harness (native.go's startSettings); what they match, and what is
// said when one cannot be applied, is here once for both.

// The journal diags the start settings leave.
const (
	// diagEffortUnmatched: --effort matched no value of the session's effort
	// select, or more than one, or the session offers no effort select.
	diagEffortUnmatched = "effort_unmatched"
	// diagFastUnmatched: --fast or --no-fast, and the session offers no fast
	// toggle that has the value asked for.
	diagFastUnmatched = "fast_unmatched"
	// diagSettingRefused: the provider refused a start setting's set; the
	// session started without it.
	diagSettingRefused = "start_setting_refused"
)

// Why effortValue found no value, as effort_unmatched's "reason".
const (
	effortUnoffered = "unoffered" // the session has no effort select
	effortNone      = "none"      // no value matches
	effortAmbiguous = "ambiguous" // more than one value matches
)

// effortValue is want's value on opt, the session's effort select
// (EffortOption): the value whose id is want exactly, else the one whose id
// is want in any case, else the one whose name is — the first rule that
// matches anything decides, and it must match exactly one value. why is ""
// for a match and says otherwise why there is none (effortUnoffered,
// effortNone, effortAmbiguous); an ambiguous rule never falls through to a
// looser one.
func effortValue(opt *ConfigOption, want string) (value, why string) {
	if opt == nil {
		return "", effortUnoffered
	}
	rules := []func(SelectValue) bool{
		func(v SelectValue) bool { return v.Value == want },
		func(v SelectValue) bool { return strings.EqualFold(v.Value, want) },
		func(v SelectValue) bool { return strings.EqualFold(strings.TrimSpace(v.Name), want) },
	}
	for _, match := range rules {
		var found []string
		for _, v := range opt.SelectValues {
			if match(v) {
				found = append(found, v.Value)
			}
		}
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], ""
		default:
			return "", effortAmbiguous
		}
	}
	return "", effortNone
}

// fastValue is the value --fast (on) or --no-fast (!on) sets on opt, the
// session's fast toggle (FastOption), read the one way the toggle is read
// everywhere (FastOnOff); false when the session offers no toggle, or one
// with no value that means off for --no-fast.
func fastValue(opt *ConfigOption, on bool) (string, bool) {
	off, onValue, ok := FastOnOff(opt)
	switch {
	case !ok:
		return "", false
	case on:
		return onValue, true
	case off == "":
		return "", false
	}
	return off, true
}

// fastFlag is how a start setting's fast value is named to the user: the
// flag that asked for it.
func fastFlag(on bool) string {
	if on {
		return "--fast"
	}
	return "--no-fast"
}

// startNotes is where a session says what its start settings could not do:
// one diag in its journal, and one line on its diag lane (say), which the
// session prefixes as its other notes are. A note is never written under the
// session's lock (plan 020 §3.5).
type startNotes struct {
	log *EventLog
	say func(string)
}

// effortUnmatched is --effort skipped: want matched nothing on opt, or more
// than one value (why, effortValue's).
func (n startNotes) effortUnmatched(want, why string, opt *ConfigOption) {
	fields := map[string]any{"effort": want, "reason": why}
	var line string
	switch why {
	case effortUnoffered:
		line = fmt.Sprintf("--effort %q: this session offers no effort setting; it starts without one", sanitizeLine(want))
	default:
		values := make([]string, 0, len(opt.SelectValues))
		for _, v := range opt.SelectValues {
			values = append(values, v.Value)
		}
		fields["option"], fields["values"] = opt.ID, values
		shown := make([]string, len(values))
		for i, v := range values {
			shown[i] = sanitizeLine(v)
		}
		what := "none"
		if why == effortAmbiguous {
			what = "more than one"
		}
		line = fmt.Sprintf("--effort %q matches %s of this session's efforts (%s); it starts at its own", sanitizeLine(want), what, strings.Join(shown, ", "))
	}
	n.log.Note(journal.DiagNote{Kind: diagEffortUnmatched, Fields: fields})
	n.say(line)
}

// fastUnmatched is --fast or --no-fast skipped: the session has no fast
// toggle with that value.
func (n startNotes) fastUnmatched(on bool) {
	n.log.Note(journal.DiagNote{Kind: diagFastUnmatched, Fields: map[string]any{"fast": on}})
	n.say(fastFlag(on) + ": this session offers no fast setting; it starts without one")
}

// refused is a start setting the provider refused: setting is "effort" or
// "fast", id the option it was sent to and value what was sent; the session
// starts without it.
func (n startNotes) refused(setting, flag, id, value string, err error) {
	n.log.Note(journal.DiagNote{Kind: diagSettingRefused, Fields: map[string]any{
		"setting": setting, "option": id, "value": value, "error": err.Error(),
	}})
	n.say(fmt.Sprintf("%s was refused, and the session starts without it: %s", flag, sanitizeLine(err.Error())))
}
