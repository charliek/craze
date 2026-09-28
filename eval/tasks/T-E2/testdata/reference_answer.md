Short answer: the turn does not end. A bad call becomes an error tool result that the model reads on its next step, and it can try again.

**1. Fantasy validates the call first.** Native runs on Fantasy (`charm.land/fantasy` v0.43.2). Before dispatching a call, `validateToolCall` (`agent.go:1198-1245`) checks that the tool name matches exactly, that the arguments parse as JSON and that the schema's required fields are present. It does not check types.

**2. Malformed JSON gets one repair attempt.** With no repair hook configured — craze sets no `WithRepairToolCall` (`internal/harness/harness.go:381-386`) — Fantasy runs `jsonrepair.RepairJSON` on the arguments and validates again (`agent.go:1176-1184`). If that still fails, the call is marked `Invalid` with its `ValidationError` and is not run (`agent.go:1187-1190`); Fantasy records an error result for it, and that is not a fatal failure (`agent.go:809-817`).

**3. craze turns it into an error result.** The tool bridge discards the dispatcher's prepared call for an invalid one (`toolbridge.go:154-156`) and hands the model Fantasy's reason as a tool result with `IsError: true` and class `invalid_input` (`toolbridge.go:252-268`). For an unknown tool the model reads `tool not found: <name>. Available tools: bash, edit, ...` — the dispatcher uses the same wording (`internal/harness/tool/dispatch.go:164-168`).

**4. Schema mismatches that are valid JSON.** A wrong type or out-of-range value passes Fantasy's check and is caught by the tool's own `Prepare` (`tool/opencode/args.go`). The dispatcher returns `The <tool> tool was called with invalid arguments: ... Please rewrite the input so it satisfies the expected schema.` (`dispatch.go:216-218`).

**5. The loop continues.** Neither the dispatcher nor the bridge ever returns a Go error, because Fantasy would treat that as fatal (`dispatch.go:45-47`). The step ends with tool calls, so Fantasy starts the next step with the error in the history. `TestUnknownToolIsAnErrorResult` (`turn_test.go:171-212`) shows it: the model gets the not-found result, answers, and the turn ends normally with `end_turn`.

**6. What does stop it.** The doom-loop detector (`doomloop.go`) refuses the 3rd identical call in a row with a nudge and stops the turn at the 5th (`max_turn_requests`); invalid calls count too. The step cap is 200 (`maxSteps`, `turn.go:53`). `maxRetries = 1` is unrelated — it retries a failed provider request.

In the UI the call shows as a failed tool row, and the transcript keeps the error result paired with the call.
