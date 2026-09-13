package cli

import (
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/charliek/craze/internal/agent"
)

// jsonStreamCap is how much of a tool's stdout/stderr the JSON stream carries.
const jsonStreamCap = 2048

type jsonEvent struct {
	Type       string              `json:"type"`
	Text       string              `json:"text,omitempty"`
	Agent      string              `json:"agent,omitempty"`
	Name       string              `json:"name,omitempty"`
	Status     string              `json:"status,omitempty"`
	ID         string              `json:"id,omitempty"`
	Kind       string              `json:"kind,omitempty"`
	Title      string              `json:"title,omitempty"`
	OptionIDs  []string            `json:"optionIds,omitempty"`
	Tool       string              `json:"tool,omitempty"`
	Output     *jsonToolOutput     `json:"output,omitempty"`
	Diffs      []jsonDiff          `json:"diffs,omitempty"`
	Task       *jsonTask           `json:"task,omitempty"`
	Todos      []jsonTodo          `json:"todos,omitempty"`
	Auto       bool                `json:"auto,omitempty"`
	Answers    map[string][]string `json:"answers,omitempty"`
	Accepted   bool                `json:"accepted,omitempty"`
	StopReason string              `json:"stopReason,omitempty"`
	Message    string              `json:"message,omitempty"`
}

type jsonSubagent struct {
	Type         string `json:"type"`
	Event        string `json:"event"`
	ID           string `json:"id"`
	AttemptID    string `json:"attemptId,omitempty"`
	ParentID     string `json:"parentId,omitempty"`
	Status       string `json:"status,omitempty"`
	Description  string `json:"description,omitempty"`
	SubagentType string `json:"subagentType,omitempty"`
	Model        string `json:"model,omitempty"`
	ToolCallID   string `json:"toolCallId,omitempty"`
	DurationMs   int    `json:"durationMs,omitempty"`
	ToolCalls    int    `json:"toolCalls,omitempty"`
	Turns        int    `json:"turns,omitempty"`
	Tokens       int    `json:"tokens,omitempty"`
	Output       string `json:"output,omitempty"`
	Error        string `json:"error,omitempty"`
	Transcript   bool   `json:"transcript"`
}

