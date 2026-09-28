# Plan: a date-rollover reminder for native sessions

## Where it fits

The date cannot go in the system prompt: it is frozen per session for the prefix cache (D-30), and `SystemEnv` holds only the workspace and OS. The right vehicle is the existing mode-reminder machinery in `internal/harness/reminders.go`: a `<system-reminder>` user message composed at a step boundary by `turn.remind` (called from `prepareStep`, `steer.go`) and spliced after the prompt.

## Storage and replay

Reminders are stored as `reminder` entries carrying a **variant**, never text (D-66, `store/entry.go` `TypeReminder`, `Entry.Variant`); `checkVariant` only allows `[a-z0-9_]{1,64}`. The store's `Renderer` re-renders the variant byte for byte on every request (`store/transcript.go`), so the date must be recoverable at replay without reading the clock:

- Encode the date in the variant: `date_2026_09_28`. It passes `checkVariant`; `modes.render` parses the `date_` prefix and renders "The date is now Monday, 2026-09-28." An older craze renders an unknown variant as nothing, which costs one cache miss and nothing else.
- Deriving it from the entry's `Timestamp` would not work as is: the renderer only gets the variant, and the timestamp is UTC stamped at append time, so the local date would need the offset too.

## When it fires

In `remind`, at each step boundary: compare today's local date from the session clock (`s.now`, from `Options.Now` — not `time.Now()`, so tests can fix it) with the last date the model was told. The session start date counts as told (the first message has no reminder). On resume, seed the last-told date from the last `date_*` reminder entry on the path (today `readPath` ignores reminder entries).

`reminderVariant` currently requires a mode and only one reminder can be pending per step; restart reuse (`restartReminder`, `lastReminderVariant`) assumes mode reminders. I would add a separate pending slot for the date reminder, written as its own entry right after a mode reminder when both fire, and have `lastReminderVariant` skip `date_*` variants. `TestEveryReminderTextHasOneVariant` needs its composed-set check to treat the parameterised family as one variant.

Like mode reminders, it is written only with a finished step's append (`AppendStepLed`); an interrupted save writes none, so the "told" date advances only when persisted.

## Tests

- `reminders_test.go`: a fixed clock crossing midnight mid-turn fires once, and not again the same day; plan mode plus a rollover in one step yields both reminders in order.
- `wire_test.go`: `TestReminderHistoryMatchesTheSentRequest`-style check that replay renders the same bytes; `TestReminderPrefixIsStable`-style prefix check.
- `store/reminder_test.go`: a `date_*` variant round-trips.
- `resume_test.go`: resuming the next day fires once; resuming the same day does not.

No changes made.
