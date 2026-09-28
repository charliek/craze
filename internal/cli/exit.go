package cli

import "fmt"

type exitError struct {
	code int
	msg  string
	// cause is what the exit is about, when a caller needs more than the
	// message: resolveLoad's refusal of a --continue another craze holds
	// carries the *rundir.HeldError, which runTUI attaches through (SQ16).
	// nil for every other exit.
	cause error
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
