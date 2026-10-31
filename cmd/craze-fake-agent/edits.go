package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/charliek/craze/internal/acp"
)

// The edits scripts are the calls a waiting command's `files` is told from
// (internal/agent/edited.go), one of each way a call can end and each way a path
// can be spelled. The agent runs in the workspace, so os.Getwd is -C as craze
// spelled it, a symlink included.

// editCall is one tool_call, its paths only in the update after it opens and how it
// ended only in the one after that. name is a grok tool's name, and such a call opens
// with no kind or status (internal/agent/testdata/grok-edits.jsonl); "" is a call of
// cursor's, which opens with its kind, pending, and says pending again with its paths
// (a live turn's journal). An empty status never ends.
func (s *server) editCall(id, name, kind, status string, paths ...string) {
	opened := map[string]any{"sessionUpdate": "tool_call", "toolCallId": id, "title": "Edit"}
	told := map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "kind": kind}
	if name != "" {
		meta := map[string]any{"x.ai/tool": map[string]any{"name": name}}
		opened["_meta"], told["_meta"] = meta, meta
	} else {
		opened["kind"], opened["status"], told["status"] = kind, "pending", "pending"
	}
	locations := make([]map[string]string, len(paths))
	for i, p := range paths {
		locations[i] = map[string]string{"path": p}
	}
	told["locations"] = locations
	s.update(fakeSessionID, opened)
	s.update(fakeSessionID, told)
	if status != "" {
		s.update(fakeSessionID, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": id, "status": status})
	}
}

// edits is cursor's: kinds edit, delete and move are what count.
func (s *server) edits(id json.RawMessage) {
	wd, _ := os.Getwd()
	real, _ := filepath.EvalSymlinks(wd)
	s.editCall("made", "", "edit", "completed", filepath.Join(wd, "made.txt"))
	s.editCall("failed", "", "edit", "failed", filepath.Join(wd, "failed.txt"))
	s.editCall("cancelled", "", "edit", "cancelled", filepath.Join(wd, "cancelled.txt"))
	s.editCall("ran", "", "execute", "completed", filepath.Join(wd, "ran.txt"))
	s.editCall("unfinished", "", "edit", "", filepath.Join(wd, "unfinished.txt"))
	s.editCall("relative", "", "edit", "completed", "rel/one.txt")
	s.editCall("real", "", "edit", "completed", filepath.Join(real, "real.txt"))
	s.editCall("deleted", "", "edit", "completed", filepath.Join(wd, "gone", "dir", "deleted.txt"))
	s.editCall("again-failed", "", "edit", "failed", filepath.Join(wd, "again.txt"))
	s.editCall("again-made", "", "edit", "completed", filepath.Join(wd, "again.txt"))
	s.editCall("removed", "", "delete", "completed", filepath.Join(wd, "removed.txt"))
	s.editCall("remove-failed", "", "delete", "failed", filepath.Join(wd, "kept.txt"))
	s.editCall("renamed", "", "edit", "completed", filepath.Join(wd, "new-name.txt"))
	s.editCall("renamed-old", "", "delete", "completed", filepath.Join(wd, "old-name.txt"))
	s.editCall("moved", "", "move", "completed", filepath.Join(wd, "moved-from.txt"), filepath.Join(wd, "moved-to.txt"))
	s.editCall("file-uri", "", "edit", "completed", "file://"+filepath.Join(wd, "uri.txt"))
	s.editCall("web-uri", "", "edit", "completed", "https://example.com/not-a-file.txt")
	s.editCall("zero-width", "", "edit", "completed", filepath.Join(wd, "zero\u200bwidth.txt"))
	s.editCall("escape", "", "edit", "completed", "../escaped.txt")
	s.editCall("sibling", "", "edit", "completed", wd+"-sibling/x.txt")
	many := make([]string, 70)
	for i := range many {
		many[i] = fmt.Sprintf("many/%02d.txt", i)
	}
	s.editCall("many", "", "edit", "completed", many...)
	s.say("edited")
	s.finishPrompt(id, acp.StopEndTurn)
}

// grokEdits is grok's: its tool names are what count, and its plan file is known
// by its directory under the session id the agent gave the session.
func (s *server) grokEdits(id json.RawMessage) {
	wd, _ := os.Getwd()
	home := os.Getenv("GROK_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".grok")
	}
	sessions := filepath.Join(home, "sessions", url.PathEscape(wd))
	s.editCall("plan", "search_replace", "edit", "completed", filepath.Join(sessions, fakeSessionID, "plan.md"))
	s.editCall("other-plan", "search_replace", "edit", "completed", filepath.Join(sessions, "another-session", "plan.md"))
	s.editCall("worktree-plan", "search_replace", "edit", "completed", filepath.Join(wd, "plan.md"))
	s.editCall("made", "search_replace", "edit", "completed", filepath.Join(wd, "made.txt"))
	s.editCall("written", "write", "edit", "completed", "written.txt")
	s.editCall("failed", "search_replace", "edit", "failed", filepath.Join(wd, "failed.txt"))
	s.editCall("unknown", "image_edit", "edit", "completed", filepath.Join(wd, "unknown.txt"))
	s.editCall("shell", "run_terminal_command", "execute", "completed", filepath.Join(wd, "shell.txt"))
	s.editCall("unfinished", "write", "edit", "", filepath.Join(wd, "unfinished.txt"))
	s.say("edited")
	s.finishPrompt(id, acp.StopEndTurn)
}

// editRaceBuffered and editRaceLive are one call's two endings either side of
// session/new's reply: the completed update reaches the client before it has a session,
// so its NewSession flushes it on its caller, and the failed one, sent once
// CRAZE_FAKE_GATE lets it, reaches the read loop at the same time.
func (s *server) editRaceBuffered() {
	wd, _ := os.Getwd()
	s.update(fakeSessionID, map[string]any{
		"sessionUpdate": "tool_call", "toolCallId": "raced", "title": "Edit", "kind": "edit", "status": "completed",
		"locations": []map[string]string{{"path": filepath.Join(wd, "raced.txt")}},
	})
}

func (s *server) editRaceLive() {
	awaitGate(os.Getenv("CRAZE_FAKE_GATE"))
	s.update(fakeSessionID, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "raced", "status": "failed"})
}
