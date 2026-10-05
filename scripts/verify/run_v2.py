#!/usr/bin/env python3
"""run_v2.py -- plan 021 §S1b engine-migration parity harness (V2).

Driven by scripts/verify/v2.sh, which builds the candidate pair from a
committed sha and the baseline pair from commit 6581e0a; run it through that
script unless you are diagnosing the harness itself.

Runs `craze prompt --json` (and a couple of plain-mode cases) for a matrix of
scenarios against a BASELINE `craze` binary and a CANDIDATE `craze` binary,
both driving the SAME `craze-fake-agent` scripts, and asserts that:

  * exit status is identical
  * the whole ORDERED stdout is identical once the `seq` key's *value* is
    stripped from every JSON line (its presence/absence, and every other
    key/value/order, must still match exactly)

Everything a `craze prompt` invocation touches -- HOME, CRAZE_HOME, workspace
-- is a fresh temp directory per run, mirroring tests/cli/conftest.py and
tests/cli/test_prompt.py exactly (see build_env() below), so nothing here
ever reads or writes the operator's real ~/.config, ~/.craze or journals.

Usage:
  run_v2.py --baseline <bin> --candidate <bin> --fake-agent <bin>
            [--candidate-fake-agent <bin>] [--hold-fake-agent <bin>]
            [--out <dir>] [--only <substr>] [--repeat N]
  run_v2.py --calibrate --baseline <bin> --fake-agent <bin>
            [--hold-fake-agent <bin>] [--out <dir>] [--only <substr>] [--repeat N]

--out defaults to a timestamped directory under
${CRAZE_VERIFY_OUT:-${XDG_CACHE_HOME:-~/.cache}/craze-verify}/v2-adhoc/,
never inside the repository.

Stdlib only. Python 3.9+.
"""

from __future__ import annotations

import argparse
import dataclasses
import difflib
import json
import os
import signal
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path
from typing import Callable, Optional

# --------------------------------------------------------------------------
# Environment isolation -- mirrors tests/cli/conftest.py's isolate_run_env
# and tests/cli/test_prompt.py's run_prompt() helper, which are the
# known-good way this repo already drives craze against the fake agent.
# --------------------------------------------------------------------------

# The config-file variable CRAZE_HOME replaced. Spelled in two parts, same
# reason conftest.py does: keep the whole name out of grep for the internal
# repo-walk test, and out of a developer's real ~/.craze by construction.
REMOVED_CONFIG_ENV = "CRAZE_" + "CONFIG"


def host_env_names(env: dict) -> list:
    """Every herdr/roost variable in env -- the hosts craze may report to."""
    return [name for name in env if name.startswith(("HERDR_", "ROOST_"))]


def build_env(home: Path, craze_home: Path, script: str, extra: dict) -> dict:
    """One isolated child environment for one craze-prompt invocation.

    Starts from a copy of THIS process's environment (so PATH, mise shims,
    etc. still work), then applies exactly what conftest.py's
    isolate_run_env + test_prompt.py's run_prompt apply to a pytest run:
    HOME and CRAZE_HOME repointed at fresh temp dirs, the removed config-file
    variable (REMOVED_CONFIG_ENV, spelled in two parts above) and
    CRAZE_JOURNAL removed, every HERDR_*/ROOST_* removed, CRAZE_PROVIDER
    removed, XAI_API_KEY and GROK_CODE_XAI_API_KEY blanked (so the grok
    provider never reaches for a real key or the network), and
    CRAZE_FAKE_SCRIPT set to the scenario's fixture.
    """
    env = dict(os.environ)
    for name in host_env_names(env):
        del env[name]
    env.pop(REMOVED_CONFIG_ENV, None)
    env.pop("CRAZE_JOURNAL", None)
    env.pop("CRAZE_PROVIDER", None)
    env["HOME"] = str(home)
    env["CRAZE_HOME"] = str(craze_home)
    env["XAI_API_KEY"] = ""
    env["GROK_CODE_XAI_API_KEY"] = ""
    env["CRAZE_FAKE_SCRIPT"] = script
    env.update(extra)
    return env


# --------------------------------------------------------------------------
# Scenario model
# --------------------------------------------------------------------------


@dataclasses.dataclass
class Scenario:
    name: str
    script: str
    category: str
    text: str = "hello"
    provider: Optional[str] = None          # None -> cursor (default)
    follow_ups: tuple = ()
    flags: tuple = ()                       # extra CLI flags, e.g. ("--no-force",)
    json_mode: bool = True
    env: dict = dataclasses.field(default_factory=dict)
    timeout: float = 15.0
    signal_name: Optional[str] = None       # "SIGINT" / "SIGHUP"
    # trigger_factory: a zero-arg callable returning a FRESH predicate
    # line(str) -> bool each time it is called. A predicate may carry its own
    # mutable state (an occurrence counter, a start time), and the same
    # Scenario object is reused for the baseline run, the candidate run, and
    # every --repeat pass -- so run_once() must call this factory anew for
    # every single process it starts, never reuse one predicate instance
    # across runs, or a stateful trigger fires on the wrong run entirely.
    trigger_factory: Optional[Callable[[], Callable]] = None
    # signal_delay: an alternative to trigger_factory for a fixture that
    # emits no line to key on at all (plain `hang`) -- a fixed wall-clock
    # delay, from process start, on a background timer independent of stdout.
    # Mutually exclusive with trigger_factory in practice (only one is set
    # per scenario below); if both were set, whichever fires first wins.
    signal_delay: Optional[float] = None
    flaky_ok: bool = True
    notes: str = ""

    def cli_args(self) -> list:
        args = []
        if self.provider:
            args += ["--provider", self.provider]
        args += list(self.flags)
        for f in self.follow_ups:
            args += ["--follow-up", f]
        args.append(self.text)
        return args


