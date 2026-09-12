package agent

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMergeToolKeepsOmittedTitle(t *testing.T) {
	s := newSession(Options{})
	title := "Shell"
	pending := "pending"
	ev, emit := s.mergeTool(toolDelta{id: "sh-1", title: &title, status: &pending})
	if !emit || ev.Title != "Shell" || ev.Name != "Shell" {
		t.Fatalf("create %+v emit=%v", ev, emit)
	}
	inProgress := "in_progress"
	ev, emit = s.mergeTool(toolDelta{id: "sh-1", status: &inProgress})
	if !emit {
		t.Fatal("status change should emit")
	}
	if ev.Title != "Shell" || ev.Name != "Shell" || ev.Status != "in_progress" {
		t.Fatalf("omitted title lost: %+v", ev)
	}
	snap := s.Snapshot()
	if len(snap.Tools) != 1 || snap.Tools[0].Title != "Shell" {
		t.Fatalf("snapshot %+v", snap.Tools)
	}
}

func TestDuplicateToolUpdateNoEmit(t *testing.T) {
	s := newSession(Options{})
	title := "Shell"
	pending := "pending"
	if _, emit := s.mergeTool(toolDelta{id: "a", title: &title, status: &pending}); !emit {
		t.Fatal("create should emit")
	}
	_, emit := s.mergeTool(toolDelta{id: "a", title: &title, status: &pending})
	if emit {
		t.Fatal("identical duplicate should not emit")
	}
	_, emit = s.mergeTool(toolDelta{id: "a"})
	if emit {
		t.Fatal("omitted-field duplicate should not emit")
	}
}

func TestStringifyRawInput(t *testing.T) {
	if got := stringifyRawInput(json.RawMessage(`{"prompt":"no","args":"also","command":"echo hi"}`)); got != "echo hi" {
		t.Fatalf("command key: %q", got)
	}
	if got := stringifyRawInput(json.RawMessage(`{"prompt":"p","args":"ls -la"}`)); got != "ls -la" {
		t.Fatalf("args key: %q", got)
	}
	if got := stringifyRawInput(json.RawMessage(`{"prompt":"think"}`)); got != "think" {
		t.Fatalf("prompt key: %q", got)
	}
	got := stringifyRawInput(json.RawMessage(`{"command":["echo","hi"]}`))
	if got != `{"command":["echo","hi"]}` {
		t.Fatalf("compact: %q", got)
	}
	long := strings.Repeat("a", 508) + "éxxxx"
	got = stringifyRawInput(json.RawMessage(`{"command":"` + long + `"}`))
	if len(got) > rawInputCap {
		t.Fatalf("cap %d", len(got))
	}
	if !strings.HasSuffix(got, ellipsis) || !utf8.ValidString(got) {
		t.Fatalf("utf8 cap %q", got)
	}
}

func TestContentTextDropsNonTextAndTails(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"diff","path":"a.go","newText":"x"},
		{"type":"content","content":{"type":"image","data":"xx","mimeType":"image/png"}},
		{"type":"content","content":{"type":"text","text":"hello"}},
		{"type":"terminal","terminalId":"t1"},
		{"type":"content","content":{"type":"text","text":" world"}}
	]`)
	if got := contentText(raw); got != "hello world" {
		t.Fatalf("got %q", got)
	}
	body := strings.Repeat("z", contentTextCap+50)
	got := contentText(json.RawMessage(`[{"type":"content","content":{"type":"text","text":"` + body + `"}}]`))
	if len(got) > contentTextCap {
		t.Fatalf("cap %d", len(got))
	}
	if !strings.HasPrefix(got, ellipsis) || !utf8.ValidString(got) {
		t.Fatalf("tail %q", got)
	}
	if !strings.HasSuffix(got, "z") {
		t.Fatal("expected tail of content")
	}
}

func TestLocationPathsCap(t *testing.T) {
	var items []map[string]string
	for i := 0; i < 10; i++ {
		items = append(items, map[string]string{"path": "f" + string(rune('0'+i)) + ".go"})
	}
	raw, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	got := locationPaths(raw)
	if len(got) != locationsCap {
		t.Fatalf("len %d", len(got))
	}
	if got[0] != "f0.go" || got[7] != "f7.go" {
		t.Fatalf("%v", got)
	}
}
