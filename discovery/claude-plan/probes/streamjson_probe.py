#!/usr/bin/env python3
"""CL0 probe 1a: drive `claude -p` over stream-json with the control protocol.

Sends initialize, one user message, answers can_use_tool with allow, waits for
`result`, then closes stdin. Logs every frame (raw) to OUT and prints a summary.
Usage: streamjson_probe.py <claude-bin> <cwd> <out.jsonl> <model> <prompt> [extra args...]
"""
import json, os, subprocess, sys, threading, time

claude, cwd, out, model, prompt = sys.argv[1:6]
extra = sys.argv[6:]
env = {k: os.environ[k] for k in ("HOME", "USER", "LANG", "TERM", "PATH") if k in os.environ}
args = [claude, "-p", "--input-format", "stream-json", "--output-format", "stream-json",
        "--verbose", "--include-partial-messages", "--permission-prompts", "host",
        "--permission-prompt-tool", "stdio",
        "--model", model, "--permission-mode", "default", *extra]
p = subprocess.Popen(args, cwd=cwd, env=env, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                     stderr=subprocess.PIPE, text=True, bufsize=1)
log = open(out, "w")
def send(obj):
    log.write(json.dumps({"dir": "out", "msg": obj}) + "\n"); log.flush()
    p.stdin.write(json.dumps(obj) + "\n"); p.stdin.flush()

errbuf = []
threading.Thread(target=lambda: errbuf.extend(p.stderr.readlines()), daemon=True).start()
send({"type": "control_request", "request_id": "init-1", "request": {"subtype": "initialize", "hooks": None}})
sent_prompt = False
kinds = {}
deadline = time.time() + 240
for line in p.stdout:
    line = line.strip()
    if not line:
        continue
    log.write(json.dumps({"dir": "in", "raw": line}) + "\n"); log.flush()
    try:
        m = json.loads(line)
    except Exception:
        kinds["<non-json>"] = kinds.get("<non-json>", 0) + 1
        continue
    k = m.get("type", "?")
    sub = m.get("subtype") or (m.get("request") or {}).get("subtype") or (m.get("event") or {}).get("type")
    key = f"{k}/{sub}" if sub else k
    kinds[key] = kinds.get(key, 0) + 1
    if k == "control_response" and not sent_prompt:
        send({"type": "user", "message": {"role": "user", "content": prompt},
              "parent_tool_use_id": None, "session_id": ""})
        sent_prompt = True
    elif k == "control_request" and (m.get("request") or {}).get("subtype") == "can_use_tool":
        req = m["request"]
        send({"type": "control_response", "response": {"subtype": "success", "request_id": m["request_id"],
              "response": {"behavior": "allow", "updatedInput": req.get("input", {})}}})
    elif k == "rate_limit_event":
        print("RATE_LIMIT_EVENT:", json.dumps(m)[:800])
    elif k == "result":
        print("RESULT:", json.dumps({x: m.get(x) for x in ("subtype", "is_error", "result", "total_cost_usd", "num_turns", "stop_reason")})[:800])
        break
    if time.time() > deadline:
        print("TIMEOUT"); break
p.stdin.close()
try:
    p.wait(timeout=20)
except subprocess.TimeoutExpired:
    p.kill()
print("KINDS:", json.dumps(kinds, sort_keys=True))
print("EXIT:", p.returncode)
if errbuf:
    print("STDERR_TAIL:", "".join(errbuf)[-1500:])
