package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/agent"
)

func TestRootRefusesNonTTY(t *testing.T) {
	var stdout bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(ee.msg, "refusing to start TUI on a non-tty") {
		t.Fatalf("msg %q", ee.msg)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout %q", stdout.String())
	}
}

func TestPromptUsage(t *testing.T) {
	t.Run("no text empty stdin", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("ask and plan", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetIn(&bytes.Buffer{})
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--ask", "--plan", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
		if !strings.Contains(ee.msg, "mutually exclusive") {
			t.Fatalf("msg %q", ee.msg)
		}
	})

	t.Run("bad permission decision", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--permission-decision", "maybe", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("workspace missing", func(t *testing.T) {
		cmd := NewRootCmd()
		cmd.SetOut(&bytes.Buffer{})
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetArgs([]string{"prompt", "--workspace", t.TempDir() + "/no-such-dir", "hi"})
		err := cmd.Execute()
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != 2 {
			t.Fatalf("got %v", err)
		}
	})
}

func TestDecisionKind(t *testing.T) {
	kind, err := decisionKind("allow-once")
	if err != nil || kind != "allow_once" {
		t.Fatalf("%q %v", kind, err)
	}
	kind, err = decisionKind("reject_once")
	if err != nil || kind != "reject_once" {
		t.Fatalf("%q %v", kind, err)
	}
}

func TestEventJSON(t *testing.T) {
	if _, ok := eventJSON(agent.Event{Type: agent.EventThought, Text: "secret"}); ok {
		t.Fatal("thoughts should not be serialized")
	}
	j, ok := eventJSON(agent.Event{Type: agent.EventText, Text: "hi"})
	if !ok || j.Type != "text" || j.Text != "hi" {
		t.Fatalf("%+v %v", j, ok)
	}
}

func TestPickPermissionKeepsUnusedDecisions(t *testing.T) {
	opts := []agent.PermissionOption{
		{OptionID: "opt-reject", Kind: "reject_once"},
	}
	id, rejected, rest, err := pickPermission(opts, []string{"allow-once"})
	if err != nil || !rejected || id != "opt-reject" {
		t.Fatalf("id=%q rejected=%v err=%v", id, rejected, err)
	}
	if len(rest) != 1 || rest[0] != "allow-once" {
		t.Fatalf("queue should keep unused allow-once, got %v", rest)
	}
}
