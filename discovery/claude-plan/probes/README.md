# CL0 probes

These are the scripts behind [08](../08-cl0-results.md), kept here to re-run
the probes before building. Probe 1 needs re-running whenever `claude` or the
adapter has a big release, or the policy pages in
[02](../02-policy-and-billing.md) change.

Every run spends a little of the plan's quota on real turns. Steps ask for
`haiku` at low effort; plan mode may switch to Sonnet on its own.
- **Where to run:** scratch directories outside the repo.
- **What to keep out of the repo:** the logs and printed output. They carry
  the account's email and organisation.
- **Requirements:** Python 3, the user's signed-in `claude`, and for route A
  Node ≥22.

## Install the adapter, outside the repo

```sh
D=$(mktemp -d)
npm install --prefix "$D" --ignore-scripts --omit=optional --no-audit --no-fund \
  @agentclientprotocol/claude-agent-acp@latest
ADAPTER_JS=$D/node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js
```

`--omit=optional` leaves out the copy of `claude` that the SDK's platform
package would bring. The scripts set `CLAUDE_CODE_EXECUTABLE` to the user's
own `claude` instead.

## Probe 1: billing (both routes)

```sh
W=$(mktemp -d); echo hello > "$W/note.txt"

# Route B: raw stream-json and the control protocol.
python3 -I streamjson_probe.py ~/.local/bin/claude "$W" "$W/1a.jsonl" haiku \
  "Use your Write tool to create out.txt containing exactly: ok. Then reply done."

# Route A: the ACP adapter.
python3 -I acp_probe.py "$ADAPTER_JS" "$W" "$W/1b.jsonl" read --mode-default
```

**Pass:** each prints a `rate_limit_event` / `usage_update` rate-limit object
with `"isUsingOverage": false` and a success result. **Fail:** `true`, or an
error naming "extra usage". That is what CLD-02 turns into a craze error. A
failure also reopens CLQ-2.

`streamjson_probe.py` strips the environment to `HOME`, `USER`, `LANG`,
`TERM` and `PATH`. So it is safe to run from inside a Claude Code session,
whose `CLAUDECODE` and `CLAUDE_CODE_*` variables would otherwise leak into
the child.

## Probe 2: adapter fit

```sh
python3 -I acp_probe.py "$ADAPTER_JS" "$W" "$W/2.jsonl" \
  write,ask,plan,agent,cancel2,steer,bgwait --elicit --subagents --mode-default
python3 -I acp_probe.py "$ADAPTER_JS" "$W" "$W/2-load.jsonl" "" \
  --elicit --subagents --load <SESSION_ID printed above>
```

Compare what it prints with the feature table in [08](../08-cl0-results.md).
What to look for:
- `session/update` kinds and request methods the table does not list;
- changed permission option ids (ExitPlanMode);
- whether new sessions still start in `auto`;
- whether a load still forgets the model.

## Probe 2b: unmodified craze against the adapter

```sh
export ADAPTER_JS
H=$(mktemp -d)   # a scratch CRAZE_HOME keeps the session index clean
env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT CRAZE_HOME="$H" CRAZE_DETACH=0 \
  craze prompt --provider cursor --agent-bin "$PWD/claude-acp-as-cursor.sh" \
  --model haiku --no-force --permission-decision allow-once --json \
  --workspace "$W" "Read note.txt and reply with its contents."
```

The wrapper ignores the cursor arguments craze adds, and runs the adapter with
a clean environment. In CL0 this completed a turn with craze 0.2.0 unchanged.
