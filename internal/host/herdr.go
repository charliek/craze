package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// herdrMetadataTimeout bounds each pane.report_metadata call on its own,
// separate from the Hub's SendTimeout for the state line (plan 015 §3.3).
const herdrMetadataTimeout = 300 * time.Millisecond

// Fixed identity herdr requires of a non-bundled reporter (plan 015 §3.3):
// source must not use the herdr: namespace (reserved for bundled
// integrations), and the reporter never sends agent_session_id.
const (
	herdrSource = "custom:craze"
	herdrAgent  = "craze"
)

// Herdr reports craze's state to herdr's per-pane control socket
// (pane.report_agent / pane.report_metadata / pane.release_agent).
//
// Report and Release are only ever called by one Hub worker goroutine, never
// concurrently with each other, so the last-sent bookkeeping below needs no
// lock of its own.
type Herdr struct {
	socket string
	pane   string

	// metadataAttempted is set the moment a report_metadata send is
	// attempted, success or not: Release uses it to decide whether tokens
	// need nulling out, since a lost metadata line must still be retried.
	metadataAttempted bool
	// lastProvider/lastModel are the tokens last delivered on an ok reply;
	// updated only then, so a failed send is retried on the next Report.
	lastProvider, lastModel string
}

// NewHerdrFromEnv builds a Herdr iff herdr's documented gate is met: HERDR_ENV
// is exactly "1", and HERDR_SOCKET_PATH and HERDR_PANE_ID are both non-empty
// (plan 015 §3.3). ok is false, with a nil *Herdr, otherwise.
func NewHerdrFromEnv(getenv func(string) string) (h *Herdr, ok bool) {
	if getenv("HERDR_ENV") != "1" {
		return nil, false
	}
	socket, pane := getenv("HERDR_SOCKET_PATH"), getenv("HERDR_PANE_ID")
	if socket == "" || pane == "" {
		return nil, false
	}
	return &Herdr{socket: socket, pane: pane}, true
}

// Name implements Reporter.
func (h *Herdr) Name() string { return "herdr" }

// herdrStateOf maps a craze Kind onto herdr's state vocabulary. Failed maps
// onto blocked, same as Blocked: a user decision (plan 015 pins), since
// herdr has no separate failed state.
func herdrStateOf(k Kind) string {
	switch k {
	case Working:
		return "working"
	case Blocked, Failed:
		return "blocked"
	default:
		return "idle"
	}
}

type herdrRequest struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type herdrReportAgentParams struct {
	PaneID  string  `json:"pane_id"`
	Source  string  `json:"source"`
	Agent   string  `json:"agent"`
	State   string  `json:"state"`
	Seq     uint64  `json:"seq"`
	Message *string `json:"message,omitempty"`
}

type herdrMetadataParams struct {
	PaneID string      `json:"pane_id"`
	Source string      `json:"source"`
	Agent  string      `json:"agent"`
	Tokens herdrTokens `json:"tokens"`
}

type herdrTokens struct {
	Provider herdrToken `json:"provider"`
	Model    herdrToken `json:"model"`
}

// herdrToken marshals an empty string as JSON null: herdr normalises an
// empty value to a clear anyway, but plan 015 §3.3 sends the nulls
// explicitly, and both an unset token and an explicit clear use this type.
type herdrToken string

func (t herdrToken) MarshalJSON() ([]byte, error) {
	if t == "" {
		return []byte("null"), nil
	}
	return json.Marshal(string(t))
}

type herdrReleaseParams struct {
	PaneID string `json:"pane_id"`
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Seq    uint64 `json:"seq"`
}