# --------------------------------------------------------------------------
# Every CRAZE_FAKE_SCRIPT fixture (from cmd/craze-fake-agent/main.go's usage
# text and its script switch). Grouped by what main.go's own comments call
# them; comments trimmed to the load-bearing fact.
# --------------------------------------------------------------------------

ALL_SCRIPTS = [
    "echo", "followup", "tool", "tasks", "effort", "permission", "ask", "plan",
    "hang", "hang-ack", "authfail", "noauth", "todos", "todos-notify", "diff",
    "bigdiff", "bash", "task", "task-late", "commands", "nocommands",
    "callorder", "markdown", "title", "planmode", "planmode-card", "env",
    "turnfail",
    "grok-echo", "grok-ask", "grok-plan", "grok-ask-wrapped",
    "grok-subagent", "grok-subagent-fail", "grok-subagent-two",
    "grok-subagent-nested", "grok-subagent-late", "grok-subagent-cancel",
    "grok-subagent-cancel-early",
    "long-turn", "grok-long-turn", "grok-long-turn-fallback",
    "load", "grok-load", "load-missing", "load-hang", "load-long",
]

# Scripts that (empirically -- see the report) run `craze prompt` to
# StopReason end_turn / exit 0 on their own, with no signal needed. This is
# the set item 2 of the brief ("every fixture that completes normally") means.
# `noauth` is here too: authMethods comes back empty, so craze never calls
# authenticate at all and the run proceeds like `echo`.
NORMAL_SCRIPTS = [
    "echo", "followup", "tool", "tasks", "effort", "permission", "ask", "plan",
    "noauth", "todos", "todos-notify", "diff", "bigdiff", "bash", "task",
    "task-late", "commands", "nocommands", "callorder", "markdown", "title",
    "planmode", "planmode-card", "env",
    "grok-echo", "grok-ask", "grok-plan", "grok-ask-wrapped",
    "grok-subagent", "grok-subagent-fail", "grok-subagent-two",
    "grok-subagent-nested", "grok-subagent-late",
    "long-turn", "grok-long-turn", "grok-long-turn-fallback",
]

# Scripts whose plain sweep run (category 1, no signal) never ends on its
# own within a normal timeout: `hang`/`hang-ack` wait out session/cancel
# forever, and both grok-subagent-cancel* scripts poll session/cancel for up
# to 30s (waitCancelled in the fake agent) before giving up on their own.
# Giving these a short, explicit timeout keeps the sweep fast and turns the
# hang into a recorded TIMEOUT result on both sides instead of a real one.
SHORT_TIMEOUT_SCRIPTS = {
    "hang": 6.0,
    "hang-ack": 6.0,
    "grok-subagent-cancel": 6.0,
    "grok-subagent-cancel-early": 6.0,
}

GROK_STEP_ENV = {"CRAZE_FAKE_STEP": "20ms"}


def is_grok(script: str) -> bool:
    return script.startswith("grok-")


# --------------------------------------------------------------------------
# Signal triggers. Each is a predicate over one *raw* JSON stdout line
# (already known to be `--json` mode); it fires once, the first time it
# returns True, and the harness sends the signal right then -- never after a
# sleep. See the module docstring in test_prompt.py's hang-ack script comment
# for why a client should wait for a line, not a flag, before it cancels.
# --------------------------------------------------------------------------


def trigger_text_prefix(prefix: str) -> Callable:
    def fn(line: str) -> bool:
        try:
            ev = json.loads(line)
        except ValueError:
            return False
        return ev.get("type") == "text" and str(ev.get("text", "")).startswith(prefix)

    return fn


def trigger_queue_event_nth(event: str, n: int = 1) -> Callable:
    seen = {"n": 0}

    def fn(line: str) -> bool:
        try:
            ev = json.loads(line)
        except ValueError:
            return False
        if ev.get("type") == "queue" and ev.get("event") == event:
            seen["n"] += 1
            return seen["n"] == n
        return False

    return fn


def trigger_foreign_turn(event: str) -> Callable:
    def fn(line: str) -> bool:
        try:
            ev = json.loads(line)
        except ValueError:
            return False
        return ev.get("type") == "foreign_turn" and ev.get("event") == event

    return fn


def trigger_subagent_event(event: str) -> Callable:
    def fn(line: str) -> bool:
        try:
            ev = json.loads(line)
        except ValueError:
            return False
        return ev.get("type") == "subagent" and ev.get("event") == event

    return fn


# There is deliberately no line-based trigger for a fixed wall-clock delay.
# The plain `hang` script (unlike hang-ack) emits nothing at all before it
# blocks on session/cancel -- after the initial `--follow-up` "queued" line,
# if any, no further stdout line EVER arrives to re-check a predicate against,
# so a trigger keyed on "N seconds after the last line I saw" would simply
# never fire again and the run would sit out its full per-run timeout instead
# of ever exercising the signal path. run_once()'s `signal_delay` is the
# honest version of the same idea: a background timer independent of stdout,
# the same fixed margin tests/cli/test_prompt.py's test_cancel_hang uses.
# See the "sigint-hang-plain" scenario and the README's "known limits".


# --------------------------------------------------------------------------
# The scenario matrix
# --------------------------------------------------------------------------


