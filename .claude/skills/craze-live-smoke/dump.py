#!/usr/bin/env python3
"""dump.py <transcript.jsonl>: one line per part of a native session's
transcript -- role, tool name, a slice of the input/result/text -- plus the
mode, model and effort changes and each step's usage, so a live smoke can be
read without the TUI's scrollback (the TUI runs in the alternate screen).

The transcripts are $CRAZE_HOME/native/sessions/<slug>/<UTC stamp>_<session
id>.jsonl, so one session is *_<session id>.jsonl (a scratch CRAZE_HOME in a
smoke, never the owner's ~/.craze). Taken from Plan 023's
smoke/dump.py. Stdlib only.
"""
import json
import sys


def main(argv):
    if len(argv) != 2 or argv[1] in ("-h", "--help"):
        print(__doc__.strip())
        return 0 if len(argv) == 2 else 2
    with open(argv[1], encoding="utf-8") as f:
        for line in f:
            e = json.loads(line)
            t = e.get("type")
            if t == "mode_change":
                print(f"-- mode_change -> {e.get('mode')}")
                continue
            if t == "model_change":
                print(f"-- model_change -> {e.get('model')}")
                continue
            if t == "effort_change":
                print(f"-- effort_change -> {e.get('effort')}")
                continue
            if t != "message":
                continue
            m = e["message"]
            role = m["role"]
            for p in m.get("content") or []:
                d = p.get("data", {})
                k = p["type"]
                if k == "tool-call":
                    print(f"{role:9} CALL   {d.get('tool_name')}  {str(d.get('input'))[:150]}")
                elif k == "tool-result":
                    out = d.get("result") or d.get("output") or d
                    print(f"{role:9} RESULT {d.get('tool_name')}  {json.dumps(out)[:200]}")
                elif k == "text":
                    print(f"{role:9} TEXT   {d.get('text', '')[:200]!r}")
                elif k == "reasoning":
                    print(f"{role:9} THINK  {d.get('text', '')[:100]!r}")
            u = e.get("usage")
            if u:
                print(f"          usage {u}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
