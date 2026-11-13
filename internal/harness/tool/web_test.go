package tool

import (
	"errors"
	"strings"
	"testing"
)

// TestKindFetchIsValid (plan 051 §A): a fetch-kind spec registers, its word is
// the one the TUI already draws ("fetch"), and a kind nobody declared still
// does not — the control that the check is still a check.
func TestKindFetchIsValid(t *testing.T) {
	spec := func(k Kind) Tool {
		f := newFake("webfetch", nil)
		f.spec.Kind, f.spec.ReadOnly = k, true
		return f
	}
	if _, err := validateTools([]Tool{spec(KindFetch)}); err != nil {
		t.Fatalf("a fetch-kind tool was refused: %v", err)
	}
	if KindFetch != "fetch" {
		t.Fatalf("KindFetch = %q, want fetch, the kind the TUI draws", KindFetch)
	}
	if _, err := validateTools([]Tool{spec("browse")}); err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("control: an undeclared kind registered (err %v)", err)
	}
}

// TestWebErrorIsItsText (plan 051 §B): a WebError reads as its Text alone,
// and is found through a wrap by errors.As with its class intact.
func TestWebErrorIsItsText(t *testing.T) {
	var err error = &WebError{Class: ClassTimeout, Text: "Web search timed out"}
	if err.Error() != "Web search timed out" {
		t.Fatalf("Error() = %q", err)
	}
	var we *WebError
	if !errors.As(errors.Join(errors.New("x"), err), &we) || we.Class != ClassTimeout {
		t.Fatalf("errors.As = %+v", we)
	}
	var env Env
	if env.Web != nil {
		t.Fatal("a zero Env has a web service")
	}
}
