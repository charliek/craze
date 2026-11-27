package cli

import "github.com/charliek/craze/internal/agent"

// Before session.prompt replies, its started event identifies the review by
// prompt text. Once the reply arrives, the waiter has an authoritative turn ID.
// A provider wake can temporarily own the stream inside the turn's bracket.
type reviewProgress struct {
	prompt, turn, foreign string
	matchesPrompt         bool
}

func (p *reviewProgress) accepts(target waitTarget, ev agent.Event) bool {
	if ev.Replayed {
		return false
	}
	if ft := ev.ForeignTurn; ev.Type == agent.EventForeignTurn && ft != nil {
		if ft.Running {
			p.foreign = ft.ID
		} else if ft.ID == p.foreign {
			p.foreign = ""
		}
		return false
	}
	if tu := ev.Turn; ev.Type == agent.EventTurn && tu != nil {
		switch tu.Phase {
		case agent.TurnStarted:
			p.turn, p.matchesPrompt = tu.ID, tu.Text == p.prompt
			return p.owns(target)
		case agent.TurnEnded:
			ours := p.turn == tu.ID && p.owns(target)
			if p.turn == tu.ID {
				p.turn, p.matchesPrompt = "", false
			}
			return ours
		}
	}
	if p.foreign != "" || !p.owns(target) {
		return false
	}
	switch ev.Type {
	case agent.EventText, agent.EventThought, agent.EventTool, agent.EventTodos, agent.EventSubagent, agent.EventPlan, agent.EventQuestion, agent.EventPermission, agent.EventAsk, agent.EventDone, agent.EventError, agent.EventStepRetry, agent.EventCompaction:
		return true
	default:
		return false
	}
}

func (p *reviewProgress) owns(target waitTarget) bool {
	if target.kind == targetTurn {
		return p.turn != "" && p.turn == target.id
	}
	return p.turn != "" && p.matchesPrompt
}
