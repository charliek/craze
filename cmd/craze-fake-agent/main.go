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
          callorder   nocommands, and every reply ends with a receipt naming
                      each session/prompt and session/cancel read so far, in
                      arrival order
          markdown    a thought run, then one reply exercising markdown-lite
          title       session_info_update then echo
          env         reply "envset: <names> :end" naming which of
                      ROOST_AGENT_HOOK, ROOST_TAB_ID, HERDR_ENV and
                      HERDR_PANE_ID are set in the agent's environment
                      ("none" when none is)
          planmode    session/new in plan mode; replies planned/implementing
          planmode-card same, plus the cursor/create_plan card cursor sends
          effort      same as echo (session/new includes effort configOptions)
          modelconfig same as echo, plus a category "model" config option: the
                      shape of an agent that has no session/set_model and keeps
                      its model among its options
          modelconfig-refuse same, and session/set_model is answered -32601: the
                      whole of that agent, so a client's
                      set_model → set_config fallback runs for real
          modellate   echo with no model option at session/new; the agent's FIRST
                      config list arrives only after session/set_model has been
                      answered, and still carries the model value it held before
                      that set_model
          preinstall  modelconfig, but a current_mode_update, a session_info_update
                      and a moved config list are sent BEFORE session/new is
                      answered, so every one of them is dispatched ahead of the
                      snapshot that reply carries — and contradicts it
          permission  request allow_once / reject_once and wait for the client
          ask         emit cursor/ask_question then finish the turn
          plan        emit cursor/create_plan then finish the turn
          hang        do not finish the prompt until session/cancel
          hang-ack    hang, plus one "ack: <prompt text>" chunk sent first so a
                      client can wait for proof the prompt was read before it
                      cancels, instead of racing session/cancel against
                      session/prompt on the wire
          authfail    initialize ok, authenticate error
          turnfail    a normal session whose session/prompt fails with a
                      JSON-RPC error (-32000 "the turn failed")
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
          load        advertises loadSession; session/load replays a two-chunk
                      user message, a thought, a pending tool_call completed by
                      an update, and a reply, then answers {modes, models}
          grok-load   the same replay tagged _meta.isReplay, with the tool_call
                      already completed, a subagent_spawned/finished pair and a
                      turn_completed on _x.ai/session_notification; answers
                      {models} alone
          load-missing session/load fails -32602 "Session not found"
          load-hang   session/load is never answered
          load-long   session/load replays 600 message chunks then answers
          load-settings cursor's replay plus a current_mode_update and a
                      config_option_update, answered by a result that
                      contradicts both (mode agent, model default, no options)

The six load scripts refuse session/new with an error, so a test can prove no
client fell back to it. Every other script advertises loadSession false and
answers session/load with -32601.

Environment:
  CRAZE_FAKE_LINGER=1  do not exit when stdin closes: stay alive until a signal
                       ends the process, or 30 s at most. Without it the fake
                       dies of its own closed stdin however craze exits, so a
                       test cannot tell an agent craze shut down from one it
                       orphaned.
  CRAZE_FAKE_STDERR=<line>  every script except hang and hang-ack writes this
                       line to stderr once at startup and once per
                       session/prompt. hang and hang-ack stay silent, by the
                       same house rule that keeps their behaviour otherwise
                       unchanged.

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
	case "echo", "followup", "tool", "tasks", "effort", "modelconfig", "modelconfig-refuse",
		"modellate", "preinstall", "permission", "ask", "plan",
		"hang", "hang-ack", "authfail", "noauth", "todos", "todos-notify", "diff", "bigdiff",
		"bash", "task", "task-late", "commands", "nocommands", "callorder", "markdown", "title", "planmode", "planmode-card",
		"env", "turnfail",
		"grok-echo", "grok-ask", "grok-plan", "grok-ask-wrapped",
		"grok-subagent", "grok-subagent-fail", "grok-subagent-two", "grok-subagent-nested",
		"grok-subagent-late", "grok-subagent-cancel", "grok-subagent-cancel-early",
		"long-turn", "grok-long-turn", "grok-long-turn-fallback",
		"load", "grok-load", "load-missing", "load-hang", "load-long", "load-settings":
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
