package agent

import "testing"

func TestSanitizeText(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"keeps newline and tab", "a\n\tb", "a\n\tb"},
		{"drops carriage return", "a\rb", "ab"},
		{"drops nul and bell", "a\x00b\x07c", "abc"},
		{"csi colour", "\x1b[31mred\x1b[0m", "red"},
		{"csi cursor move", "x\x1b[2Jy", "xy"},
		{"osc title bel", "\x1b]0;pwned\x07title", "title"},
		{"osc title st", "\x1b]0;pwned\x1b\\title", "title"},
		{"dcs", "\x1bPq;stuff\x1b\\ok", "ok"},
		{"apc", "\x1b_payload\x1b\\ok", "ok"},
		{"two byte escape", "\x1b(Bok", "ok"},
		{"lone escape at end", "ok\x1b", "ok"},
		{"del", "a\x7fb", "ab"},
		{"invalid utf8", "a\xffb", "a�b"},
		{"keeps valid utf8", "héllo ✓", "héllo ✓"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeText(tc.in); got != tc.want {
				t.Fatalf("sanitizeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeEveryToolField(t *testing.T) {
	s := newSession(Options{})
	esc := "\x1b]0;x\x07\x1b[31m"
	title := esc + "Edit File"
	tool, _ := s.mergeTool(toolDelta{
		id:    "id-1",
		title: &title,
		rawInput: mustJSON(map[string]any{
			"_toolName":   "task",
			"prompt":      esc + "run",
			"description": esc + "desc",
		}),
		hasRawInput: true,
		content: mustJSON([]map[string]any{{
			"type":    "diff",
			"path":    esc + "/tmp/a.go",
			"oldText": "a\n",
			"newText": esc + "b\n",
		}}),
		hasContent: true,
		rawOutput: mustJSON(map[string]any{
			"exitCode": 1,
			"stdout":   esc + "out",
			"stderr":   esc + "err",
			"content":  esc + "file",
		}),
		hasRawOutput: true,
		locations:    mustJSON([]map[string]any{{"path": esc + "/tmp/a.go"}}),
		hasLocations: true,
	})
	if tool.Title != "Edit File" || tool.Name != "Edit File" {
		t.Fatalf("title %q", tool.Title)
	}
	if tool.RawInput != "run" {
		t.Fatalf("rawInput %q", tool.RawInput)
	}
	if tool.ToolName != "task" {
		t.Fatalf("toolName %q", tool.ToolName)
	}
	if tool.Task == nil || tool.Task.Prompt != "run" || tool.Task.Description != "desc" {
		t.Fatalf("task %+v", tool.Task)
	}
	if tool.Output.Stdout != "out" || tool.Output.Stderr != "err" || tool.Output.Content != "file" {
		t.Fatalf("output %+v", tool.Output)
	}
	if tool.Output.StdoutHead != "out" || tool.Output.StderrHead != "err" {
		t.Fatalf("heads %+v", tool.Output)
	}
	if len(tool.Diffs) != 1 || tool.Diffs[0].Path != "/tmp/a.go" || tool.Diffs[0].NewText != "b\n" {
		t.Fatalf("diffs %+v", tool.Diffs)
	}
	if len(tool.Locations) != 1 || tool.Locations[0] != "/tmp/a.go" {
		t.Fatalf("locations %+v", tool.Locations)
	}
	// The id is a map key, never rendered, so it is kept verbatim.
	if tool.ID != "id-1" {
		t.Fatalf("id %q", tool.ID)
	}
}

func TestToolIDKeptVerbatimWithNewline(t *testing.T) {
	s := newSession(Options{})
	id := "call-abc-0\nfc_xyz"
	title := "Read File"
	tool, _ := s.mergeTool(toolDelta{id: id, title: &title})
	if tool.ID != id {
		t.Fatalf("id %q", tool.ID)
	}
	if got := s.Snapshot().Tools[0].ID; got != id {
		t.Fatalf("snapshot id %q", got)
	}
}
