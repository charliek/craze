#!/usr/bin/env python3
"""CL0 probes 1b and 2: drive claude-agent-acp over ACP v1 and log everything.

Usage: acp_probe.py <adapter-js> <cwd> <out.jsonl> <plan>
                    [--elicit] [--subagents] [--mode-default] [--load SESSION_ID]
plan: comma list of steps:
  read, write, ask, plan, agent, steer, cancel2 (cancel during generation),
  bgwait (a background command, then 30 s idle to catch the wake turn),
  cancel (a backgrounded sleep: Claude Code backgrounds it, so it rarely
  tests cancel), usage
--elicit      declare clientCapabilities.elicitation.form (AskUserQuestion)
--subagents   declare clientCapabilities.subagents (child sessions)
--mode-default  switch from the adapter's default `auto` mode to `default`
--load ID     session/load ID instead of session/new (replay check; no turn)
Every frame is logged to out.jsonl; a per-step summary is printed. Each turn
asks for haiku at low effort. Logs and output carry account identifiers
(email, organisation): keep them out of the repo.
"""
import json, os, subprocess, sys, threading, time, queue

adapter, cwd, out, plan = sys.argv[1:5]
elicit = "--elicit" in sys.argv
load_id = sys.argv[sys.argv.index("--load") + 1] if "--load" in sys.argv else None
steps = [s for s in plan.split(",") if s]

env = {k: os.environ[k] for k in ("HOME", "USER", "LANG", "TERM", "PATH") if k in os.environ}
env["CLAUDE_CODE_EXECUTABLE"] = os.path.expanduser("~/.local/bin/claude")
env["CLAUDE_AGENT_ACP_PROTOCOL"] = "v1"
p = subprocess.Popen(["node", adapter], cwd=cwd, env=env, stdin=subprocess.PIPE,
                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, bufsize=1)
log = open(out, "w")
lock = threading.Lock()
next_id = [0]
pending = {}
step_name = ["setup"]
summary = {}

def note(kind):
    s = summary.setdefault(step_name[0], {})
    s[kind] = s.get(kind, 0) + 1

def write(obj):
    with lock:
        log.write(json.dumps({"t": round(time.time(), 3), "step": step_name[0], "dir": "out", "msg": obj}) + "\n"); log.flush()
        p.stdin.write(json.dumps(obj) + "\n"); p.stdin.flush()

def call(method, params, timeout=240):
    next_id[0] += 1
    rid = next_id[0]
    q = queue.Queue()
    pending[rid] = q
    write({"jsonrpc": "2.0", "id": rid, "method": method, "params": params})
    try:
        return q.get(timeout=timeout)
    except queue.Empty:
        return {"error": "TIMEOUT"}

def notify(method, params):
    write({"jsonrpc": "2.0", "method": method, "params": params})

texts = {}
def on_request(m):
    method = m["method"]; prm = m.get("params", {})
    note("req:" + method)
    if method == "session/request_permission":
        tc = prm.get("toolCall", {})
        opts = prm.get("options", [])
        print(f"  [{step_name[0]}] PERMISSION title={tc.get('title')!r} kind={tc.get('kind')} "
              f"options={[(o.get('optionId'), o.get('kind'), o.get('name')) for o in opts]}")
        pick = next((o for o in opts if o.get("kind") == "allow_once"), None) or \
               next((o for o in opts if (o.get("kind") or "").startswith("allow")), None) or opts[0]
        write({"jsonrpc": "2.0", "id": m["id"], "result": {"outcome": {"outcome": "selected", "optionId": pick["optionId"]}}})
    elif method == "elicitation/create":
        schema = prm.get("requestedSchema", {})
        print(f"  [{step_name[0]}] ELICITATION mode={prm.get('mode')} msg={str(prm.get('message'))[:120]!r} schema={json.dumps(schema)[:400]}")
        content = {}
        for k, v in (schema.get("properties") or {}).items():
            if "enum" in v: content[k] = v["enum"][0]
            elif v.get("type") == "array": content[k] = [((v.get("items") or {}).get("enum") or ["x"])[0]]
            elif v.get("oneOf"): content[k] = v["oneOf"][0].get("const")
            elif v.get("type") == "boolean": content[k] = True
            else: content[k] = "blue"
        write({"jsonrpc": "2.0", "id": m["id"], "result": {"action": "accept", "content": content}})
    else:
        print(f"  [{step_name[0]}] UNHANDLED REQUEST {method}: {json.dumps(prm)[:300]}")
        write({"jsonrpc": "2.0", "id": m["id"], "error": {"code": -32601, "message": "method not found"}})