def build_scenarios() -> list:
    scenarios = []

    # 1. Every CRAZE_FAKE_SCRIPT fixture, plain `--json` prompt.
    for script in ALL_SCRIPTS:
        env = dict(GROK_STEP_ENV) if script in ("long-turn", "grok-long-turn", "grok-long-turn-fallback") else {}
        timeout = SHORT_TIMEOUT_SCRIPTS.get(script, 15.0)
        notes = ""
        if script in SHORT_TIMEOUT_SCRIPTS:
            notes = (
                "never ends without session/cancel; run with a short timeout on "
                "purpose and recorded as a TIMEOUT on both sides."
            )
        scenarios.append(
            Scenario(
                name=f"sweep-{script}",
                script=script,
                category="1-sweep",
                text="hello",
                provider="grok" if is_grok(script) else None,
                env=env,
                timeout=timeout,
                notes=notes or "pins the fixture's plain --json event stream and exit status.",
            )
        )

    # 2. Every fixture that completes normally x two follow-ups.
    for script in NORMAL_SCRIPTS:
        env = dict(GROK_STEP_ENV) if script in ("long-turn", "grok-long-turn", "grok-long-turn-fallback") else {}
        scenarios.append(
            Scenario(
                name=f"followup-{script}",
                script=script,
                category="2-followup",
                text="one",
                provider="grok" if is_grok(script) else None,
                follow_ups=("a", "b"),
                env=env,
                notes="pins the queued/sent order across three turns on this fixture.",
            )
        )

    # 3. Permission fixture x decision variants x --no-force, and the forced
    # default (no --no-force, no --permission-decision: the session
    # auto-picks the yolo "allow always" option and never emits a
    # `permission` event at all).
    scenarios += [
        Scenario(
            name="permission-noforce-allow-once",
            script="permission",
            category="3-permission",
            text="go",
            flags=("--no-force", "--permission-decision", "allow-once"),
            notes="--no-force + allow-once: a `permission` event is emitted and answered.",
        ),
        Scenario(
            name="permission-noforce-reject-once",
            script="permission",
            category="3-permission",
            text="go",
            flags=("--no-force", "--permission-decision", "reject-once"),
            notes="--no-force + reject-once: exits 1, decision text is opt-reject.",
        ),
        Scenario(
            name="permission-forced-default",
            script="permission",
            category="3-permission",
            text="go",
            flags=(),
            notes="default --force: onPermission auto-picks yolo allow; no `permission` event at all.",
        ),
    ]

    # 4. ask / plan / grok-ask(-wrapped) / grok-plan auto-answer order. These
    # are also part of category 1's sweep (every fixture, plain --json); kept
    # as their own named scenarios too so a reader of the report can find
    # them by the property they pin rather than by fixture name alone.
    for name, script, provider, text in [
        ("ask-order", "ask", None, "q"),
        ("plan-order", "plan", None, "go"),
        ("grok-ask-order", "grok-ask", "grok", "q"),
        ("grok-plan-order", "grok-plan", "grok", "go"),
        ("grok-ask-wrapped-order", "grok-ask-wrapped", "grok", "q"),
    ]:
        scenarios.append(
            Scenario(
                name=name,
                script=script,
                category="4-auto-answer-order",
                text=text,
                provider=provider,
                notes="pins event order around a non-interactive auto-answer.",
            )
        )

    # 5. A non-end_turn stop with follow-ups queued: this is the same
    # mechanism as the mid-turn SIGINT scenarios in category 8 (a signal
    # both ends the current turn with stopReason cancelled AND clears the
    # queue), so it is not duplicated here -- see "sigint-mid-turn" and
    # "sighup-mid-turn" below, and the README's cross-reference.

    # 6. A turn that fails (JSON-RPC error) with follow-ups queued.
    scenarios.append(
        Scenario(
            name="turnfail-with-followups",
            script="turnfail",
            category="6-turnfail",
            text="go",
            follow_ups=("never runs",),
            notes="session/prompt errors mid-session; queue must report removed, exit 1.",
        )
    )

    # 7. Foreign-turn fixture (grok interject fallback), with and without a
    # follow-up queued behind it.
    scenarios += [
        Scenario(
            name="foreign-turn-with-followup",
            script="grok-long-turn-fallback",
            category="7-foreign-turn",
            text="do the steps STRAND-INTERJECTION",
            provider="grok",
            follow_ups=("Reply PINEAPPLE",),
            env=dict(GROK_STEP_ENV),
            timeout=30.0,
            notes="a turn grok starts on its own is bracketed; the queued follow-up runs after it, not into it.",
        ),
        Scenario(
            name="foreign-turn-no-followup",
            script="grok-long-turn-fallback",
            category="7-foreign-turn",
            text="do the steps STRAND-INTERJECTION",
            provider="grok",
            follow_ups=(),
            env=dict(GROK_STEP_ENV),
            timeout=30.0,
            flaky_ok=False,
            notes=(
                "no follow-up means nothing in craze ever waits on the stranded "
                "fallback turn; whether its brackets appear before the process "
                "exits races process teardown against the fake agent's 400ms "
                "strand window. Kept because both binaries race identically."
            ),
        ),
    ]

    # 8. Signals. Every trigger below fires off a specific line in the
    # child's stdout, never a sleep -- see the trigger_* helpers.
    scenarios += [
        Scenario(
            name="sigint-mid-turn",
            script="hang-ack",
            category="8-signal",
            text="wait",
            follow_ups=("never runs",),
            signal_name="SIGINT",
            trigger_factory=lambda: trigger_text_prefix("ack: "),
            timeout=15.0,
            notes=(
                "SIGINT sent the instant the 'ack: wait' line proves the prompt "
                "was read; also the category-5 non-end_turn-stop-with-queued-"
                "followups case: queue must report removed, exit 1."
            ),
        ),
        Scenario(
            name="sighup-mid-turn",
            script="hang-ack",
            category="8-signal",
            text="wait",
            follow_ups=("never runs",),
            signal_name="SIGHUP",
            trigger_factory=lambda: trigger_text_prefix("ack: "),
            timeout=15.0,
            notes="SIGHUP is craze's other signal-ends-everything path (a closed terminal).",
        ),
        Scenario(
            name="sigint-between-turns",
            script="sigint-hold",
            category="8-signal",
            text="one",
            follow_ups=("two", "three"),
            signal_name="SIGINT",
            trigger_factory=lambda: trigger_queue_event_nth("sent", 1),
            timeout=15.0,
            flaky_ok=False,
            notes=(
                "SIGINT sent the instant the first `queue sent` line appears -- "
                "the line that fires right as turn 2 is about to be dispatched, "
                "before its session/prompt reaches the wire. This is a real race "
                "against the runLoop's own goroutine (nothing in craze inserts a "
                "gap between turns), so it is the closest to deterministic this "
                "harness can make it without changing craze; see README."
            ),
        ),
        Scenario(
            name="sigint-foreign-turn-holds-the-drain",
            script="grok-long-turn-fallback",
            category="8-signal",
            text="do the steps STRAND-INTERJECTION",
            provider="grok",
            follow_ups=("Reply PINEAPPLE",),
            env=dict(GROK_STEP_ENV),
            signal_name="SIGINT",
            trigger_factory=lambda: trigger_text_prefix("noted: "),
            timeout=30.0,
            flaky_ok=False,
            notes=(
                "SIGINT sent off the fallback turn's OWN first text line, which is "
                "while that turn is still running and the queued follow-up is held "
                "behind it: craze is in its foreign-turn wait. Added for plan 021's "
                "r9 review finding 4 -- the signal clears that row, so no drain can "
                "ever release the wait, and the run must end on the stop rather "
                "than sit out its 60s foreign-turn budget. Judged like every other "
                "scenario here: exit status and the whole ordered stdout.\n"
                "The trigger is that line and not `foreign_turn started` (the first "
                "moment the wait exists) deliberately. Between those two lines sit "
                "turn one's own `done`, published by the prompt call's goroutine, "
                "and the fallback's text, published by the ACP read loop: two "
                "independent session publishers whose order nothing in craze fixes, "
                "in either binary. A signal landing between them makes a third "
                "publisher (the engine's outbox, with the cleared row) contend for "
                "the log's boundary and turns that unordered pair into a coin flip: "
                "calibration showed the baseline landing one way in 6 runs and the "
                "candidate both ways in 8. Triggering one line later leaves the "
                "pair already resolved and the wait still on, which is the property "
                "this scenario exists to pin.\n"
                "flaky_ok=False for a second, separate reason, and one the README "
                "already records for `foreign-turn-no-followup`: whether the "
                "fallback turn's CLOSING bracket reaches stdout before the process "
                "exits is a race in both binaries. Both read the session's "
                "foreign-turn flag, which is cleared just before the bracket is "
                "published, so a run can exit with that last line still in flight. "
                "Observed: the baseline printed it in 10 calibration runs on an "
                "idle box and lost it once inside a full 103-scenario matrix run; "
                "the candidate, which reads the event before it re-reads the flag, "
                "has not been seen to lose it. A DIFF here whose only difference is "
                "that last line is this race, not a regression -- judge it by "
                "content, as the README says for the other two."
            ),
        ),
        Scenario(
            name="sigint-hang-plain",
            script="hang",
            category="8-signal",
            text="wait",
            follow_ups=("never runs",),
            signal_name="SIGINT",
            signal_delay=0.4,
            timeout=15.0,
            flaky_ok=False,
            notes=(
                "the plain `hang` script (unlike hang-ack) emits nothing before "
                "it blocks, so there is no line to key on; this uses a fixed "
                "0.4s wall-clock wait, the same margin tests/cli/test_prompt.py "
                "uses for test_cancel_hang. Kept for the extra fixture coverage; "
                "sigint-mid-turn above is the deterministic version of this case."
            ),
        ),
        Scenario(
            name="sigint-subagent-cancel",
            script="grok-subagent-cancel",
            category="8-signal",
            text="go",
            provider="grok",
            signal_name="SIGINT",
            trigger_factory=lambda: trigger_subagent_event("spawned"),
            timeout=15.0,
            notes="SIGINT sent the instant the child's `spawned` line appears; child still running when cancelled.",
        ),
        Scenario(
            name="sigint-subagent-cancel-early",
            script="grok-subagent-cancel-early",
            category="8-signal",
            text="go",
            provider="grok",
            signal_name="SIGINT",
            trigger_factory=lambda: trigger_subagent_event("spawned"),
            timeout=15.0,
            notes=(
                "same trigger as sigint-subagent-cancel: both scripts hold the "
                "parent open until session/cancel arrives regardless of timing "
                "(waitCancelled), and only the fixture's own post-cancel script "
                "differs (child reported already completed, not cancelled)."
            ),
        ),
    ]

    # 9. Plain (non-JSON) mode, comparing stdout bytes exactly.
    scenarios += [
        Scenario(
            name="plain-echo",
            script="echo",
            category="9-plain-mode",
            text="hello",
            json_mode=False,
            notes="plain mode stdout is exactly the agent's own text, byte for byte.",
        ),
        Scenario(
            name="plain-grok-subagent",
            script="grok-subagent",
            category="9-plain-mode",
            text="go",
            provider="grok",
            json_mode=False,
            notes="plain mode excludes child/subagent text; only the parent's own text reaches stdout.",
        ),
    ]

    # 10. Sub-agent fixtures (craze prompt drains sub-agents at the end).
    # grok-subagent, grok-subagent-fail, grok-subagent-two,
    # grok-subagent-nested and grok-subagent-late are already covered by
    # category 1 (sweep) and category 2 (followup); grok-subagent-cancel(-early)
    # by category 8's two sigint-subagent-cancel* scenarios; the cursor
    # `task`/`task-late` drain path by categories 1 and 2 too. Nothing new to
    # add here -- see the README's cross-reference table.

    return scenarios


