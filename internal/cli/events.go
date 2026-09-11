package cli

import (
	"encoding/json"
	"io"

	"github.com/charliek/craze/internal/agent"
)

type jsonEvent struct {
	Type       string   `json:"type"`
	Text       string   `json:"text,omitempty"`
	Name       string   `json:"name,omitempty"`
	Status     string   `json:"status,omitempty"`
	ID         string   `json:"id,omitempty"`
	OptionIDs  []string `json:"optionIds,omitempty"`
	Tool       string   `json:"tool,omitempty"`
	StopReason string   `json:"stopReason,omitempty"`
	Message    string   `json:"message,omitempty"`
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

func eventJSON(ev agent.Event) (jsonEvent, bool) {
	switch ev.Type {
	case agent.EventText:
		return jsonEvent{Type: "text", Text: ev.Text}, true
	case agent.EventTool:
		j := jsonEvent{Type: "tool"}
		if ev.Tool != nil {
			j.Name = ev.Tool.Name
			j.Status = ev.Tool.Status
			j.ID = ev.Tool.ID
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

func writeErrorEvent(w io.Writer, err error) {
	if err == nil {
		return
	}
	_ = encodeEvent(w, agent.Event{Type: agent.EventError, Err: err})
}
