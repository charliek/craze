package agent

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/charliek/craze/internal/acp"
)

const (
	rawInputCap    = 512
	contentTextCap = 2048
	locationsCap   = 8
	ellipsis       = "…"
)

type toolDelta struct {
	id           string
	title        *string
	kind         *string
	status       *string
	rawInput     json.RawMessage
	hasRawInput  bool
	content      json.RawMessage
	hasContent   bool
	locations    json.RawMessage
	hasLocations bool
}

func (s *session) mergeTool(d toolDelta) (ToolEvent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tools == nil {
		s.tools = make(map[string]ToolEvent)
	}
	prev, exists := s.tools[d.id]
	out := prev
	if !exists {
		out.ID = d.id
		s.toolOrder = append(s.toolOrder, d.id)
	}
	if d.title != nil {
		out.Title = *d.title
		out.Name = *d.title
	}
	if d.kind != nil {
		out.Kind = *d.kind
	}
	if d.status != nil {
		out.Status = *d.status
	}
	if d.hasRawInput {
		out.RawInput = stringifyRawInput(d.rawInput)
	}
	if d.hasContent {
		out.ContentText = contentText(d.content)
	}
	if d.hasLocations {
		out.Locations = locationPaths(d.locations)
	}
	changed := !exists ||
		out.Status != prev.Status ||
		out.Title != prev.Title ||
		out.Kind != prev.Kind ||
		out.RawInput != prev.RawInput ||
		out.ContentText != prev.ContentText
	s.tools[d.id] = out
	return cloneTool(out), changed
}

func cloneTool(t ToolEvent) ToolEvent {
	if t.Locations != nil {
		t.Locations = append([]string(nil), t.Locations...)
	}
	return t
}

func snapshotTools(order []string, byID map[string]ToolEvent) []ToolEvent {
	if len(order) == 0 {
		return nil
	}
	out := make([]ToolEvent, 0, len(order))
	for _, id := range order {
		if t, ok := byID[id]; ok {
			out = append(out, cloneTool(t))
		}
	}
	return out
}

func cloneConfig(in []ConfigOption) []ConfigOption {
	if in == nil {
		return nil
	}
	out := make([]ConfigOption, len(in))
	for i, c := range in {
		out[i] = c
		if c.SelectValues != nil {
			out[i].SelectValues = append([]SelectValue(nil), c.SelectValues...)
		}
	}
	return out
}

func presentJSON(raw json.RawMessage) (json.RawMessage, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, false
	}
	return raw, true
}

func stringifyRawInput(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, key := range []string{"command", "args", "prompt"} {
			v, ok := obj[key]
			if !ok {
				continue
			}
			var s string
			if err := json.Unmarshal(v, &s); err == nil {
				return truncateUTF8(s, rawInputCap)
			}
		}
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return truncateUTF8(string(raw), rawInputCap)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return truncateUTF8(string(raw), rawInputCap)
	}
	return truncateUTF8(string(b), rawInputCap)
}

func contentText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var items []acp.ToolContent
	if err := json.Unmarshal(raw, &items); err != nil {
		return ""
	}
	var b []byte
	for _, it := range items {
		if it.Type != "content" || it.Content == nil || it.Content.Type != "text" {
			continue
		}
		b = append(b, it.Content.Text...)
	}
	return tailUTF8(string(b), contentTextCap)
}

func locationPaths(raw json.RawMessage) []string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var items []acp.ToolCallLocation
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.Path == "" {
			continue
		}
		out = append(out, it.Path)
		if len(out) >= locationsCap {
			break
		}
	}
	return out
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	keep := max - len(ellipsis)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + ellipsis
}

func tailUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	keep := max - len(ellipsis)
	if keep < 0 {
		keep = 0
	}
	start := len(s) - keep
	if start < 0 {
		start = 0
	}
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return ellipsis + s[start:]
}
