package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charliek/craze/internal/version"
)

func TestVersionCommand(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewRootCmd()
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	if got != version.Version {
		t.Fatalf("version output %q, want %q", got, version.Version)
	}
}