def reader():
    for line in p.stdout:
        line = line.strip()
        if not line: continue
        with lock:
            log.write(json.dumps({"t": round(time.time(), 3), "step": step_name[0], "dir": "in", "raw": line}) + "\n"); log.flush()
        try: m = json.loads(line)
        except Exception: note("<non-json>"); continue
        if "id" in m and "method" not in m:
            q = pending.pop(m["id"], None)
            if q: q.put(m)
        elif "method" in m and "id" in m:
            threading.Thread(target=on_request, args=(m,), daemon=True).start()
        elif "method" in m:
            prm = m.get("params", {})
            if m["method"] == "session/update":
                u = prm.get("update", {})
                kind = u.get("sessionUpdate", "?")
                note("upd:" + kind)
                sids = summary.setdefault("_session_ids", {})
                sids[prm.get("sessionId")] = sids.get(prm.get("sessionId"), 0) + 1
                if kind == "agent_message_chunk":
                    c = u.get("content", {})
                    texts[step_name[0]] = texts.get(step_name[0], "") + (c.get("text") or "")
                elif kind in ("usage_update", "notice", "subagent_update", "current_mode_update", "compaction_update", "session_info_update"):
                    print(f"  [{step_name[0]}] {kind}: {json.dumps(u)[:500]}")
                elif kind == "tool_call":
                    print(f"  [{step_name[0]}] tool_call: title={u.get('title')!r} kind={u.get('kind')} meta={json.dumps(u.get('_meta'))[:200]}")
            else:
                note("notif:" + m["method"])
                print(f"  [{step_name[0]}] NOTIF {m['method']}: {json.dumps(prm)[:300]}")
threading.Thread(target=reader, daemon=True).start()
errbuf = []
threading.Thread(target=lambda: errbuf.extend(p.stderr.readlines()), daemon=True).start()

caps = {"fs": {"readTextFile": False, "writeTextFile": False}, "terminal": False,
        "_meta": {"parameterizedModelPicker": True}}
if elicit:
    caps["elicitation"] = {"form": {}}
if "--subagents" in sys.argv:
    caps["subagents"] = {}
if "--mode-default" in sys.argv:
    pass  # handled after session/new
init = call("initialize", {"protocolVersion": 1, "clientCapabilities": caps,
                           "clientInfo": {"name": "cl0-probe", "version": "0"}})
r = init.get("result", {})
print("INIT agentCapabilities:", json.dumps(r.get("agentCapabilities"))[:600])
print("INIT authMethods:", json.dumps(r.get("authMethods")))
print("INIT _meta:", json.dumps(r.get("_meta"))[:400])

if load_id:
    step_name[0] = "load"
    res = call("session/load", {"sessionId": load_id, "cwd": cwd, "mcpServers": []})
    print("LOAD result:", json.dumps(res)[:600])
    sid = load_id
else:
    res = call("session/new", {"cwd": cwd, "mcpServers": []})
    if "error" in res:
        print("NEW ERROR:", json.dumps(res)); sys.exit(1)
    nr = res["result"]
    sid = nr["sessionId"]
    print("SESSION", sid)
    print("MODES:", json.dumps(nr.get("modes"))[:500])
    cos = nr.get("configOptions") or []
    for co in cos:
        print("CONFIG:", co.get("id"), co.get("category"), co.get("currentValue"),
              [o.get("value") for o in (co.get("options") or [])][:14])
    model_opt = next((c for c in cos if c.get("category") == "model" or c.get("id") == "model"), None)
    if model_opt:
        hv = next((o["value"] for o in model_opt.get("options", []) if "haiku" in str(o.get("value")).lower()), None)
        if hv:
            print("SET model ->", hv, json.dumps(call("session/set_config_option", {"sessionId": sid, "configId": model_opt["id"], "value": hv}))[:200])
    if "--mode-default" in sys.argv:
        print("SET mode default:", json.dumps(call("session/set_mode", {"sessionId": sid, "modeId": "default"}))[:120])
    eff = next((c for c in cos if c.get("id") in ("effort", "thought_level") or c.get("category") == "thought_level"), None)
    if eff and any(o.get("value") == "low" for o in eff.get("options", [])):
        print("SET effort -> low", json.dumps(call("session/set_config_option", {"sessionId": sid, "configId": eff["id"], "value": "low"}))[:120])

