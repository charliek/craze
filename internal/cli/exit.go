package cli

import (
	"fmt"

	"github.com/charliek/craze/internal/errkind"
)

type exitError struct {
	code int
	msg  string
	// cause is what the exit is about, when a caller needs more than the
	// message: resolveLoad's refusal of a --continue another craze holds
	// carries the *rundir.HeldError, which runTUI attaches through (SQ16).
	// nil for every other exit.
	cause error
	// kind is what the failure was, as a waiting command's --json says it (and
	// a usage error's under --json). "" is internal at exit 1 and usage at exit
	// 2 (ErrorInfo); TestInternalFailureSitesAreListed lists the exit-1 sites
	// that leave it so.
	kind errkind.Kind
}

func (e *exitError) Error() string {
	if e == nil {
		return ""
	}
	return e.msg
}

// Unwrap is the exit's cause, so errors.As finds it.
func (e *exitError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func exitf(code int, format string, args ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, args...)}
}

func usagef(format string, args ...any) error {
	return exitf(2, format, args...)
}

// kindf is an exit-1 failure of kind.
func kindf(kind errkind.Kind, format string, args ...any) error {
	return &exitError{code: 1, msg: fmt.Sprintf(format, args...), kind: kind}
}

// usageKindf is a usage error (exit 2) of a kind of its own: a prompt that is
// not there, a provider that is not one.
func usageKindf(kind errkind.Kind, format string, args ...any) error {
	return &exitError{code: 2, msg: fmt.Sprintf(format, args...), kind: kind}
}

// ErrorInfo is what the failure was (kind), or for one that does not say, its
// exit's default: usage at exit 2, internal otherwise.
func (e *exitError) ErrorInfo() errkind.Info {
	switch {
	case e.kind != "":
		return errkind.New(e.kind)
	case e.code == 2:
		return errkind.New(errkind.Usage)
	}
	return errkind.New(errkind.Internal)
}

// failureKind is what err says it was (an exitError, a submitError), internal
// when it says nothing.
func failureKind(err error) errkind.Kind {
	if info, ok := errkind.Of(err); ok {
		return info.Kind
	}
	return errkind.Internal
}