type jsonToolOutput struct {
	ExitCode *int   `json:"exitCode,omitempty"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Content  string `json:"content,omitempty"`
}

type jsonDiff struct {
	Path      string `json:"path"`
	Added     int    `json:"added"`
	Removed   int    `json:"removed"`
	Truncated bool   `json:"truncated"`
}

type jsonTask struct {
	Description string `json:"description,omitempty"`
	Model       string `json:"model,omitempty"`
	AgentID     string `json:"agentId,omitempty"`
	DurationMs  int    `json:"durationMs,omitempty"`
	Status      string `json:"status,omitempty"`
}

type jsonTodo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

func encodeEvent(w io.Writer, ev agent.Event) error {
	j, ok := eventJSON(ev)
	if !ok {
		return nil
	}
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func eventJSON(ev agent.Event) (any, bool) {
	switch ev.Type {
	case agent.EventText:
		return jsonEvent{Type: "text", Text: ev.Text, Agent: ev.Agent}, true
	case agent.EventThought:
		return jsonEvent{Type: "thought", Text: ev.Text, Agent: ev.Agent}, true
	case agent.EventUser:
		return jsonEvent{Type: "user", Text: ev.Text, Agent: ev.Agent}, true
	case agent.EventSubagent:
		return subagentJSON(ev), ev.Subagent != nil
	case agent.EventTool:
		return toolJSON(ev), true
	case agent.EventTodos:
		j := jsonEvent{Type: "todos", Todos: make([]jsonTodo, 0, len(ev.Todos))}
		for _, t := range ev.Todos {
			j.Todos = append(j.Todos, jsonTodo{ID: t.ID, Content: t.Content, Status: t.Status})
		}
		return j, true
	case agent.EventPermission:
		j := jsonEvent{Type: "permission"}
		if ev.Permission != nil {
			j.Tool = ev.Permission.Tool
			ids := make([]string, 0, len(ev.Permission.Options))
			for _, o := range ev.Permission.Options {
				ids = append(ids, o.OptionID)
			}
			j.OptionIDs = ids
		}
		return j, true
	case agent.EventQuestion:
		j := jsonEvent{Type: "question"}
		if ev.Question != nil {
			j.ID = ev.Question.ID
			j.Title = ev.Question.Title
			j.Auto = ev.Question.Auto
			j.Answers = ev.Question.Answers
		}
		return j, true
	case agent.EventPlan:
		j := jsonEvent{Type: "plan"}
		if ev.Plan != nil {
			j.ID = ev.Plan.ID
			j.Name = ev.Plan.Name
			j.Auto = ev.Plan.Auto
			j.Accepted = ev.Plan.Accepted
		}
		return j, true
	case agent.EventMeta:
		// The session carries the new title on EventMeta; other meta updates
		// have nothing to report headlessly.
		if ev.Text == "" {
			return jsonEvent{}, false
		}
		return jsonEvent{Type: "title", Title: ev.Text}, true
	case agent.EventDone:
		return jsonEvent{Type: "done", StopReason: ev.StopReason}, true
	case agent.EventError:
		msg := ""
		if ev.Err != nil {
			msg = ev.Err.Error()
		}
		return jsonEvent{Type: "error", Message: msg}, true
	default:
		return jsonEvent{}, false
	}
}

func subagentJSON(ev agent.Event) jsonSubagent {
	j := jsonSubagent{Type: "subagent", Event: ev.SubagentChange}
	if ev.Subagent == nil {
		return j
	}
	a := ev.Subagent
	j.ID = a.ID
	j.AttemptID = a.AttemptID
	j.ParentID = a.ParentID
	j.Status = string(a.Status)
	j.Description = a.Description
	j.SubagentType = a.SubagentType
	j.Model = a.Model
	j.ToolCallID = a.ToolCallID
	j.DurationMs = a.DurationMs
	j.ToolCalls = a.ToolCalls
	j.Turns = a.Turns
	j.Tokens = a.TokensUsed
	j.Output = a.Output
	j.Error = a.Error
	j.Transcript = a.Transcript
	return j
}

func toolJSON(ev agent.Event) jsonEvent {
	j := jsonEvent{Type: "tool", Agent: ev.Agent}
	t := ev.Tool
	if t == nil {
		return j
	}
	j.Name = t.Name
	j.Status = t.Status
	j.ID = t.ID
	j.Kind = t.Kind
	j.Title = t.Title
	if t.Output != nil {
		j.Output = &jsonToolOutput{
			ExitCode: t.Output.ExitCode,
			Stdout:   headUTF8(t.Output.Stdout, jsonStreamCap),
			Stderr:   headUTF8(t.Output.Stderr, jsonStreamCap),
			Content:  headUTF8(t.Output.Content, jsonStreamCap),
		}
	}
	for _, d := range t.Diffs {
		j.Diffs = append(j.Diffs, jsonDiff{
			Path:      d.Path,
			Added:     d.Added,
			Removed:   d.Removed,
			Truncated: d.Truncated,
		})
	}
	if t.Task != nil {
		j.Task = &jsonTask{
			Description: t.Task.Description,
			Model:       t.Task.Model,
			AgentID:     t.Task.AgentID,
			DurationMs:  t.Task.DurationMs,
			Status:      string(t.Task.Status),
		}
	}
	return j
}

func headUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	keep := max
	for keep > 0 && !utf8.RuneStart(s[keep]) {
		keep--
	}
	return s[:keep]
}

func writeErrorEvent(w io.Writer, err error) {
	if err == nil {
		return
	}
	_ = encodeEvent(w, agent.Event{Type: agent.EventError, Err: err})
}