def prompt(text, timeout=240):
    return call("session/prompt", {"sessionId": sid, "prompt": [{"type": "text", "text": text}]}, timeout)

PROMPTS = {
    "read": "Read note.txt with your Read tool and reply with its exact contents and nothing else.",
    "write": "Use your Write tool to create out.txt containing exactly: ok. Then reply done.",
    "ask": "Use the AskUserQuestion tool to ask me one question: do I prefer red or blue (two options). Then reply with only the answer I chose.",
    "agent": "Use the Agent tool (subagent_type general-purpose) to have a subagent read note.txt and report its contents. Then reply with what it reported.",
    "plan": "Plan how to create plan.txt containing the single word x. Present the plan with ExitPlanMode; after approval, create the file and reply done.",
}
for st in steps:
    step_name[0] = st
    t0 = time.time()
    if st in PROMPTS:
        if st == "plan":
            print("SET mode plan:", json.dumps(call("session/set_mode", {"sessionId": sid, "modeId": "plan"}))[:200])
        res = prompt(PROMPTS[st])
    elif st == "cancel":
        holder = {}
        th = threading.Thread(target=lambda: holder.update(r=prompt("Run the bash command: sleep 25 && echo finished. Then reply with its output.")))
        th.start(); time.sleep(8)
        notify("session/cancel", {"sessionId": sid})
        th.join(60); res = holder.get("r", {"error": "no reply after cancel"})
    elif st == "steer":
        holder = {}
        th = threading.Thread(target=lambda: holder.update(r=prompt("Run the bash command: sleep 12. After it finishes, reply with the secret word if I have told you one, otherwise reply 'none'.")))
        th.start(); time.sleep(5)
        sr = call("_session/steering", {"sessionId": sid, "prompt": [{"type": "text", "text": "The secret word is banana."}]}, 30)
        print("  [steer] steering reply:", json.dumps(sr)[:300])
        th.join(120); res = holder.get("r", {"error": "no reply"})
    elif st == "usage":
        res = prompt("/usage", 60)
    elif st == "cancel2":
        holder = {}
        th = threading.Thread(target=lambda: holder.update(r=prompt("Without using any tools, write a 1500-word story about a lighthouse keeper.")))
        th.start(); time.sleep(4)
        notify("session/cancel", {"sessionId": sid})
        th.join(60); res = holder.get("r", {"error": "no reply after cancel"})
    elif st == "bgwait":
        res = prompt("Run this bash command in the background (run_in_background): sleep 10 && echo bg-done. Reply only 'started' right away, without waiting for it.")
        print("  [bgwait] prompt returned; idling 30s to watch for unsolicited updates")
        step_name[0] = "bgwait-idle"; time.sleep(30); step_name[0] = "bgwait"
        print(f"  idle text: {texts.get('bgwait-idle', '')[:300]!r}")
        print(f"  idle kinds: {json.dumps(summary.get('bgwait-idle', {}), sort_keys=True)}")
    else:
        print("unknown step", st); continue
    rr = res.get("result") or res
    print(f"STEP {st}: {round(time.time()-t0,1)}s stopReason={rr.get('stopReason') if isinstance(rr, dict) else rr} "
          f"err={json.dumps(res.get('error'))[:300] if res.get('error') else None} meta={json.dumps((rr or {}).get('_meta') if isinstance(rr, dict) else None)[:300]}")
    print(f"  text: {texts.get(st, '')[:300]!r}")
    print(f"  kinds: {json.dumps(summary.get(st, {}), sort_keys=True)}")

print("SESSION_ID", sid)
print("UPDATES PER SESSION ID:", json.dumps(summary.get("_session_ids", {})))
p.stdin.close()
try: p.wait(timeout=15)
except subprocess.TimeoutExpired: p.kill()
if errbuf:
    print("STDERR_TAIL:", "".join(errbuf)[-1200:])