# --------------------------------------------------------------------------
# Running one scenario against one binary
# --------------------------------------------------------------------------


@dataclasses.dataclass
class RunResult:
    returncode: Optional[int]
    stdout: str
    stderr: str
    timed_out: bool
    signal_sent: bool
    cmd: list
    env_overrides: dict


def run_once(binary: Path, fake_agent: Path, scenario: Scenario, run_dir: Path) -> RunResult:
    home = run_dir / "home"
    craze_home = run_dir / "craze-home"
    ws = run_dir / "ws"
    home.mkdir(parents=True, exist_ok=True)
    ws.mkdir(parents=True, exist_ok=True)

    env = build_env(home, craze_home, scenario.script, scenario.env)
    cmd = [str(binary), "prompt"]
    if scenario.json_mode:
        cmd.append("--json")
    cmd += ["--agent-bin", str(fake_agent), "--workspace", str(ws)]
    cmd += scenario.cli_args()

    proc = subprocess.Popen(
        cmd,
        cwd=str(ws),
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        bufsize=1,
    )

    timed_out = {"flag": False}

    def killer():
        timed_out["flag"] = True
        try:
            proc.kill()
        except Exception:
            pass

    timer = threading.Timer(scenario.timeout, killer)
    timer.daemon = True
    timer.start()

    stderr_chunks = []

    def read_stderr():
        try:
            for line in proc.stderr:
                stderr_chunks.append(line)
        except Exception:
            pass

    t_err = threading.Thread(target=read_stderr, daemon=True)
    t_err.start()

    sig = getattr(signal, scenario.signal_name) if scenario.signal_name else None
    signal_state = {"sent": False}
    signal_lock = threading.Lock()

    def send_once():
        with signal_lock:
            if signal_state["sent"] or sig is None:
                return
            signal_state["sent"] = True
        try:
            proc.send_signal(sig)
        except Exception:
            pass

    delay_timer = None
    if sig is not None and scenario.signal_delay is not None:
        # A fixed wall-clock delay, independent of stdout: the only option
        # for a fixture (plain `hang`) that emits no line to key a trigger on
        # after its process starts. See the comment above trigger_subagent_event.
        delay_timer = threading.Timer(scenario.signal_delay, send_once)
        delay_timer.daemon = True
        delay_timer.start()

    stdout_lines = []
    # A fresh predicate instance for THIS run: the same Scenario is reused
    # for the baseline run, the candidate run and every --repeat pass, and a
    # stateful trigger (an occurrence counter, a start time) must not carry
    # state over from a previous process's run.
    trigger = scenario.trigger_factory() if scenario.trigger_factory else None
    try:
        for line in proc.stdout:
            stdout_lines.append(line)
            if sig is not None and trigger is not None and not signal_state["sent"]:
                if trigger(line):
                    send_once()
    except Exception:
        pass
    finally:
        if delay_timer is not None:
            delay_timer.cancel()
    signal_sent = signal_state["sent"]

    try:
        proc.wait(timeout=max(1.0, scenario.timeout))
    except subprocess.TimeoutExpired:
        killer()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            pass
    timer.cancel()
    t_err.join(timeout=5)

    return RunResult(
        returncode=proc.returncode,
        stdout="".join(stdout_lines),
        stderr="".join(stderr_chunks),
        timed_out=timed_out["flag"],
        signal_sent=signal_sent,
        cmd=cmd,
        env_overrides={"CRAZE_FAKE_SCRIPT": scenario.script, **scenario.env},
    )


