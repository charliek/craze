package tui

import (
	"strings"

	"github.com/charliek/craze/internal/presentation"
)

type transcriptLayout = presentation.Layout

const (
	layoutGrouped = presentation.Grouped
	layoutCompact = presentation.Compact
	layoutClassic = presentation.Classic
)

func (e *entry) activity(layout transcriptLayout) presentation.Activity {
	if e.kind != entryTool || e.local || e.retracted {
		return presentation.Separate
	}
	return presentation.Classify(layout, e.tool)
}

// activityRun consumes one separate entry or the maximal run starting here.
func activityRun(rows []*entry, layout transcriptLayout) (int, presentation.Counts) {
	var counts presentation.Counts
	for i, e := range rows {
		kind := e.activity(layout)
		if kind == presentation.Separate {
			return max(1, i), counts
		}
		counts[kind]++
	}
	return len(rows), counts
}

func (m *Model) entryRows(tr *pane, e *entry, key renderKey, expanded bool) []string {
	if e.needsRender(key) || e.renderedDetail != expanded {
		detail := key
		detail.expanded = expanded
		e.renderedDetail = expanded
		e.rendered = physicalLines(m.renderEntry(tr, e, detail))
		e.plain = nil
		e.renderedFor = key
		e.dirty = false
		tr.renders++
	}
	return e.rendered
}

func (e *entry) canDisclose() bool {
	switch e.kind {
	case entryThought:
		return e.text != ""
	case entryShell:
		return e.shell != nil && e.shell.done && strings.Count(strings.TrimRight(e.text, "\n"), "\n") >= shellPreviewLines
	case entryTool:
		t := e.tool
		if e.retracted || t == nil || t.IsTask() {
			return false
		}
		switch t.Kind {
		case "edit":
			for _, d := range t.Diffs {
				if d.Truncated {
					return false
				}
			}
			return len(t.Diffs) > 0
		case "execute":
			return t.Output != nil && (t.Output.Stdout != "" || t.Output.Stderr != "")
		case "read", "search", "list":
			return t.Output != nil && t.Output.Content != ""
		}
	}
	return false
}

// anchor translates a physical offset across a repaint. Usually the entry
// stays in the same span; a trim or middle removal can shift it. A member
// hidden by regrouping anchors to its group's summary.
func (x *rowIndex) anchor(old *rowIndex, offset int) int {
	s, i := old.find(offset)
	if i >= x.spans() || x.span(i).e != s.e {
		i = 0
		for i < x.spans() && x.span(i).e != s.e {
			i++
		}
		if i == x.spans() {
			return offset
		}
	}
	next := x.span(i)
	if len(next.rows) == 0 && next.groupHead != nil {
		for i > 0 && next.e != next.groupHead {
			i--
			next = x.span(i)
		}
		return next.start
	}
	return next.start + min(offset-s.start, max(0, len(next.rows)-1))
}
