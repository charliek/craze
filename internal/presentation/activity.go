// Package presentation describes transcript display choices shared by renderers.
package presentation

import (
	"fmt"
	"strings"

	"github.com/charliek/craze/internal/agent"
)

// Layout determines which adjacent completed tools can share a disclosure.
type Layout uint8

const (
	Grouped Layout = iota
	Compact
	Classic
)

func (l Layout) String() string {
	switch l {
	case Compact:
		return "Compact"
	case Classic:
		return "Classic"
	default:
		return "Grouped"
	}
}

type Activity uint8

const (
	Separate Activity = iota
	Read
	Search
	List
	Edit
)

// Classify uses the provider's normalized kind and, when available, its tool
// identity. A native read-kind call can also poll a shell or sub-agent.
func Classify(layout Layout, t *agent.ToolEvent) Activity {
	if layout == Classic || t == nil || t.Status != "completed" || t.IsTask() {
		return Separate
	}
	if o := t.Output; o != nil && o.ExitCode != nil && *o.ExitCode != 0 {
		return Separate
	}
	if t.Kind == "edit" && layout == Compact && compactEdit(t) {
		return Edit
	}
	if t.Edited != nil || len(t.Diffs) > 0 {
		return Separate
	}
	name := strings.ToLower(t.ToolName)
	switch t.Kind {
	case "read", "search", "list":
		switch name {
		case "ls", "list", "list_dir", "list_directory", "list_files", "listfiles":
			return List
		case "read", "readfile", "read_file":
			return Read
		case "grep", "glob", "search", "grep_search", "search_files", "ripgrep", "websearch", "web_search":
			return Search
		case "":
			switch t.Kind {
			case "read":
				return Read
			case "search":
				return Search
			case "list":
				return List
			}
		}
	}
	return Separate
}

// A compact edit must have evidence of an ordinary workspace change. Keep
// warnings about a truncated diff, out-of-workspace paths and plan edits visible.
func compactEdit(t *agent.ToolEvent) bool {
	if e := t.Edited; e != nil {
		if e.Truncated || e.PlanFile != "" || len(e.OutsideFiles) != 0 {
			return false
		}
	}
	for _, diff := range t.Diffs {
		if diff.Truncated {
			return false
		}
	}
	return len(t.Diffs) != 0 || (t.Edited != nil && len(t.Edited.Files) != 0)
}

type Counts [Edit + 1]int

func (c Counts) Summary() string {
	labels := [...]string{"", "read", "search", "listing", "edit"}
	plural := [...]string{"", "reads", "searches", "listings", "edits"}
	var parts []string
	for kind := Read; kind <= Edit; kind++ {
		if n := c[kind]; n > 0 {
			unit := plural[kind]
			if n == 1 {
				unit = labels[kind]
			}
			parts = append(parts, fmt.Sprintf("%d %s", n, unit))
		}
	}
	group := "Explored"
	if c[Edit] > 0 {
		group = "Activity"
	}
	return group + " · " + strings.Join(parts, ", ")
}