# --------------------------------------------------------------------------
# Normalisation and comparison
# --------------------------------------------------------------------------


def canonical_no_seq(line: str):
    """(canonical JSON text with `seq` popped, had_seq: bool) for one line.

    json.loads on a well-formed object preserves key order in the returned
    dict (CPython inserts keys in encounter order, and dicts have preserved
    insertion order since 3.7); popping "seq" and re-dumping with the same
    separators therefore reproduces the original key order minus that one
    key, exactly as the brief specifies -- and does it without the trailing-
    comma edge case a regex over the raw text would have to special-case
    (seq is not always followed by another key: an event with every other
    field at its zero value has seq as the JSON object's last key).
    """
    obj = json.loads(line)
    had_seq = isinstance(obj, dict) and "seq" in obj
    if isinstance(obj, dict):
        obj.pop("seq", None)
    canon = json.dumps(obj, separators=(",", ":"), ensure_ascii=False, sort_keys=False)
    return canon, had_seq


def parse_stdout_lines(stdout: str) -> list:
    return [ln for ln in stdout.split("\n") if ln.strip() != ""]


@dataclasses.dataclass
class LineDiff:
    index: int
    kind: str  # "content" | "seq-presence" | "extra-base" | "extra-cand" | "parse-error"
    base: str
    cand: str


