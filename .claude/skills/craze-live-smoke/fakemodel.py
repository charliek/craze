#!/usr/bin/env python3
"""fakemodel.py --native-dir <CRAZE_HOME>/native --state <file> [--fixtures <dir>]:
a loopback model server for a $0 native smoke. It scripts one tool-using turn
-- a `read` of notes.txt in the workspace, then an answer quoting it -- on
tests/cli/sse_fixture.py's OpenAI-compatible endpoint (127.0.0.1, an
OS-assigned port), and writes providers.toml and models.toml into
--native-dir pointing craze's native provider at it with the fixture's
obviously-fake key (catalog = false: "Fixture Model" is the whole table).

It runs until SIGTERM or SIGINT. --state is rewritten (atomically) every
half second: `pid=`, `port=`, `requests=` (how many the model has answered),
`remaining=` (scripted steps left; 0 once the turn has run) and one
`req <path> tools=<n>` line per request. A request past the script is
answered with an error, never a guess.

--fixtures is the tests/cli folder holding sse_fixture.py; by default the one
of the checkout this file sits in (<repo>/.claude/skills/craze-live-smoke).
Stdlib and sse_fixture only.
"""
import argparse
import os
import signal
import sys
import time
from pathlib import Path


def main(argv):
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0], add_help=True)
    ap.add_argument("--native-dir", required=True, type=Path, help="<CRAZE_HOME>/native (a scratch CRAZE_HOME)")
    ap.add_argument("--state", required=True, type=Path, help="the state file to keep up to date")
    ap.add_argument("--fixtures", type=Path, default=Path(__file__).resolve().parents[3] / "tests" / "cli")
    args = ap.parse_args(argv)
    if not (args.fixtures / "sse_fixture.py").is_file():
        ap.error(f"no sse_fixture.py in {args.fixtures}: pass --fixtures <repo>/tests/cli")

    sys.path.insert(0, str(args.fixtures))
    from sse_fixture import SSEFixture, answer, call_step, write_native_config

    stop = False

    def _stop(*_):
        nonlocal stop
        stop = True

    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT, _stop)

    srv = SSEFixture().start()
    try:
        write_native_config(args.native_dir, srv.base_url)
        srv.set_script([
            call_step("read", {"filePath": "notes.txt"}),
            answer("The notes say: ", "banana-42 is the secret word."),
        ])
        part = args.state.with_name(args.state.name + ".part")
        while True:
            reqs = list(srv.requests)
            part.write_text(
                f"pid={os.getpid()}\nport={srv.port}\nrequests={len(reqs)}\nremaining={srv.script_remaining}\n"
                + "".join(f"req {r.path} tools={len(r.tool_names)}\n" for r in reqs)
            )
            os.replace(part, args.state)
            if stop:
                return 0
            time.sleep(0.5)
    finally:
        srv.close()


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