type herdrReply struct {
	ID     string `json:"id"`
	Result *struct {
		Type string `json:"type"`
	} `json:"result"`
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// call sends one request and reads its one reply line: herdr always answers
// with exactly one line per request (plan 015 §3.3's "Wire" bullet), so
// accept is always done after the first line. A reply is only accepted if
// its id matches the request's and, on success, its result is exactly
// {"type":"ok"} — every one of pane.report_agent, pane.report_metadata and
// pane.release_agent answers ResponseResult::Ok{}, which serialises that
// way, in herdr's own source. Anything else (a mismatched id, a result with
// some other type, or neither result nor error) is an error, not a silent
// success: a wrong-but-truthy reply must not update lastProvider/lastModel
// or otherwise look like an acknowledged send.
func (h *Herdr) call(ctx context.Context, id, method string, params any) error {
	req := herdrRequest{ID: id, Method: method, Params: params}
	return roundTrip(ctx, h.socket, req, func(line []byte) (bool, error) {
		var reply herdrReply
		if err := json.Unmarshal(line, &reply); err != nil {
			return true, fmt.Errorf("decode reply: %w", err)
		}
		if reply.ID != id {
			return true, fmt.Errorf("reply id %q does not match request id %q", reply.ID, id)
		}
		if reply.Error != nil {
			return true, fmt.Errorf("herdr error %v: %s", reply.Error.Code, reply.Error.Message)
		}
		if reply.Result == nil {
			return true, errors.New("reply has neither result nor error")
		}
		if reply.Result.Type != "ok" {
			return true, fmt.Errorf("reply result type %q, want \"ok\"", reply.Result.Type)
		}
		return true, nil
	})
}

// Report sends s's state line, then, only on a provider/model change, a
// metadata line (plan 015 §3.3).
func (h *Herdr) Report(ctx context.Context, s Status, seq uint64) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	params := herdrReportAgentParams{
		PaneID: h.pane,
		Source: herdrSource,
		Agent:  herdrAgent,
		State:  herdrStateOf(s.Kind),
		Seq:    seq,
	}
	if s.Kind == Blocked || s.Kind == Failed {
		params.Message = &s.Message
	}
	if err := h.call(ctx, fmt.Sprintf("craze:%d", seq), "pane.report_agent", params); err != nil {
		return err
	}

	// The closing check before the next line: a Close racing this Report
	// must not let a metadata line land after the state line it followed
	// (plan 015 §3.2's release-is-last invariant starts here).
	if ctx.Err() != nil {
		return nil
	}

	if s.Provider == h.lastProvider && s.Model == h.lastModel {
		return nil
	}

	h.metadataAttempted = true
	mctx, cancel := context.WithTimeout(ctx, herdrMetadataTimeout)
	defer cancel()
	err := h.call(mctx, fmt.Sprintf("craze:%d:metadata", seq), "pane.report_metadata", herdrMetadataParams{
		PaneID: h.pane,
		Source: herdrSource,
		Agent:  herdrAgent,
		Tokens: herdrTokens{Provider: herdrToken(s.Provider), Model: herdrToken(s.Model)},
	})
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	h.lastProvider, h.lastModel = s.Provider, s.Model
	return nil
}

// Release nulls out any metadata this reporter ever sent, then always sends
// pane.release_agent as the final write (plan 015 §3.3): a nulls failure
// does not skip the release.
func (h *Herdr) Release(ctx context.Context, seq uint64) error {
	var errs []error
	if h.metadataAttempted {
		mctx, cancel := context.WithTimeout(ctx, herdrMetadataTimeout)
		err := h.call(mctx, fmt.Sprintf("craze:%d:metadata", seq), "pane.report_metadata", herdrMetadataParams{
			PaneID: h.pane,
			Source: herdrSource,
			Agent:  herdrAgent,
			Tokens: herdrTokens{}, // both empty -> both null
		})
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("metadata: %w", err))
		}
	}
	if err := h.call(ctx, fmt.Sprintf("craze:%d", seq), "pane.release_agent", herdrReleaseParams{
		PaneID: h.pane,
		Source: herdrSource,
		Agent:  herdrAgent,
		Seq:    seq,
	}); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