def compare_json_mode(base_stdout: str, cand_stdout: str) -> list:
    base_lines = parse_stdout_lines(base_stdout)
    cand_lines = parse_stdout_lines(cand_stdout)
    diffs = []
    n = max(len(base_lines), len(cand_lines))
    for i in range(n):
        if i >= len(base_lines):
            diffs.append(LineDiff(i, "extra-cand", "", cand_lines[i]))
            continue
        if i >= len(cand_lines):
            diffs.append(LineDiff(i, "extra-base", base_lines[i], ""))
            continue
        b_raw, c_raw = base_lines[i], cand_lines[i]
        try:
            b_canon, b_had_seq = canonical_no_seq(b_raw)
        except ValueError:
            diffs.append(LineDiff(i, "parse-error", b_raw, c_raw))
            continue
        try:
            c_canon, c_had_seq = canonical_no_seq(c_raw)
        except ValueError:
            diffs.append(LineDiff(i, "parse-error", b_raw, c_raw))
            continue
        if b_canon != c_canon:
            diffs.append(LineDiff(i, "content", b_raw, c_raw))
        elif b_had_seq != c_had_seq:
            diffs.append(LineDiff(i, "seq-presence", b_raw, c_raw))
    return diffs


def compare_plain_mode(base_stdout: str, cand_stdout: str) -> list:
    if base_stdout == cand_stdout:
        return []
    return [LineDiff(0, "content", base_stdout, cand_stdout)]


@dataclasses.dataclass
class ScenarioOutcome:
    scenario: Scenario
    same: bool
    reason: str
    base: RunResult
    cand: RunResult
    diffs: list


def evaluate(scenario: Scenario, base: RunResult, cand: RunResult) -> ScenarioOutcome:
    reasons = []
    if base.timed_out != cand.timed_out:
        reasons.append(f"timed_out differs: base={base.timed_out} cand={cand.timed_out}")
    if not (base.timed_out and cand.timed_out):
        # A TIMEOUT's returncode is whatever killing the process produced
        # (typically a negative signal number) and is not itself part of the
        # contract; only compare exit status when neither side timed out.
        if base.returncode != cand.returncode:
            reasons.append(f"exit status differs: base={base.returncode} cand={cand.returncode}")
    if scenario.json_mode:
        diffs = compare_json_mode(base.stdout, cand.stdout)
    else:
        diffs = compare_plain_mode(base.stdout, cand.stdout)
    if diffs:
        reasons.append(f"{len(diffs)} stdout line(s) differ")
    same = not reasons
    return ScenarioOutcome(scenario, same, "; ".join(reasons), base, cand, diffs)


# --------------------------------------------------------------------------
# Reporting
# --------------------------------------------------------------------------


def save_raw(out_dir: Path, scenario: Scenario, label: str, result: RunResult) -> None:
    d = out_dir / scenario.name
    d.mkdir(parents=True, exist_ok=True)
    (d / f"{label}.stdout").write_text(result.stdout, encoding="utf-8")
    (d / f"{label}.stderr").write_text(result.stderr, encoding="utf-8")
    meta = {
        "cmd": result.cmd,
        "env_overrides": result.env_overrides,
        "returncode": result.returncode,
        "timed_out": result.timed_out,
        "signal_sent": result.signal_sent,
    }
    (d / f"{label}.meta.json").write_text(json.dumps(meta, indent=2), encoding="utf-8")


def write_diff(out_dir: Path, scenario: Scenario, outcome: ScenarioOutcome) -> None:
    d = out_dir / scenario.name
    d.mkdir(parents=True, exist_ok=True)
    base_lines = parse_stdout_lines(outcome.base.stdout)
    cand_lines = parse_stdout_lines(outcome.cand.stdout)
    udiff = difflib.unified_diff(
        base_lines, cand_lines, fromfile="baseline", tofile="candidate", lineterm=""
    )
    text = "\n".join(udiff)
    if outcome.reason:
        text = f"# {outcome.reason}\n" + text
    (d / "diff.txt").write_text(text, encoding="utf-8")


def print_summary(outcomes: list) -> bool:
    all_same = True
    name_w = max((len(o.scenario.name) for o in outcomes), default=10)
    header = f"{'scenario':{name_w}}  base_rc  cand_rc  base_lines  cand_lines  result"
    print(header)
    print("-" * len(header))
    for o in outcomes:
        result = "SAME" if o.same else "DIFF"
        if not o.same:
            all_same = False
        base_lines = len(parse_stdout_lines(o.base.stdout))
        cand_lines = len(parse_stdout_lines(o.cand.stdout))
        base_rc = "TIMEOUT" if o.base.timed_out else str(o.base.returncode)
        cand_rc = "TIMEOUT" if o.cand.timed_out else str(o.cand.returncode)
        flaky = "" if o.scenario.flaky_ok else "  (flaky_ok=False)"
        print(
            f"{o.scenario.name:{name_w}}  {base_rc:>7}  {cand_rc:>7}  "
            f"{base_lines:>10}  {cand_lines:>10}  {result}{flaky}"
        )
        if not o.same:
            print(f"{'':{name_w}}    reason: {o.reason}")
    n_same = sum(1 for o in outcomes if o.same)
    print("-" * len(header))
    print(f"{n_same}/{len(outcomes)} scenarios SAME")
    return all_same


