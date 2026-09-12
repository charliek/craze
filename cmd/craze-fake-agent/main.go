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
          effort      same as echo (session/new includes effort configOptions)
          permission  request allow_once / reject_once and wait for the client
          ask         emit cursor/ask_question then finish the turn
          plan        emit cursor/create_plan then finish the turn
          hang        do not finish the prompt until session/cancel
          authfail    initialize ok, authenticate error
          noauth      initialize with empty authMethods; reject authenticate

Unknown arguments (including acp and --force) are ignored so this binary can
stand in for cursor-agent acp.
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
	case "echo", "followup", "tool", "tasks", "effort", "permission", "ask", "plan", "hang", "authfail", "noauth":
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
