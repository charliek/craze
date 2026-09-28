# Plan: date rollover

1. Add today's date to the system prompt's environment section (`Date: 2026-09-28`).
2. Start a timer when a session opens that fires at local midnight.
3. When it fires, rebuild the system prompt with the new date and send it with the next request.
4. Save the new prompt text in the session file so a resumed session shows the right date.
5. Add a unit test for the timer.
