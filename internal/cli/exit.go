package cli

import "fmt"

type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string {
	if e == nil {
		return ""
	}
	return e.msg
}

func exitf(code int, format string, args ...any) error {
	return &exitError{code: code, msg: fmt.Sprintf(format, args...)}
}

func usagef(format string, args ...any) error {
	return exitf(2, format, args...)
}
