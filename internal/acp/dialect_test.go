package acp

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestDialDefaultsToCursor pins the U1 contract: Dial speaks cursor, and
// Spawn without a dialect does too.
func TestDialDefaultsToCursor(t *testing.T) {
	clientR, serverW := io.Pipe()
	serverR, clientW := io.Pipe()
	t.Cleanup(func() {
		_ = clientR.Close()
		_ = clientW.Close()
		_ = serverR.Close()
		_ = serverW.Close()
	})
	c := Dial(clientR, clientW)
	t.Cleanup(func() { _ = c.Close() })
	if c.Dialect() != DialectCursor {
		t.Fatalf("dialect %q", c.Dialect())
	}
	if DialWithDialect(clientR, clientW, DialectGrok).Dialect() != DialectGrok {
		t.Fatal("explicit dialect must stick")
	}
}

// TestResolveBinaryCandidates pins the provider lookup: candidates are PATH
// names only, so a grok lookup can never resolve a stray "agent".
func TestResolveBinaryCandidates(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"grok", "agent"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("CRAZE_AGENT_BIN", "")
	if got, err := ResolveBinaryCandidates("", []string{"grok"}); err != nil || got != filepath.Join(dir, "grok") {
		t.Fatalf("grok = %q, %v", got, err)
	}
	// The legacy default still finds cursor-agent first and agent second.
	cursorDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cursorDir, "agent"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", cursorDir)
	if got, err := ResolveBinary(""); err != nil || got != filepath.Join(cursorDir, "agent") {
		t.Fatalf("default = %q, %v", got, err)
	}
	explicit := filepath.Join(dir, "agent")
	if got, err := ResolveBinaryCandidates(explicit, []string{"grok"}); err != nil || got != explicit {
		t.Fatalf("explicit = %q, %v", got, err)
	}
	withEnv := filepath.Join(dir, "grok")
	t.Setenv("CRAZE_AGENT_BIN", withEnv)
	if got, err := ResolveBinaryCandidates("", []string{"cursor-agent", "agent"}); err != nil || got != withEnv {
		t.Fatalf("env override = %q, %v", got, err)
	}
	t.Setenv("CRAZE_AGENT_BIN", "")
	t.Setenv("PATH", t.TempDir())
	if _, err := ResolveBinaryCandidates("", []string{"grok"}); err == nil {
		t.Fatal("missing binary must error")
	}
}

// TestAuthenticateSendsMethodAndMeta pins the U1 auth shape: cursor sends
// cursor_login with no _meta, grok sends its method with headless meta.
func TestAuthenticateSendsMethodAndMeta(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		meta   map[string]any
	}{
		{"cursor", AuthCursorLogin, nil},
		{"grok-key", AuthXAIAPIKey, map[string]any{"headless": true}},
		{"grok-token", AuthCachedToken, map[string]any{"headless": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clientR, serverW := io.Pipe()
			serverR, clientW := io.Pipe()
			t.Cleanup(func() {
				_ = clientR.Close()
				_ = clientW.Close()
				_ = serverR.Close()
				_ = serverW.Close()
			})
			client := Dial(clientR, clientW)
			t.Cleanup(func() { _ = client.Close() })
			srv := NewConn(serverR, serverW)
			got := make(chan json.RawMessage, 1)
			srv.SetRequestHandler(func(msg *Message) {
				if msg.Method == MethodAuthenticate {
					got <- append(json.RawMessage(nil), msg.Params...)
					_ = srv.Reply(msg.ID, map[string]any{})
					return
				}
				_ = srv.ReplyErr(msg.ID, MethodNotFound(msg.Method))
			})
			srv.Start()
			if err := client.Authenticate(t.Context(), tc.method, tc.meta); err != nil {
				t.Fatal(err)
			}
			var params AuthenticateParams
			if err := json.Unmarshal(<-got, &params); err != nil {
				t.Fatal(err)
			}
			if params.MethodID != tc.method {
				t.Fatalf("methodId %q", params.MethodID)
			}
			if tc.meta == nil && params.Meta != nil {
				t.Fatalf("cursor must send no _meta: %v", params.Meta)
			}
			if tc.meta != nil && params.Meta["headless"] != true {
				t.Fatalf("meta %v", params.Meta)
			}
		})
	}
}
