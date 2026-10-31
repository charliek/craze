package agent

import (
	"context"
	"errors"

	"github.com/charliek/craze/internal/acp"
	"github.com/charliek/craze/internal/chatgptauth"
	"github.com/charliek/craze/internal/errkind"
	"github.com/charliek/craze/internal/harness"
	"github.com/charliek/craze/internal/harness/modeltable"
	"github.com/charliek/craze/internal/harness/store"
)

// errInfoSentinels are the errors that say nothing of themselves, each with
// what it is, in the order an error is tested against them. The sign-in's own
// come first: a provider failure can carry one (harness.ProviderError.Unwrap),
// and it says more than the failure's class does.
var errInfoSentinels = []struct {
	sentinel error
	info     errkind.Info
}{
	{chatgptauth.ErrUsageLimited, errkind.New(errkind.Quota)},
	{chatgptauth.ErrSignedOut, errkind.New(errkind.Auth)},
	{chatgptauth.ErrSignInAgain, errkind.New(errkind.Auth)},
	{chatgptauth.ErrPlanUsageDisabled, errkind.New(errkind.Config)},
	{modeltable.ErrNotSignedIn, errkind.New(errkind.Auth)},
	{modeltable.ErrPlanUsageDisabled, errkind.New(errkind.Config)},
	{modeltable.ErrOtherAccount, errkind.New(errkind.Config)},
	{modeltable.ErrNothingFunded, errkind.New(errkind.Auth)},
	{harness.ErrNoAPIKey, errkind.New(errkind.Auth)},
	{harness.ErrUnknownModel, errkind.New(errkind.Config)},
	{harness.ErrResumeModel, errkind.New(errkind.Config)},
	{harness.ErrProfileMismatch, errkind.New(errkind.Config)},
	{harness.ErrUnknownMode, errkind.New(errkind.Config)},
	{harness.ErrStoredKeyFrozen, errkind.New(errkind.Config)},
	{harness.ErrEmptyPrompt, errkind.New(errkind.Prompt)},
	{harness.ErrEmptyStep, errkind.New(errkind.Provider)},
	{harness.ErrClosed, errkind.New(errkind.Stopped)},
	{acp.ErrClosed, errkind.New(errkind.Agent)},
	{acp.ErrAgentExited, errkind.New(errkind.Agent)},
	{context.Canceled, errkind.New(errkind.Interrupted)},
	// Refused because the agent's own turn holds the session: that turn's end
	// lets a fresh prompt through.
	{ErrPromptInFlight, errkind.Info{Kind: errkind.Agent, Retryable: true}},
	{ErrForeignTurn, errkind.Info{Kind: errkind.Agent, Retryable: true}},
}

// ErrorInfo is what err says it is, at the seam every turn's failure crosses
// with its type still known. A *RemoteError came through the codec and keeps
// what it said, nothing at all from a host that predates it: its class's
// sentinel (RemoteError.Is) must not make something up. Otherwise a sentinel of
// errInfoSentinels, else what its chain says of itself (errkind.Classified:
// harness.ProviderError, acp.RPCError), else the agent process having died under
// the turn, and anything else internal. nil is the zero Info.
func ErrorInfo(err error) errkind.Info {
	if err == nil {
		return errkind.Info{}
	}
	var remote *RemoteError
	if errors.As(err, &remote) && remote != nil {
		return remote.Info
	}
	for _, s := range errInfoSentinels {
		if errors.Is(err, s.sentinel) {
			return s.info
		}
	}
	if info, ok := errkind.Of(err); ok {
		return info
	}
	var exit *acp.ExitError
	if errors.As(err, &exit) && exit != nil {
		return errkind.New(errkind.Agent)
	}
	return errkind.New(errkind.Internal)
}

// unresumable are a load's refusals of the stored session itself: another attempt
// reads the same file and is refused the same way. store.ErrBusy is not one, its
// holder being free to let go.
var unresumable = []error{store.ErrCorrupt, store.ErrNoHeader, store.ErrNotResumable, store.ErrNewerTranscript, store.ErrBadSessionID}

// StartErrorKind is what a failed session start says it is: what its error says
// of itself when that is something another attempt would meet again —
// credentials, the model, the configuration; a stored session that cannot be
// loaded, which is internal — and otherwise that the agent or host did not start,
// which may go better next time.
func StartErrorKind(err error) errkind.Kind {
	kind := ErrorInfo(err).Kind
	switch kind {
	case errkind.Auth, errkind.Config, errkind.ModelNotFound, errkind.Quota, errkind.Workspace:
		return kind
	}
	for _, s := range unresumable {
		if errors.Is(err, s) {
			return errkind.Internal
		}
	}
	return errkind.StartFailed
}