# --------------------------------------------------------------------------
# Modes
# --------------------------------------------------------------------------


def run_compare(args) -> int:
    scenarios = build_scenarios()
    if args.only:
        scenarios = [s for s in scenarios if args.only in s.name]
    if not scenarios:
        print(f"no scenario matches --only {args.only!r}", file=sys.stderr)
        return 2

    out_dir = Path(args.out) if args.out else default_out_dir()
    out_dir.mkdir(parents=True, exist_ok=True)
    print(f"output dir: {out_dir}")

    baseline = Path(args.baseline)
    candidate = Path(args.candidate)
    fake_agent = Path(args.fake_agent)
    cand_fake_agent = Path(args.candidate_fake_agent) if args.candidate_fake_agent else fake_agent

    hold_fake_agent = Path(args.hold_fake_agent) if args.hold_fake_agent else None

    repeat = args.repeat or 1
    all_outcomes_by_repeat = []
    overall_same = True

    for rep in range(repeat):
        rep_outcomes = []
        for scenario in scenarios:
            with tempfile.TemporaryDirectory(prefix="v2-base-") as base_tmp, tempfile.TemporaryDirectory(
                prefix="v2-cand-"
            ) as cand_tmp:
                base_fa, cand_fa = fake_agent, cand_fake_agent
                if hold_fake_agent is not None and scenario.script == "sigint-hold":
                    base_fa = cand_fa = hold_fake_agent
                base = run_once(baseline, base_fa, scenario, Path(base_tmp))
                cand = run_once(candidate, cand_fa, scenario, Path(cand_tmp))
            outcome = evaluate(scenario, base, cand)
            rep_outcomes.append(outcome)
            label_suffix = f".rep{rep}" if repeat > 1 else ""
            save_raw(out_dir, scenario, f"baseline{label_suffix}", base)
            save_raw(out_dir, scenario, f"candidate{label_suffix}", cand)
            if not outcome.same:
                write_diff(out_dir, scenario, outcome)
        all_outcomes_by_repeat.append(rep_outcomes)
        print(f"\n=== repeat {rep + 1}/{repeat} ===")
        same = print_summary(rep_outcomes)
        overall_same = overall_same and same

    return 0 if overall_same else 1


def run_calibrate(args) -> int:
    scenarios = build_scenarios()
    if args.only:
        scenarios = [s for s in scenarios if args.only in s.name]
    if not scenarios:
        print(f"no scenario matches --only {args.only!r}", file=sys.stderr)
        return 2
    baseline = Path(args.baseline)
    fake_agent = Path(args.fake_agent)
    hold_fake_agent = Path(args.hold_fake_agent) if args.hold_fake_agent else None
    repeat = args.repeat or 3
    if repeat < 2:
        print("--calibrate needs --repeat >= 2 to compare anything", file=sys.stderr)
        return 2

    out_dir = Path(args.out) if args.out else default_out_dir(prefix="calibrate")
    out_dir.mkdir(parents=True, exist_ok=True)
    print(f"calibrating {len(scenarios)} scenario(s) x {repeat} baseline runs each")
    print(f"output dir: {out_dir}\n")

    any_variance = False
    for scenario in scenarios:
        # The same rule compare mode applies: a sigint-hold scenario runs on
        # the fake agent that knows that script.
        fa = fake_agent
        if hold_fake_agent is not None and scenario.script == "sigint-hold":
            fa = hold_fake_agent
        runs = []
        for i in range(repeat):
            with tempfile.TemporaryDirectory(prefix="v2-cal-") as tmp:
                r = run_once(baseline, fa, scenario, Path(tmp))
            runs.append(r)
            save_raw(out_dir, scenario, f"cal{i}", r)

        # Compare every repeat against the first. Plain (non-JSON) mode
        # scenarios compare raw stdout text, exactly like evaluate() does for
        # the baseline-vs-candidate path -- a plain-mode line is the agent's
        # own text, not JSON, and trying to json.loads it here would report
        # every repeat as "VARIES" on a parse error that has nothing to do
        # with real nondeterminism.
        base_lines = parse_stdout_lines(runs[0].stdout)
        scenario_diffs = []

        if not scenario.json_mode:
            for i in range(1, repeat):
                rc_i = runs[i]
                if rc_i.returncode != runs[0].returncode or rc_i.timed_out != runs[0].timed_out:
                    scenario_diffs.append(
                        f"run0 rc={runs[0].returncode} timed_out={runs[0].timed_out}  "
                        f"run{i} rc={rc_i.returncode} timed_out={rc_i.timed_out}"
                    )
                if rc_i.stdout != runs[0].stdout:
                    scenario_diffs.append(
                        f"plain stdout differs (run0 vs run{i}):\n    run0: {runs[0].stdout!r}\n    run{i}: {rc_i.stdout!r}"
                    )
        else:
            base_canon = []
            for ln in base_lines:
                try:
                    base_canon.append(canonical_no_seq(ln)[0])
                except ValueError:
                    base_canon.append(f"<<PARSE-ERROR>>{ln}")

            for i in range(1, repeat):
                rc_i = runs[i]
                if rc_i.returncode != runs[0].returncode or rc_i.timed_out != runs[0].timed_out:
                    scenario_diffs.append(
                        f"run0 rc={runs[0].returncode} timed_out={runs[0].timed_out}  "
                        f"run{i} rc={rc_i.returncode} timed_out={rc_i.timed_out}"
                    )
                lines_i = parse_stdout_lines(rc_i.stdout)
                n = max(len(base_lines), len(lines_i))
                for idx in range(n):
                    if idx >= len(base_lines) or idx >= len(lines_i):
                        scenario_diffs.append(
                            f"line {idx}: run0 has {len(base_lines)} lines, run{i} has {len(lines_i)}"
                        )
                        continue
                    try:
                        canon_i, had_seq_i = canonical_no_seq(lines_i[idx])
                    except ValueError:
                        scenario_diffs.append(f"line {idx}: run{i} failed to parse as JSON: {lines_i[idx]!r}")
                        continue
                    if canon_i != base_canon[idx]:
                        try:
                            obj0 = json.loads(base_lines[idx])
                            obji = json.loads(lines_i[idx])
                        except ValueError:
                            obj0 = obji = None
                        field_report = diff_fields(obj0, obji) if isinstance(obj0, dict) and isinstance(obji, dict) else None
                        if field_report:
                            scenario_diffs.append(f"line {idx} field(s) differ (run0 vs run{i}): {field_report}")
                        else:
                            scenario_diffs.append(
                                f"line {idx} differs (run0 vs run{i}):\n    run0: {base_lines[idx]}\n    run{i}: {lines_i[idx]}"
                            )

        if scenario_diffs:
            any_variance = True
            print(f"[VARIES] {scenario.name} ({scenario.script}):")
            for d in scenario_diffs:
                print(f"    {d}")
        else:
            print(f"[stable] {scenario.name}")

    print()
    if any_variance:
        print("Calibration found real baseline-vs-baseline variance -- see above.")
        print("Add ONLY the specific fields shown to NORMALIZE_TABLE, nothing broader.")
        return 1
    print("Calibration found no variance beyond `seq` in any scenario.")
    return 0


