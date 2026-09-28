craze builds the system prompt in the TUI when you start a session. It includes the working directory, today's date and the current git status, so the model always knows where it is.

To keep the provider's cache warm, craze re-sends the same prompt on every request and sets cache-control breakpoints on the system message. When the date changes or git status changes, the prompt is rebuilt, which costs one cache miss.

Sub-agents get a fresh prompt of their own with a shorter tool list, so they do not share the parent's cache.

When you resume a session, craze rebuilds the prompt and compares it against the stored copy in the transcript; if it differs you get a warning in the status row.
