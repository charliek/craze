package main

import (
	"fmt"
	"os"
	"strings"
)

const usage = `craze-fake-agent is a scripted ACP stdio server for tests.

Usage:
  craze-fake-agent [flags] [ignored...]

Flags:
  -script string
        Scripted behavior (default "echo"):
          echo        initialize, authenticate, session/new; echo prompt text
          followup    first prompt and second prompt return different replies
          tool        emit a tool_call update then a text chunk
          tasks       emit two tool_calls with updates, then text "done tasks"
          todos       cursor/update_todos requests (replace then merge)
          todos-notify same list sent as notifications (no JSON-RPC id)
          diff        read tool with rawOutput plus an edit tool with a diff
          bigdiff     edit tool with 200 KiB diff sides
          bash        execute tool completing with exitCode 127
          task        sub-agent tool with a cursor/task receipt
          task-late   same, receipt sent before the tool_call
          grok-subagent one grok explore child with progress and finish
          grok-subagent-fail same, child finishes failed
          grok-subagent-two two parallel children, colliding tool ids
          grok-subagent-nested grandchild spawned on the child's session
          grok-subagent-late child still running at prompt_complete
          grok-subagent-cancel cancel with a running child (finish after)
          grok-subagent-cancel-early cancel after the child finished
          commands    echo, but session/new advertises 24 commands with long
                      descriptions (one of them multi-line) for the slash menu
          nocommands  echo, but session/new advertises no commands at all, so
                      craze's own plugin rows stay provisionally qualified
          markdown    a thought run, then one reply exercising markdown-lite
          title       session_info_update then echo
          planmode    session/new in plan mode; replies planned/implementing
          planmode-card same, plus the cursor/create_plan card cursor sends
          effort      same as echo (session/new includes effort configOptions)
          permission  request allow_once / reject_once and wait for the client
          ask         emit cursor/ask_question then finish the turn
          plan        emit cursor/create_plan then finish the turn
          hang        do not finish the prompt until session/cancel
          authfail    initialize ok, authenticate error
          noauth      initialize with empty authMethods; reject authenticate
          grok-echo   grok initialize/auth; echo; x.ai/session/prompt_complete
          grok-ask    x.ai/ask_user_question then complete
          grok-plan   x.ai/exit_plan_mode then complete
          grok-ask-wrapped wrapped _x.ai/ask_user_question then complete
          long-turn   cursor: two 600 ms execute tools then DONE step1 step2;
                      a second prompt cancels the first and runs instead
          grok-long-turn same over the grok dialect, with x.ai/queue/changed at
                      turn start and x.ai/interject merged at the next tool
                      result (DONE step1 <interjection> step2)
          grok-long-turn-fallback interjections are never merged: each becomes
                      grok's own interject-fallback turn once the session is
                      idle, ended by turn_completed alone

Unknown arguments (including acp, --force, agent, stdio, --always-approve,
--yolo, --no-auto-update, --trust) are ignored so this binary can stand in
for cursor-agent acp or grok agent stdio.
`

func main() {
	script := "echo"
	if env := os.Getenv("CRAZE_FAKE_SCRIPT"); env != "" {
		script = env
	}
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-h" || a == "--help":
			fmt.Fprint(os.Stdout, usage)
			os.Exit(0)
		case a == "-script" || a == "--script":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "craze-fake-agent: -script requires a value")
				os.Exit(2)
			}
			i++
			script = args[i]
		case strings.HasPrefix(a, "-script="):
			script = strings.TrimPrefix(a, "-script=")
		case strings.HasPrefix(a, "--script="):
			script = strings.TrimPrefix(a, "--script=")
		}
	}
	switch script {
	case "echo", "followup", "tool", "tasks", "effort", "permission", "ask", "plan",
		"hang", "authfail", "noauth", "todos", "todos-notify", "diff", "bigdiff",
		"bash", "task", "task-late", "commands", "nocommands", "markdown", "title", "planmode", "planmode-card",
		"grok-echo", "grok-ask", "grok-plan", "grok-ask-wrapped",
		"grok-subagent", "grok-subagent-fail", "grok-subagent-two", "grok-subagent-nested",
		"grok-subagent-late", "grok-subagent-cancel", "grok-subagent-cancel-early",
		"long-turn", "grok-long-turn", "grok-long-turn-fallback":
	default:
		fmt.Fprintf(os.Stderr, "craze-fake-agent: unknown script %q\n", script)
		os.Exit(2)
	}
	dumpArgv()
	if err := run(script); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func dumpArgv() {
	p := os.Getenv("CRAZE_FAKE_DUMP_ARGV")
	if p == "" {
		return
	}
	_ = os.WriteFile(p, []byte(strings.Join(os.Args[1:], "\n")+"\n"), 0o644)
}
