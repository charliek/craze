package main

import (
	"bufio"
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
          sigint-hold echo for the first prompt; every later prompt waits for
                      session/cancel (answered cancelled) or, with
                      CRAZE_FAKE_GATE set, one byte from that FIFO (answered
                      as echo): a signal sent between turns always finds the
                      second turn in flight
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
          grok-subagent-two-hold same, hangs after sub-1's progress instead
                      of finishing (a golden's last event to wait on)
          grok-subagent-nested grandchild spawned on the child's session
          grok-subagent-late child still running at prompt_complete
          grok-subagent-late-hold same off-beat prompt_complete, but the
                      child is then held forever (no late line, no finish):
                      the running frame can never race a child that finished
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
                      whole of that agent, so a client that tried
                      set_model first would have to fall back to set_config
          modellate   echo with no model option at session/new and no
                      session/set_model follow-up of its own: the shape of an
                      agent whose FIRST config list arrives only after
                      session/set_model has been answered, still carrying the
                      model value it held before that set_model, is delivered
                      by the test itself (r28 finding 3), so the schedule that
                      exercises it is forced rather than raced on the wire
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
          exit-two-lines reads its first request (craze's initialize), leaves
                      it unanswered, writes "Error: KEYCHAIN LOCKED" and "Run
                      unlock and retry." to stderr and exits 1: an agent that
                      dies at its start with two lines to say why, as cursor
                      does on a locked keychain. The request is read first,
                      so it was written before the exit: the call fails with
                      the exit's status, never with a write's broken pipe
          long-reply  echo's session, but every prompt is answered with 600
                      message chunks, "line 1" to "line 600", then end_turn
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
          permodel    cursor's per-model catalog (permodel.go): grok-4.6,
                      composer-2.5, claude-opus-5 and glm-5.2, each with its
                      own options; set_config_option(model, X) switches and
                      answers X's catalog, pushing nothing; set_model switches
                      and answers {}; session/load answers with the current
                      model's catalog; an echo turn
          permodel-empty same, and every set_config_option that succeeds
                      answers {"configOptions": []}
          permodel-noreply same, and set_config_option(model, X) answers {}
          permodel-refuse same, and set_config_option(model, …) is refused
                      -32602 "Unknown model config option: model"
          permodel-nomodel same, and no catalog carries a model option
          permodel-pushbefore same, and set_config_option(model, X) writes a
                      config_option_update just before its reply: X's catalog
                      with its last option moved to the other value
          permodel-pushafter the same push, just after the reply
          permodel-pushmodel-after after the reply, a push of the next
                      model's catalog, the model moved on to it
          prompt-dump echo's session, but each prompt is answered with one
                      chunk listing its blocks: "prompt N: K blocks", then per
                      block its index and type, a text block's quoted text, an
                      image's mimeType, data=<base64 length>, bytes=<decoded
                      length> and uri
          grok-prompt-dump the same over the grok dialect (image:false)
          reject-image a prompt carrying an image block is refused -32602
                      (after one message chunk when its text holds SAY-FIRST);
                      any other prompt is answered as prompt-dump answers
          grok-reject-image the same refusal over the grok dialect; the
                      resend that follows it first gets a queue/changed naming
                      the refused prompt (p-<n-1>) alone, by the resend's
                      block 1, and that prompt's prompt_complete (cancelled),
                      then its own queue entry, dump, prompt_complete and a
                      reply carrying its promptId (p-<n>)

Every script advertises promptCapabilities as its dialect's live agent does:
image true for the cursor scripts, false for the grok- ones.

The six load scripts refuse session/new with an error, so a test can prove no
client fell back to it. The permodel scripts advertise loadSession and answer
both. Every other script advertises loadSession false and answers
session/load with -32601.

Environment:
  CRAZE_FAKE_LINGER=1  do not exit when stdin closes: stay alive until a signal
                       ends the process, or 30 s at most. Without it the fake
                       dies of its own closed stdin however craze exits, so a
                       test cannot tell an agent craze shut down from one it
                       orphaned.
  CRAZE_FAKE_STUBBORN=1  CRAZE_FAKE_LINGER for 90 s, and SIGTERM, SIGINT,
                       SIGHUP and SIGPIPE are ignored: only SIGKILL ends the
                       process before then, so a test can prove a detached
                       host's spawner kills the agent's process group itself.
  CRAZE_FAKE_STDERR=<line>  every script except hang and hang-ack writes this
                       line to stderr once at startup and once per
                       session/prompt. hang and hang-ack stay silent, by the
                       same house rule that keeps their behaviour otherwise
                       unchanged.
  CRAZE_FAKE_GATE=<path>  tasks stops after its first tool_call until it has
                       read one byte from <path>, a FIFO: the test writes one
                       byte per turn to release it, so it can act while the
                       turn is known to be in progress. Unset, tasks runs
                       straight through. sigint-hold's held prompts also end
                       on that byte, answered as echo, and every step of the
                       long-turn scripts waits for one (CRAZE_FAKE_STEP is
                       then unused): a turn held for as long as the test needs.
                       So does a grok-long-turn-fallback fallback turn whose
                       interjection's text contains HOLD-FALLBACK, in place of
                       its fixed window.
  CRAZE_FAKE_DUMP_PROMPTS=<path>  every script appends one JSON line to this
                       file per session/prompt it reads ({"prompt":[blocks]},
                       each block as prompt-dump describes it) and per
                       x.ai/interject ({"interject":"<text>"}), in arrival
                       order.
  CRAZE_FAKE_DUMP_CALLS=<path>  every script appends one line to this file per
                       request and notification it reads, in arrival order:
                       the method, and for session/set_config_option a space
                       and "<configId>=<value>".
  CRAZE_FAKE_SET_GATE=<path>  every session/set_config_option is answered only
                       once one byte has been read from <path>, a FIFO (one
                       byte per set), off the read loop: the fake reads -- and
                       CRAZE_FAKE_DUMP_CALLS records -- whatever craze sends
                       while a set is held.
  CRAZE_FAKE_SET_REFUSE=<id>  every session/set_config_option of option <id>
                       is refused -32602 (data.message "Refused config option:
                       <id>"), after CRAZE_FAKE_SET_GATE's byte when that is
                       set too.
  CRAZE_FAKE_LATE_CATALOG=1  session/new's available_commands_update is held
                       back until the first session/prompt is read, and sent
                       then, ahead of that prompt's turn: the catalog lands
                       while craze's first prompt is in flight, the schedule
                       the image resend's heard rule has to survive (craze
                       plan 033 C3r) without a race against the reader.
  CRAZE_FAKE_SESSION_ID=<id>  session/new answers this session id instead of
                       fake-session-1, so several sessions in one test HOME
                       are several rows of its session index. {dir} in it is
                       the name of the agent's working directory -- the
                       session's workspace -- for the hosts one terminal
                       starts, which inherit its environment.

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
		"hang", "hang-ack", "sigint-hold", "authfail", "noauth", "todos", "todos-notify", "diff", "bigdiff",
		"bash", "task", "task-late", "commands", "nocommands", "callorder", "markdown", "title", "planmode", "planmode-card",
		"env", "turnfail", "long-reply",
		"grok-echo", "grok-ask", "grok-plan", "grok-ask-wrapped",
		"grok-subagent", "grok-subagent-fail", "grok-subagent-two", "grok-subagent-two-hold", "grok-subagent-nested",
		"grok-subagent-late", "grok-subagent-late-hold", "grok-subagent-hold", "grok-subagent-cancel", "grok-subagent-cancel-early",
		"long-turn", "grok-long-turn", "grok-long-turn-fallback",
		"load", "grok-load", "load-missing", "load-hang", "load-long", "load-settings",
		"permodel", "permodel-empty", "permodel-noreply", "permodel-refuse", "permodel-nomodel",
		"permodel-pushbefore", "permodel-pushafter", "permodel-pushmodel-after",
		"prompt-dump", "grok-prompt-dump", "reject-image", "grok-reject-image",
		"exit-two-lines":
	default:
		fmt.Fprintf(os.Stderr, "craze-fake-agent: unknown script %q\n", script)
		os.Exit(2)
	}
	dumpArgv()
	if script == "exit-two-lines" {
		exitTwoLines()
	}
	if err := run(script); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// exitTwoLines is exit-two-lines (plan 035 A6): an agent that dies at its
// start with two lines on its stderr, as cursor does on a locked keychain.
// It reads its first request — craze's initialize — before it says anything,
// so that request was written while the agent ran: the call is still pending
// at the exit and fails with the exit's status (acp: agent exited: exit
// status 1), never with the broken pipe a write to an agent already gone
// would get, whatever the scheduling.
func exitTwoLines() {
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Fprint(os.Stderr, "Error: KEYCHAIN LOCKED\nRun unlock and retry.\n")
	os.Exit(1)
}

func dumpArgv() {
	p := os.Getenv("CRAZE_FAKE_DUMP_ARGV")
	if p == "" {
		return
	}
	_ = os.WriteFile(p, []byte(strings.Join(os.Args[1:], "\n")+"\n"), 0o644)
}