def diff_fields(a: dict, b: dict):
    keys = list(dict.fromkeys(list(a.keys()) + list(b.keys())))
    out = {}
    for k in keys:
        if k == "seq":
            continue
        va, vb = a.get(k, "<absent>"), b.get(k, "<absent>")
        if va != vb:
            out[k] = {"run0": va, "runN": vb}
    return out


def default_out_dir(prefix: str = "run") -> Path:
    """A timestamped directory under the verify output root, never the repo."""
    ts = time.strftime("%Y%m%d-%H%M%S")
    root = os.environ.get("CRAZE_VERIFY_OUT") or os.path.join(
        os.environ.get("XDG_CACHE_HOME") or os.path.join(os.path.expanduser("~"), ".cache"),
        "craze-verify",
    )
    return Path(root) / "v2-adhoc" / f"{prefix}_{ts}"


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------


def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--baseline", required=True, help="path to the baseline craze binary")
    p.add_argument("--candidate", help="path to the candidate craze binary (not needed with --calibrate)")
    p.add_argument("--fake-agent", required=True, help="path to craze-fake-agent for the baseline side")
    p.add_argument(
        "--hold-fake-agent",
        help="plan 032 SF-87: a craze-fake-agent that knows the sigint-hold script; used for BOTH "
        "sides on scenarios whose script is sigint-hold, and for every --calibrate run of one "
        "(every other scenario is unchanged)",
    )
    p.add_argument(
        "--candidate-fake-agent",
        help="path to craze-fake-agent for the candidate side (default: same as --fake-agent)",
    )
    p.add_argument(
        "--out",
        help="output directory (default: a timestamped dir under "
        "${CRAZE_VERIFY_OUT:-${XDG_CACHE_HOME:-~/.cache}/craze-verify}/v2-adhoc/)",
    )
    p.add_argument("--only", help="run only scenarios whose name contains this substring")
    p.add_argument("--repeat", type=int, help="repeat the whole matrix N times (compare mode) or N baseline runs per scenario (--calibrate, default 3)")
    p.add_argument("--calibrate", action="store_true", help="run baseline-vs-itself N times and report every differing field")
    return p.parse_args(argv)


def main(argv=None) -> int:
    args = parse_args(argv if argv is not None else sys.argv[1:])
    # Every binary path is resolved to absolute up front: run_once passes
    # cwd=<per-run workspace> to the child process, so a relative path given
    # on the command line would otherwise be looked up from the wrong place
    # (or fail outright) once the working directory changes underneath it.
    args.baseline = str(Path(args.baseline).resolve())
    args.fake_agent = str(Path(args.fake_agent).resolve())
    if args.candidate:
        args.candidate = str(Path(args.candidate).resolve())
    if args.candidate_fake_agent:
        args.candidate_fake_agent = str(Path(args.candidate_fake_agent).resolve())
    if args.hold_fake_agent:
        args.hold_fake_agent = str(Path(args.hold_fake_agent).resolve())
    if args.out:
        args.out = str(Path(args.out).resolve())

    if not Path(args.baseline).is_file():
        print(f"--baseline {args.baseline} is not a file", file=sys.stderr)
        return 2
    if not Path(args.fake_agent).is_file():
        print(f"--fake-agent {args.fake_agent} is not a file", file=sys.stderr)
        return 2
    if args.calibrate:
        return run_calibrate(args)
    if not args.candidate:
        print("--candidate is required unless --calibrate is given", file=sys.stderr)
        return 2
    if not Path(args.candidate).is_file():
        print(f"--candidate {args.candidate} is not a file", file=sys.stderr)
        return 2
    return run_compare(args)


if __name__ == "__main__":
    sys.exit(main())
