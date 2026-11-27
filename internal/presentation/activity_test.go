package presentation

import (
	"testing"

	"github.com/charliek/craze/internal/agent"
)

func TestClassifyCompletedActivity(t *testing.T) {
	code := 3
	for _, tc := range []struct {
		name string
		tool agent.ToolEvent
		want Activity
	}{
		{"read", agent.ToolEvent{Kind: "read", Status: "completed"}, Read},
		{"grep", agent.ToolEvent{Kind: "search", ToolName: "Grep", Status: "completed"}, Search},
		{"directory", agent.ToolEvent{Kind: "read", ToolName: "list_dir", Status: "completed"}, List},
		{"session jump", agent.ToolEvent{Kind: "read", ToolName: "ExitPlanMode", Status: "completed"}, Separate},
		{"shell poll", agent.ToolEvent{Kind: "read", ToolName: "bash_output", Status: "completed"}, Separate},
		{"child poll", agent.ToolEvent{Kind: "read", ToolName: "agent_output", Status: "completed"}, Separate},
		{"outcome missing", agent.ToolEvent{Kind: "read"}, Separate},
		{"running", agent.ToolEvent{Kind: "search", Status: "in_progress"}, Separate},
		{"failed", agent.ToolEvent{Kind: "read", Status: "failed"}, Separate},
		{"nonzero exit", agent.ToolEvent{Kind: "read", Status: "completed", Output: &agent.ToolOutput{ExitCode: &code}}, Separate},
		{"edited", agent.ToolEvent{Kind: "read", Status: "completed", Edited: &agent.ToolEdited{}}, Separate},
		{"diff", agent.ToolEvent{Kind: "search", Status: "completed", Diffs: []agent.ToolDiff{{}}}, Separate},
		{"subagent", agent.ToolEvent{Kind: "search", Status: "completed", Task: &agent.TaskInfo{}}, Separate},
		{"shell", agent.ToolEvent{Kind: "execute", Status: "completed"}, Separate},
		{"edit", agent.ToolEvent{Kind: "edit", Status: "completed", Edited: &agent.ToolEdited{Files: []string{"file.go"}}}, Edit},
		{"unconfirmed edit", agent.ToolEvent{Kind: "edit", Status: "completed"}, Separate},
		{"truncated edit", agent.ToolEvent{Kind: "edit", Status: "completed", Diffs: []agent.ToolDiff{{Truncated: true}}}, Separate},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(Compact, &tc.tool); got != tc.want {
				t.Errorf("compact: got %v, want %v", got, tc.want)
			}
			if got := Classify(Classic, &tc.tool); got != Separate {
				t.Error("classic grouped a tool")
			}
			want := tc.want
			if want == Edit {
				want = Separate
			}
			if got := Classify(Grouped, &tc.tool); got != want {
				t.Errorf("grouped: got %v, want %v", got, want)
			}
		})
	}
}
