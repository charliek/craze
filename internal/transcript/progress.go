package transcript

// Progress is what the main transcript says the session is doing and last
// said (plan 030 §3.10): the engine's sessions.list row facts Doing and
// LastReply are read from it (engine.RowFacts), so a list shows what a client
// folding the same stream would draw. Its strings are the entries' own, whole
// and unsanitised past what the fold already did: the row's one-line rule is
// the engine's.
type Progress struct {
	// Tool is the title of the most recently started tool of the current
	// turn that is still running — status pending or in_progress — its name
	// when it has no title, and "" when none is. The current turn is what
	// follows the last turn boundary: a prompt's user row (not an
	// interjection's), a foreign turn's opening note, or a replay's closing
	// one; a tool before it whose status never settled (a cancelled turn's)
	// is not running now.
	Tool string
	// Responding says the agent's text is streaming: the transcript's open
	// run is an assistant one.
	Responding bool
	// LastReply is the text of the last completed assistant entry — a run
	// that has closed; the tail of one longer than StreamText, led by "…" —
	// and "" when there is none.
	LastReply string
}

// Progress reads the main transcript's progress (Progress) under the model's
// lock: a scan back from the newest entry, which stops at the first
// completed assistant entry once the current turn's tools are behind it. It
// copies no entry and holds the lock for one scan, never a wait, so it may be
// called while the model folds on another goroutine — but not from inside the
// log's publishing boundary, where the fold itself holds the lock.
func (m *Model) Progress() Progress {
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.Main
	var p Progress
	ents := t.live()
	last := len(ents) - 1
	if last >= 0 && t.streamOpen && t.omittedRun == 0 && ents[last].Streaming && ents[last].Kind == KindAssistant {
		p.Responding = true
	}
	inTurn := true
	for i := last; i >= 0; i-- {
		e := ents[i]
		if inTurn {
			switch {
			case turnBoundary(e):
				inTurn = false
			case e.Kind == KindTool && p.Tool == "" && toolRunning(e):
				p.Tool = toolLabel(e)
			}
		}
		if p.LastReply == "" && e.Kind == KindAssistant && !e.Streaming {
			p.LastReply = e.Text
		}
		if !inTurn && p.LastReply != "" {
			break
		}
	}
	return p
}

// turnBoundary says e opens a turn — what follows it is that turn's: a
// prompt's user row, a foreign turn's opening note, or the note a replay
// closes with (what precedes it is history).
func turnBoundary(e *Entry) bool {
	switch e.Kind {
	case KindUser:
		return !e.Interject
	case KindNote:
		return e.Text == NoteForeignTurn || e.Text == NoteSubagentWake || e.Text == NoteRestored
	}
	return false
}

// toolRunning says a tool entry's call has not settled: its status is pending
// or in_progress, the two a client's working line counts as running.
func toolRunning(e *Entry) bool {
	return e.Tool != nil && (e.Tool.Status == "pending" || e.Tool.Status == "in_progress")
}

// toolLabel is a running tool's name for a row: its title, else its name,
// else its tool name.
func toolLabel(e *Entry) string {
	switch {
	case e.Tool.Title != "":
		return e.Tool.Title
	case e.Tool.Name != "":
		return e.Tool.Name
	}
	return e.Tool.ToolName
}
