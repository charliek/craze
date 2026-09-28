When the model calls a tool that doesn't exist, craze stops the turn and shows an "invalid tool call" error in the transcript; you have to send another message to continue.

For arguments that aren't valid JSON, craze validates them against the tool's JSON Schema (types included) and, if they fail, sends the model a system message asking it to retry. It gets one retry (maxRetries = 1); if the second attempt is also invalid the turn fails with an error.

So yes: a bad tool call usually ends the turn.
