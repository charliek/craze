package agent

import "testing"

func TestResolveMode(t *testing.T) {
	available := []string{"agent", "plan", "ask"}
	if id, ok := ResolveMode("plan", available); !ok || id != "plan" {
		t.Fatalf("plan -> %q %v", id, ok)
	}
	if id, ok := ResolveMode("architect", []string{"architect", "ask"}); !ok || id != "architect" {
		t.Fatalf("architect alias miss: %q %v", id, ok)
	}
	if id, ok := ResolveMode("plan", []string{"architect"}); !ok || id != "architect" {
		t.Fatalf("plan should map to architect, got %q %v", id, ok)
	}
	if id, ok := ResolveMode("ask", available); !ok || id != "ask" {
		t.Fatalf("ask -> %q %v", id, ok)
	}
	if _, ok := ResolveMode("plan", []string{"ask"}); ok {
		t.Fatal("expected no match")
	}
}
