package agent

import (
	"path/filepath"

	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/websearch"
)

// The web tools' decision (plan 051 §C.3, F.2): whether a native session is
// offered websearch and webfetch, made once, at Open, from the caller's flag,
// config.toml, the test override, the remembered choice and whether anybody
// can be asked. One function, DecideWeb, makes it, for Open now and for the
// callers' early refusals later (the flags are plan 051 C6).

// WebSetting is what config.toml's `websearch` says (plan 051 §C.1).
type WebSetting int

const (
	// WebSettingAbsent: no `websearch` key, which defers to the remembered
	// choice. The zero value.
	WebSettingAbsent WebSetting = iota
	// WebSettingOff: `websearch = false`.
	WebSettingOff
	// WebSettingOn: a `[websearch]` table whose only key is a provider craze
	// has.
	WebSettingOn
	// WebSettingInvalid: anything else under `websearch`, or a config.toml
	// that cannot be read or parsed. Off, never absent, so a remembered allow
	// cannot outvote a mistyped off.
	WebSettingInvalid
)

// WebConfig is config.toml's `websearch` as the CLI read it
// (tui.ConfigWebSearch).
type WebConfig struct {
	Setting WebSetting
	// Provider is the table's provider, with WebSettingOn.
	Provider string
	// Why is the line said about a WebSettingInvalid reading: what was
	// wrong, the two valid forms, and that web search is off. It never holds
	// a value from the file.
	Why string
}

// WebFlag is --web-search / --no-web-search (plan 051 F.1): absent, on or
// off.
type WebFlag int

const (
	WebFlagAbsent WebFlag = iota
	WebFlagOn
	WebFlagOff
)

// WebFlagOrigin is where a flag came from (F.2, F.3): given on this command
// line, or recorded on the session's row when it was created.
type WebFlagOrigin int

const (
	WebFlagGiven WebFlagOrigin = iota
	WebFlagRecorded
)

// WebInputs is everything the decision reads.
type WebInputs struct {
	Flag   WebFlag
	Origin WebFlagOrigin
	// Attended is the session's final Interactive value, after any
	// unattended policy: a person is opening it.
	Attended bool
	// Native: the session's provider is native, the only one with web
	// tools.
	Native bool
	Config WebConfig
	// OverrideInvalid: websearch.TestEndpointEnv is set to something that is
	// not a loopback http URL.
	OverrideInvalid bool
	// Remembered is the consent card's remembered answer (websearch.json),
	// and RememberedWhy the line ReadChoice said about a file it could not
	// use.
	Remembered    websearch.Choice
	RememberedWhy string
	// CanAsk: an asker is wired and the question policy is ask, so the
	// consent question reaches a person rather than an automatic answer.
	// Not the same fact as Attended.
	CanAsk bool
}

// WebDecision is the decision: the session's web mode and provider, and the
// lines to say about it, in order (config.toml, the override, the remembered
// choice). The lines hold no values.
type WebDecision struct {
	Mode     harness.WebMode
	Provider string
	Notes    []string
}

// WebRefusal is a --web-search that cannot be honoured (plan 051 F.2): the
// session must not start.
type WebRefusal struct{ Msg string }

func (e *WebRefusal) Error() string { return e.Msg }

// The lines DecideWeb says.
const (
	webOverrideNote = websearch.TestEndpointEnv + " is not a loopback http URL, so web search is off"
	webRecordedNote = "this session was created with --web-search, but config.toml turns web search off, so it opens without web tools"
)

// DecideWeb is the web decision (plan 051 §C.3, F.2).
//
// Refusals come first, and only for --web-search: a non-native provider,
// config.toml off, invalid or unreadable, or an invalid test override. The
// one exception is a flag recorded on the session that a person is now
// resuming attended, which falls through to the list below instead (and opens
// off, with a line, when the config has since turned web search off).
//
// Then, first match wins:
//
//  1. --no-web-search: off.
//  2. config.toml off, invalid or unreadable: off.
//  3. an invalid test override: off, with a line.
//  4. --web-search: on, with the config table's provider if it names one.
//  5. a valid [websearch] table: on.
//  6. the remembered choice: on or off.
//  7. undecided: offered and asking when canAsk, withheld otherwise.
//
// A non-native session with no refusal is withheld. As a backstop, a flag
// given now, or recorded and reloaded unattended, that would open anything
// but on is refused.
func DecideWeb(in WebInputs) (WebDecision, error) {
	resumed := in.Flag == WebFlagOn && in.Origin == WebFlagRecorded && in.Attended
	if in.Flag == WebFlagOn && !resumed {
		if why := webRefusal(in); why != "" {
			return WebDecision{}, &WebRefusal{Msg: webRefusalText(in.Origin, why)}
		}
	}
	if !in.Native {
		return WebDecision{}, nil
	}
	var d WebDecision
	if in.Config.Setting == WebSettingInvalid && in.Config.Why != "" {
		d.Notes = append(d.Notes, in.Config.Why)
	}
	switch {
	case in.Flag == WebFlagOff:
	case in.Config.Setting == WebSettingOff || in.Config.Setting == WebSettingInvalid:
		if resumed {
			d.Notes = append(d.Notes, webRecordedNote)
		}
	case in.OverrideInvalid:
		d.Notes = append(d.Notes, webOverrideNote)
	case in.Flag == WebFlagOn:
		d.Mode, d.Provider = harness.WebOn, websearch.DefaultProvider
		if in.Config.Setting == WebSettingOn {
			d.Provider = in.Config.Provider
		}
	case in.Config.Setting == WebSettingOn:
		d.Mode, d.Provider = harness.WebOn, in.Config.Provider
	default:
		if in.RememberedWhy != "" {
			d.Notes = append(d.Notes, in.RememberedWhy)
		}
		switch {
		case in.Remembered.Provider != "":
			d.Mode, d.Provider = harness.WebOn, in.Remembered.Provider
		case in.Remembered.Disabled:
		case in.CanAsk:
			d.Mode, d.Provider = harness.WebAsking, websearch.DefaultProvider
		}
	}
	if in.Flag == WebFlagOn && !resumed && d.Mode != harness.WebOn {
		return WebDecision{}, &WebRefusal{Msg: webRefusalText(in.Origin, "web search would not be on")}
	}
	return d, nil
}

// webRefusal is why --web-search cannot be honoured, "" when it can.
func webRefusal(in WebInputs) string {
	switch {
	case !in.Native:
		return "web search is a native provider feature"
	case in.Config.Setting == WebSettingOff:
		return "config.toml turns web search off (websearch = false)"
	case in.Config.Setting == WebSettingInvalid:
		return in.Config.Why
	case in.OverrideInvalid:
		return webOverrideNote
	}
	return ""
}

func webRefusalText(origin WebFlagOrigin, why string) string {
	if origin == WebFlagRecorded {
		return "this session was created with --web-search, which cannot be honoured: " + why
	}
	return "--web-search cannot be honoured: " + why
}

// decideWeb fills hopts.Web from DecideWeb (plan 051 §C.3), unless the seam
// already set it, and injects the web service's endpoint (WebEndpoint, read
// from craze's own environment through websearch.Override like
// AnthropicAPIBase, and left to the seam's last word). It runs after the
// asker is settled, since whether anybody can be asked is half of canAsk. The
// lines go to s.note; they hold no values. hopts.Getenv is the sealed reader,
// the one the session's keys are resolved from.
func (s *nativeSession) decideWeb(hopts *harness.Options) error {
	if hopts.WebEndpoint == nil {
		endpoint, err := websearch.Override(hopts.Getenv)
		hopts.WebEndpoint = func() (string, error) { return endpoint, err }
	}
	if hopts.Web.Mode != harness.WebWithheld || hopts.Web.Service != nil {
		return nil
	}
	_, overrideErr := hopts.WebEndpoint()
	canAsk := hopts.Asker != nil && EffectiveApproval(s.opts).Question == ApprovalAsk
	remembered, why := websearch.ReadChoice(filepath.Join(hopts.Home, websearch.StateFile))
	d, err := DecideWeb(WebInputs{
		// The flags are plan 051 C6's: until then there is never one.
		Flag:            WebFlagAbsent,
		Attended:        s.opts.Interactive,
		Native:          true,
		Config:          s.opts.WebSearch,
		OverrideInvalid: overrideErr != nil,
		Remembered:      remembered,
		RememberedWhy:   why,
		CanAsk:          canAsk,
	})
	if err != nil {
		return err
	}
	for _, n := range d.Notes {
		s.note(n)
	}
	hopts.Web = harness.WebState{Mode: d.Mode, Provider: d.Provider, CanAsk: canAsk}
	return nil
}
