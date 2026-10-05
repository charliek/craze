"""Plan 018 C11: `craze --provider native` from the outside.

Through H7 the native provider was hidden (plan 018 §3.4): never listed,
never persisted, never indexed. Plan 028 §3.16/D-65 lists it once H7's smoke
passed on both platforms: it is now in `--help` and the unknown-provider
error like cursor, grok and gx, and an explicit `--provider native` run
persists it as the default the same way theirs would (plan 028 §3.5 already
made it indexed). These tests drive the real `craze` binary against a
loopback SSE fixture standing in for a real OpenAI-compatible provider
(sse_fixture.py) and check the CLI-visible contract: text streams and the
turn ends end_turn, `--help` and the unknown-provider error both say
"native", a run persists it to config.toml's provider and leaves every other
key alone, and a canary key never reaches stdout or stderr -- including when
the provider answers 401 or 500 with the key echoed back, the realistic leak
path (plan 018 §3.5).
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import threading
import time
import urllib.request
import warnings
from datetime import datetime, timezone
from pathlib import Path

import pytest

from conftest import require_rg, without_seq
from sse_fixture import (
    CANARY,
    UNUSED_ENV_KEY,
    RecordedRequest,
    SSEFixture,
    Step,
    ToolCall,
    answer,
    call_step,
    parallel_step,
    plan_path_in,
    prompt_is,
    sse_call_chunks,
    user_prompt,
    write_native_config,
)
from test_tui import PTYCraze, _ANSI, quit_craze


@pytest.fixture
def fixture_server():
    server = SSEFixture().start()
    try:
        yield server
    finally:
        server.close()


def parse_events(stdout: str) -> list[dict]:
    events = []
    for line in stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        events.append(json.loads(line))
    return events


def joined(events: list[dict], event_type: str) -> str:
    return "".join(e.get("text", "") for e in events if e.get("type") == event_type)


def last_tools(events: list[dict]) -> list[dict]:
    """The last object each tool row produced, one per call, in call order.

    A row is published on every update (internal/agent/native_tools.go), so
    the last object for a row id is the merged row as the call ended -- the
    one holding its result. The ids are the harness's own (turn.step.call,
    "t1.2.1"), not the provider's call ids, so a test that wants a particular
    call finds it by name.
    """
    out: dict[str, dict] = {}
    for e in events:
        if e.get("type") == "tool":
            out[e.get("id", "")] = e
    return list(out.values())


def native_env(craze_home: Path) -> dict[str, str]:
    """The child environment run_native builds, for a test that runs craze
    itself because it needs the process (a signal, a pid)."""
    env = os.environ.copy()
    env["CRAZE_HOME"] = str(craze_home)
    env.pop("CRAZE_PROVIDER", None)
    env.pop("CRAZE_AGENT_BIN", None)
    env.pop(UNUSED_ENV_KEY, None)
    return env


def run_native(
    craze_bin: Path,
    craze_home: Path,
    workspace: Path,
    *args: str,
    timeout: float = 10,
    json_mode: bool = True,
) -> subprocess.CompletedProcess[str]:
    # native_env drops UNUSED_ENV_KEY, the one variable providers.toml names
    # in env_keys: a developer or CI runner that happened to export it would
    # make the test pass for the wrong reason (the env var's key, not the
    # inline one the fixture actually expects).
    env = native_env(craze_home)
    cmd = [str(craze_bin), "prompt"]
    if json_mode:
        cmd.append("--json")
    cmd += ["--provider", "native", "--workspace", str(workspace), *args]
    return subprocess.run(
        cmd,
        check=False,
        capture_output=True,
        text=True,
        env=env,
        timeout=timeout,
    )


def test_native_prompt_streams_text_and_ends_end_turn(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    fixture_server.set_ok(
        text_parts=["hello ", "from native fixture"],
        reasoning_parts=["thinking it over"],
    )
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)

    proc = run_native(craze_bin, craze_home, tmp_path, "hi")
    assert proc.returncode == 0, proc.stderr

    events = parse_events(proc.stdout)
    assert joined(events, "text") == "hello from native fixture"
    assert joined(events, "thought") == "thinking it over"
    terminals = [e for e in events if e.get("type") in ("done", "error")]
    assert without_seq(events, terminals) == [{"type": "done", "stopReason": "end_turn"}], events
    assert CANARY not in proc.stdout
    assert CANARY not in proc.stderr


def test_native_prompt_model_flag_resolves_alias(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    fixture_server.set_ok(text_parts=["picked by alias"])
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url, alias="fixture-model")

    proc = run_native(craze_bin, craze_home, tmp_path, "--model", "fixture-model", "hi")
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    assert joined(events, "text") == "picked by alias"
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}


def test_native_prompt_reads_the_model_memory_and_never_writes_it(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """Plan 031 §3.4-§3.5, A1: `craze prompt` with no --model starts where a
    new TUI session would -- on the newest remembered model, at its
    remembered effort -- and `craze prompt --model X` runs X, at the effort
    remembered for X when X offers it (P6). Neither run writes recent.json:
    only a switch made inside a session does."""
    fixture_server.set_ok(text_parts=["ok"])
    craze_home = tmp_path / "craze-home"
    native = craze_home / "native"
    write_native_config(native, fixture_server.base_url)
    # A second model on the fixture's provider, with effort control.
    with (native / "models.toml").open("a", encoding="utf-8") as f:
        f.write(
            '\n[models."fixture-effort"]\n'
            'provider = "fixture"\n'
            'wire_model = "fixture-effort-wire"\n'
            'efforts = ["low", "high"]\n'
            'default_effort = "low"\n'
        )
    recent = native / "recent.json"
    recent.write_text(
        json.dumps(
            {
                "version": 1,
                "recent": [
                    {
                        "model": "fixture-effort",
                        "provider": "fixture",
                        "wire_model": "fixture-effort-wire",
                        "effort": "high",
                        "at": "2026-09-30T10:12:00Z",
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    before = recent.read_bytes()

    for args, wire, effort in [
        ((), "fixture-effort-wire", "high"),  # the memory's model and effort
        (("--model", "fixture-model"), "fixture-wire-model", None),  # the flag's model, no effort control
        (("--model", "fixture-effort"), "fixture-effort-wire", "high"),  # the flag's model, the memory's effort
    ]:
        proc = run_native(craze_bin, craze_home, tmp_path, *args, "hi")
        assert proc.returncode == 0, proc.stderr
        body = fixture_server.requests[-1].body
        assert body.get("model") == wire, (args, body.get("model"))
        assert body.get("reasoning_effort") == effort, (args, body.get("reasoning_effort"))
        assert recent.read_bytes() == before, args


def user_contents(request) -> list[str]:
    """Every user message of a recorded request, as text.

    openai-compat sends a user message's content as a string, but a provider
    shape that sent the parts array would still be readable here: what the
    case is about is the bytes craze put in the body, not how they were
    framed.
    """
    out = []
    for m in request.messages:
        if m.get("role") != "user":
            continue
        content = m.get("content")
        if isinstance(content, str):
            out.append(content)
        elif isinstance(content, list):
            out.append("".join(p.get("text", "") for p in content if isinstance(p, dict)))
    return out


def test_native_expands_a_project_command_into_the_request_body(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """Plan 022 §3.3 from outside the process: a command file under the
    workspace's own .claude is found at Start, a draft naming it is expanded,
    and what reaches the provider is the typed draft followed by the block --
    with the arguments and ${CLAUDE_PLUGIN_ROOT} substituted, and the source
    named as this project rather than as a plugin.

    HOME is the suite's isolated one (conftest.isolate_run_env), so the user
    half of the scan finds nothing and the rows here are the workspace's.
    """
    fixture_server.set_ok(text_parts=["expanded"])
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    ws = tmp_path / "ws"
    (ws / ".claude" / "commands").mkdir(parents=True)
    (ws / ".claude" / "commands" / "ship.md").write_text(
        "---\ndescription: ship it\n---\nShip $ARGUMENTS from ${CLAUDE_PLUGIN_ROOT}.\n",
        encoding="utf-8",
    )

    proc = run_native(craze_bin, craze_home, ws, "/ship v2")
    assert proc.returncode == 0, proc.stdout + proc.stderr

    events = parse_events(proc.stdout)
    commands = [e for e in events if e.get("type") == "command"]
    assert len(commands) == 1, events
    assert commands[0]["qualified"] == "project:ship", commands[0]
    assert commands[0]["plugin"] == "project", commands[0]
    assert commands[0]["kind"] == "command", commands[0]
    assert commands[0]["path"] == str(ws / ".claude" / "commands" / "ship.md"), commands[0]

    assert len(fixture_server.requests) == 1
    users = user_contents(fixture_server.requests[0])
    assert len(users) == 1, fixture_server.requests[0].messages
    body = users[0]
    assert body.startswith("/ship v2\n\n"), body
    assert "Ship v2 from " in body, body
    assert "from this project" in body, body
    # What the event announced is exactly what the request carried.
    assert body == "/ship v2\n\n" + commands[0]["text"], body
    # A command name typed mid-line is not a reference (§3.3): the scan would
    # otherwise expand a body for every `cd /ship` in a draft.
    assert body.count("<command ") == 1, body


def test_native_mid_line_reference_is_not_expanded(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """The other half of that rule, end to end: `cd /ship && make` reaches the
    provider as it was typed, with no block behind it and no command event."""
    fixture_server.set_ok(text_parts=["nothing expanded"])
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    ws = tmp_path / "ws"
    (ws / ".claude" / "commands").mkdir(parents=True)
    (ws / ".claude" / "commands" / "ship.md").write_text(
        "---\ndescription: ship it\n---\nship body\n", encoding="utf-8"
    )

    proc = run_native(craze_bin, craze_home, ws, "cd /ship && make")
    assert proc.returncode == 0, proc.stdout + proc.stderr

    events = parse_events(proc.stdout)
    assert [e for e in events if e.get("type") == "command"] == [], events
    assert user_contents(fixture_server.requests[0]) == ["cd /ship && make"]


def system_text(request) -> str:
    """The frozen system prompt as one request carried it.

    A request leads with it, so this is what every turn of a session sends
    ahead of anything the user wrote (plan 022 §3.4). It is read out of
    ``RecordedRequest.body`` rather than off the process, because the whole
    question here is what left craze.
    """
    for m in request.messages:
        if m.get("role") != "system":
            continue
        content = m.get("content")
        if isinstance(content, str):
            return content
        if isinstance(content, list):
            return "".join(p.get("text", "") for p in content if isinstance(p, dict))
    raise AssertionError(f"no system message in {request.messages}")


def write_compat_workspace(tmp_path: Path) -> Path:
    """A workspace and a Claude home holding one of everything the prompt can
    draw from: an instruction file, a rule, a project command, a project skill,
    and one enabled plugin shipping a command of its own.

    The home is the suite's isolated HOME (conftest.isolate_run_env), which is
    what a native session reads the user's own content under. The workspace
    gets a .git so the chain stops there: without one the walk climbs toward
    the filesystem root looking for a repository, and what it found would
    depend on where the temporary directory happened to be.
    """
    ws = tmp_path / "ws"
    (ws / ".claude" / "commands").mkdir(parents=True)
    (ws / ".claude" / "rules").mkdir(parents=True)
    (ws / ".claude" / "skills" / "build").mkdir(parents=True)
    (ws / ".git").mkdir()
    (ws / "CLAUDE.md").write_text("project instructions here\n", encoding="utf-8")
    (ws / ".claude" / "rules" / "style.md").write_text("project rule here\n", encoding="utf-8")
    (ws / ".claude" / "commands" / "ship.md").write_text(
        "---\ndescription: ship it\n---\nShip $ARGUMENTS.\n", encoding="utf-8"
    )
    (ws / ".claude" / "skills" / "build" / "SKILL.md").write_text(
        "---\nname: build\ndescription: build the thing\n---\nbody\n", encoding="utf-8"
    )

    home = Path(os.environ["HOME"]) / ".claude"
    install = home / "plugins" / "cache" / "pack"
    (install / "commands").mkdir(parents=True)
    (install / "commands" / "deploy.md").write_text(
        "---\ndescription: deploy it\n---\nDeploy.\n", encoding="utf-8"
    )
    (home / "plugins").mkdir(parents=True, exist_ok=True)
    (home / "plugins" / "installed_plugins.json").write_text(
        json.dumps({"version": 2, "plugins": {"pack@mkt": [{"scope": "user", "installPath": str(install)}]}}),
        encoding="utf-8",
    )
    (home / "settings.json").write_text(
        json.dumps({"enabledPlugins": {"pack@mkt": True}}), encoding="utf-8"
    )
    return ws


# What each class of content looks like in the prompt, for the toggle table
# below: the marker a class is present by, and absent by.
COMPAT_MARKERS = {
    "instructions": "project instructions here",
    "rules": "project rule here",
    "skills": "- build (skill): build the thing",
    "commands": "- ship (command): ship it",
    "plugins": "- deploy (command): deploy it",
}


def test_native_instructions_and_catalog_reach_the_system_prompt(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """Plan 022 §3.4 from outside the process: what craze read off disk is in
    the system message of the first request, under craze's own framing, with
    the menu's names and the absolute paths that load them."""
    fixture_server.set_ok(text_parts=["read it"])
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    ws = write_compat_workspace(tmp_path)

    proc = run_native(craze_bin, craze_home, ws, "hi")
    assert proc.returncode == 0, proc.stdout + proc.stderr

    sent = system_text(fixture_server.requests[0])
    assert "# Project and user instructions" in sent, sent
    assert f"## From: {ws / 'CLAUDE.md'}" in sent, sent
    assert "# Skills and commands" in sent, sent
    for marker in COMPAT_MARKERS.values():
        assert marker in sent, (marker, sent)
    # A row is only worth listing because the model can open it.
    assert f"  Path: {ws / '.claude' / 'commands' / 'ship.md'}" in sent, sent
    # Root is a plugin's alone: the project's own entries have none (§3.4).
    assert sent.count("  Root: ") == 1, sent
    assert CANARY not in proc.stdout and CANARY not in proc.stderr


def test_native_never_prints_an_identity_holding_a_key(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A8's `--json` half, against the real binary.

    A provider key can be embedded in an *identity* — a plugin id, a
    frontmatter name, a directory a skill lives in — and an identity cannot be
    redacted without breaking the thing it identifies: a marker where a name
    was answers to nothing the user could type, and a marker where a path was
    opens no file. So craze suppresses the whole entry (§3.4), and neither the
    system prompt nor the `--json` stream nor the diagnostics on stderr ever
    carries it.

    The controls are the point: a clean command in the same workspace is
    listed and expands, so a run that printed nothing at all could not pass
    here. `/deploy` is typed although the key is nowhere in that name — the
    user never sees a plugin's id — which is the route the event's `plugin`
    and `qualified` fields leaked through before the gate existed.
    """
    fixture_server.set_ok(text_parts=["nothing leaked"])
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)

    ws = tmp_path / "ws"
    (ws / ".git").mkdir(parents=True)
    (ws / ".claude" / "commands").mkdir(parents=True)
    (ws / ".claude" / "commands" / "ship.md").write_text(
        "---\ndescription: ship it\n---\nShip $ARGUMENTS.\n", encoding="utf-8"
    )
    # A skill whose directory, and therefore whose name and path, is the key.
    keyed_skill = ws / ".claude" / "skills" / CANARY
    keyed_skill.mkdir(parents=True)
    (keyed_skill / "SKILL.md").write_text(
        f"---\nname: {CANARY}\ndescription: named after the key\n---\nbody\n", encoding="utf-8"
    )
    # And an enabled plugin whose id is the key, shipping an ordinary command.
    home = Path(os.environ["HOME"]) / ".claude"
    install = home / "plugins" / "cache" / "keyed"
    (install / "commands").mkdir(parents=True)
    (install / "commands" / "deploy.md").write_text(
        "---\ndescription: deploy it\n---\nDeploy.\n", encoding="utf-8"
    )
    (home / "plugins" / "installed_plugins.json").write_text(
        json.dumps(
            {
                "version": 2,
                "plugins": {f"{CANARY}@mkt": [{"scope": "user", "installPath": str(install)}]},
            }
        ),
        encoding="utf-8",
    )
    (home / "settings.json").write_text(
        json.dumps({"enabledPlugins": {f"{CANARY}@mkt": True}}), encoding="utf-8"
    )

    proc = run_native(craze_bin, craze_home, ws, "/ship v2\n/deploy")
    assert proc.returncode == 0, proc.stdout + proc.stderr

    assert CANARY not in proc.stdout, proc.stdout
    assert CANARY not in proc.stderr, proc.stderr

    events = parse_events(proc.stdout)
    commands = [e for e in events if e.get("type") == "command"]
    assert len(commands) == 1, events
    assert commands[0]["name"] == "ship", commands[0]
    for field in ("name", "qualified", "plugin", "kind", "path", "text"):
        assert CANARY not in commands[0].get(field, ""), commands[0]

    sent = system_text(fixture_server.requests[0])
    assert "- ship (command): ship it" in sent, sent
    for gone in ("named after the key", "deploy it", CANARY):
        assert gone not in sent, (gone, sent)


@pytest.mark.parametrize("toggle", sorted(COMPAT_MARKERS))
def test_native_compat_toggle_removes_its_class(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture, toggle: str
) -> None:
    """A13 end to end: `[compat.claude]` in craze's own config.toml removes
    exactly its row of §3.5's matrix from what reaches the provider, and
    nothing else — including `instructions`, which takes the rules with it
    because they are part of the same section, and `commands`, which does not
    take a plugin's commands with it because `plugins` governs those.
    """
    fixture_server.set_ok(text_parts=["toggled"])
    craze_home = tmp_path / "craze-home"
    craze_home.mkdir(parents=True, exist_ok=True)
    write_native_config(craze_home / "native", fixture_server.base_url)
    (craze_home / "config.toml").write_text(
        f"[compat.claude]\n{toggle} = false\n", encoding="utf-8"
    )
    ws = write_compat_workspace(tmp_path)

    proc = run_native(craze_bin, craze_home, ws, "hi")
    assert proc.returncode == 0, proc.stdout + proc.stderr

    sent = system_text(fixture_server.requests[0])
    gone = {toggle} | ({"rules"} if toggle == "instructions" else set())
    for name, marker in COMPAT_MARKERS.items():
        if name in gone:
            assert marker not in sent, (name, sent)
        else:
            assert marker in sent, (name, sent)
    if toggle == "instructions":
        assert "# Project and user instructions" not in sent, sent


def test_native_compat_non_bool_is_the_default_and_one_line(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """§3.5's one departure from craze's other config switches: a value that is
    not a bool leaves the class on and says so, because a typo there would
    otherwise look like craze having lost the user's own instructions."""
    fixture_server.set_ok(text_parts=["still on"])
    craze_home = tmp_path / "craze-home"
    craze_home.mkdir(parents=True, exist_ok=True)
    write_native_config(craze_home / "native", fixture_server.base_url)
    (craze_home / "config.toml").write_text(
        '[compat.claude]\nskills = "no"\n', encoding="utf-8"
    )
    ws = write_compat_workspace(tmp_path)

    proc = run_native(craze_bin, craze_home, ws, "hi")
    assert proc.returncode == 0, proc.stdout + proc.stderr
    assert "compat.claude.skills is not a bool" in proc.stderr, proc.stderr
    assert COMPAT_MARKERS["skills"] in system_text(fixture_server.requests[0])


def test_native_is_in_help_and_unknown_provider_error(
    craze_bin: Path, tmp_path: Path
) -> None:
    """Plan 028 §3.16/D-65: native is listed like cursor, grok and gx.

    `--help`'s --provider flag names it -- as the one that runs inside craze,
    not one of the ACP agents (C19a) -- and an unknown --provider's error
    offers it alongside the others.
    """
    help_proc = subprocess.run(
        [str(craze_bin), "--help"], capture_output=True, text=True, timeout=5
    )
    assert help_proc.returncode == 0, help_proc.stderr
    assert (
        "cursor, grok, gx (ACP agents) or native (runs inside craze)"
        in help_proc.stdout
    ), help_proc.stdout
    assert "ACP provider" not in help_proc.stdout, help_proc.stdout

    unknown_proc = subprocess.run(
        [
            str(craze_bin),
            "prompt",
            "--provider",
            "nope",
            "--workspace",
            str(tmp_path),
            "hi",
        ],
        capture_output=True,
        text=True,
        timeout=5,
    )
    assert unknown_proc.returncode == 2
    assert "unknown provider" in unknown_proc.stderr
    assert "cursor, grok, gx, or native" in unknown_proc.stderr, unknown_proc.stderr
    assert unknown_proc.stdout == ""


def test_native_run_persists_provider_and_leaves_sessions_alone(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """Plan 028 §3.16/D-65: native is listed, so `craze prompt --provider
    native` persists it as the default the same way cursor or grok would,
    leaving every other config key alone. `craze prompt` still writes no
    session index, listed or not (§2.5)."""
    fixture_server.set_ok(text_parts=["ok"])
    craze_home = tmp_path / "craze-home"
    craze_home.mkdir()
    config_path = craze_home / "config.toml"
    before = 'provider = "grok"\ntheme = "tokyo-night"\n'
    config_path.write_text(before, encoding="utf-8")
    write_native_config(craze_home / "native", fixture_server.base_url)

    proc = run_native(craze_bin, craze_home, tmp_path, "hi")
    assert proc.returncode == 0, proc.stderr

    assert config_path.read_text(encoding="utf-8") == 'provider = "native"\ntheme = "tokyo-night"\n'
    assert not (craze_home / "sessions.jsonl").exists()


@pytest.mark.parametrize("mode", ["ok", "unauthorized", "servererror"])
def test_native_canary_never_leaks(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture, mode: str
) -> None:
    """A canary key placed in native/providers.toml appears nowhere in stdout
    or stderr, whether the fixture answers 200, 401, or 500 -- the last two
    with the Authorization header echoed straight into the error body
    (package llm's scrub.go is what has to catch that).
    """
    if mode == "ok":
        fixture_server.set_ok(text_parts=["clean turn"])
    elif mode == "unauthorized":
        fixture_server.set_unauthorized()
    else:
        fixture_server.set_server_error()

    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)

    proc = run_native(craze_bin, craze_home, tmp_path, "hi", timeout=15)

    # Sanity: the wire really did carry the canary, so its absence below is
    # the scrubber's doing and not an empty response no one looked at.
    assert fixture_server.requests, "the fixture server saw no requests"
    assert any(CANARY in r.authorization for r in fixture_server.requests), fixture_server.requests

    assert CANARY not in proc.stdout, proc.stdout
    assert CANARY not in proc.stderr, proc.stderr

    events = parse_events(proc.stdout)
    if mode == "ok":
        assert proc.returncode == 0, proc.stderr
        assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}
    else:
        assert proc.returncode == 1, proc.stdout + proc.stderr
        errors = [e for e in events if e.get("type") == "error"]
        assert errors, events
        assert CANARY not in errors[0]["message"]
        if mode == "servererror":
            # 401 gets the adapter's own fixed phrasing (phraseTurnError's
            # ErrAuth branch), which never repeats the provider's message at
            # all; 500 falls through to the generic branch, which does
            # include it -- scrubbed, in place of the echoed Authorization
            # header -- so this is where the scrub is actually exercised.
            assert "[redacted]" in errors[0]["message"], errors[0]
        assert not any(e.get("type") == "done" for e in events), events


def test_native_agent_bin_is_a_usage_error(craze_bin: Path, tmp_path: Path) -> None:
    craze_home = tmp_path / "craze-home"
    proc = run_native(
        craze_bin, craze_home, tmp_path, "--agent-bin", "/bin/true", "hi", json_mode=False
    )
    assert proc.returncode == 2, proc.stderr
    assert "--agent-bin" in proc.stderr and "native" in proc.stderr, proc.stderr
    assert proc.stdout == ""


# --- Plan 019 C11: tool loops, end to end ---------------------------------
#
# From here on the fixture is scripted (sse_fixture.Step): one response per
# request, so a turn's whole loop -- call, run, feed the result back, call
# again -- is declared as a list. These drive the real tools against a real
# temporary workspace through the real binary; the Go tests own the merge and
# the projection, and these own "it works from outside, over HTTP".

# The statuses a settled row may carry: completed and failed are a call that
# reported how it ended, cancelled is settleTools closing one that never did.
TERMINAL = ("completed", "failed", "cancelled")


def tool_workspace(tmp_path: Path) -> Path:
    """A workspace the scripted loops below have something to do in."""
    ws = tmp_path / "ws"
    ws.mkdir()
    (ws / "notes.txt").write_text("alpha\n", encoding="utf-8")
    (ws / "main.go").write_text("package main\n\n// TODO: ship it\n", encoding="utf-8")
    return ws


def tree(root: Path) -> dict[str, bytes]:
    """Every file under root by relative path, with its bytes.

    It is what "no workspace file changed" is asserted with (A6): a path that
    appeared, one that went and one whose contents moved all show up as a
    difference between two of these.
    """
    return {
        str(p.relative_to(root)): p.read_bytes()
        for p in sorted(root.rglob("*"))
        if p.is_file()
    }


def test_native_tool_loop_read_grep_edit_bash(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """Four tools in one turn, as `craze prompt --json` prints them.

    One step per tool, each answered over the wire with a real tool_calls
    delta, so what is exercised is the whole path: openaicompat's accumulator,
    the harness's dispatcher, the adapter's merge and toolJSON's projection.
    The turn ends end_turn only if every step's results really went back to
    the model, which the request bodies below then confirm directly.
    """
    require_rg()
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    fixture_server.set_script(
        [
            call_step("read", {"filePath": "notes.txt"}),
            call_step("grep", {"pattern": "TODO", "path": "."}),
            call_step(
                "edit",
                {"filePath": "notes.txt", "oldString": "alpha", "newString": "beta"},
            ),
            call_step("bash", {"command": "cat notes.txt"}),
            answer("all four ran"),
        ]
    )

    proc = run_native(craze_bin, craze_home, ws, "do the four", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert joined(events, "text") == "all four ran"
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}, events[-3:]

    # Every scripted step was taken and nothing asked for a sixth.
    assert fixture_server.script_remaining == 0
    assert fixture_server.unscripted == 0
    assert len(fixture_server.requests) == 5, len(fixture_server.requests)

    rows = last_tools(events)
    assert [r["name"] for r in rows] == ["read", "grep", "edit", "bash"], rows
    assert [r["status"] for r in rows] == ["completed"] * 4, rows
    by_name = {r["name"]: r for r in rows}

    read = by_name["read"]
    assert read["kind"] == "read" and read["title"] == "notes.txt", read
    assert "alpha" in read["output"]["content"], read["output"]

    # A search row draws its query and nothing else: read is the one tool
    # whose result fills Output.Content (plan 019 §3.10), so a search row
    # carries no output object at all. What ripgrep found is asserted below,
    # where it goes -- back to the model.
    grep = by_name["grep"]
    assert grep["kind"] == "search" and grep["title"] == "TODO in .", grep
    assert "output" not in grep, grep

    edit = by_name["edit"]
    assert edit["kind"] == "edit", edit
    assert edit["diffs"] == [
        {"path": str(ws / "notes.txt"), "added": 1, "removed": 1, "truncated": False}
    ], edit["diffs"]

    cmd = by_name["bash"]
    assert cmd["kind"] == "execute" and cmd["title"] == "cat notes.txt", cmd
    assert cmd["output"]["exitCode"] == 0, cmd["output"]
    # The edit really happened on disk, and the command that ran after it saw
    # the edited file -- which is a loop, not four independent calls.
    assert "beta" in cmd["output"]["stdout"], cmd["output"]
    assert (ws / "notes.txt").read_text(encoding="utf-8") == "beta\n"

    # The wire's own view of the same loop. The first request offers the
    # profile's eleven tools in its registry order (opencode.Profile) -- a
    # headless session runs no background jobs, so it is offered neither
    # bash_output nor bash_stop (plan 033 X101) -- and the last one carries
    # every call's result back, each tied to its call id.
    first, last = fixture_server.requests[0], fixture_server.requests[-1]
    assert first.tool_names == [
        "bash",
        "read",
        "glob",
        "grep",
        "edit",
        "write",
        "agent",
        "agent_output",
        "todo_write",
        "ask_user_question",
        "exit_plan_mode",
    ], first.tool_names
    # Its bash is the one without jobs: no run_in_background parameter and no
    # word of it in the description. workdir is the control: the schema read
    # is bash's own.
    bash_fn = next(t["function"] for t in first.body["tools"] if t["function"]["name"] == "bash")
    assert "workdir" in bash_fn["parameters"]["properties"], bash_fn["parameters"]
    assert "run_in_background" not in bash_fn["parameters"]["properties"], bash_fn["parameters"]
    assert "run_in_background" not in bash_fn["description"], bash_fn["description"]
    results = {m["tool_call_id"]: m["content"] for m in last.messages if m.get("role") == "tool"}
    assert set(results) == {"read", "grep", "edit", "bash"}, sorted(results)
    assert "alpha" in results["read"], results["read"]
    assert "main.go" in results["grep"], results["grep"]  # ripgrep really ran
    assert "beta" in results["bash"], results["bash"]


def test_native_tool_loop_without_rg_is_still_a_loop(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """The negative control for the test above's ripgrep dependency: the same
    machinery with two calls instead of four still ends end_turn, so a
    failure up there is about read/grep/edit/bash and not about scripting a
    loop at all. It needs no ripgrep, so it runs on every machine.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    fixture_server.set_script(
        [
            call_step("read", {"filePath": "main.go"}),
            call_step("bash", {"command": "echo two"}),
            answer("two ran"),
        ]
    )

    proc = run_native(craze_bin, craze_home, ws, "two of them", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert joined(events, "text") == "two ran"
    assert len(fixture_server.requests) == 3
    rows = last_tools(events)
    assert [(r["name"], r["status"]) for r in rows] == [
        ("read", "completed"),
        ("bash", "completed"),
    ], rows


def test_native_question_is_auto_answered_and_reaches_the_model(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A question from the harness's ask_user_question, through the whole
    headless path (plan 023 §3.4, A6).

    `craze prompt` answers questions itself with each question's first option
    -- one policy across providers, not a consequence of having no frontend
    (plan 021 §3.6) -- so no card is ever raised: the registry writes the Auto
    opening carrying the answer it sent, and the stream prints it as a
    `question` line. What the model gets back is the *label*, not the option id
    the card would have answered with: the seam answers by index and the tool
    words it for the model (tool.Answers).

    The todo list rides along in the same turn, since it is the other H5 tool a
    headless run reaches: it prints as a `todos` line with the harness's own
    statuses. exit_plan_mode is not here -- the gate refuses it by name outside
    plan mode, and no mode can be entered on native until plan 023's PR 2.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    fixture_server.set_script(
        [
            call_step(
                "todo_write",
                {"todos": [{"id": "1", "content": "ask the user", "status": "in_progress"}]},
            ),
            call_step(
                "ask_user_question",
                {
                    "questions": [
                        {
                            "question": "Which shall it be?",
                            "header": "pick one",
                            "options": [
                                {"label": "alpha", "description": "the first"},
                                {"label": "beta"},
                            ],
                        }
                    ]
                },
            ),
            answer("went with alpha"),
        ]
    )

    proc = run_native(craze_bin, craze_home, ws, "ask me", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert joined(events, "text") == "went with alpha"
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}, events[-3:]
    assert CANARY not in proc.stdout and CANARY not in proc.stderr

    questions = [e for e in events if e["type"] == "question"]
    assert len(questions) == 1, questions
    q = questions[0]
    assert q["id"] == "ask-1" and q["title"] == "pick one", q
    # Answered by craze itself, with the first option, and said so in the
    # opening -- which is exactly what cursor's headless questions print.
    assert q["auto"] is True and q["answers"] == {"q1": ["o1"]}, q

    todos = [e for e in events if e["type"] == "todos"]
    assert len(todos) == 1, todos
    assert todos[0]["todos"] == [
        {"id": "1", "content": "ask the user", "status": "in_progress"}
    ], todos[0]

    # The wire's own view: the last request carries both results, and the
    # answer is the label the model wrote, not the id it was answered by.
    assert len(fixture_server.requests) == 3, len(fixture_server.requests)
    results = {
        m["tool_call_id"]: m["content"]
        for m in fixture_server.requests[-1].messages
        if m.get("role") == "tool"
    }
    assert set(results) == {"todo_write", "ask_user_question"}, sorted(results)
    assert '"Which shall it be?"="alpha"' in results["ask_user_question"], results["ask_user_question"]
    assert "o1" not in results["ask_user_question"], results["ask_user_question"]
    assert "ask the user" in results["todo_write"], results["todo_write"]


# The plan text the scripted model writes below. The first heading becomes the
# plan ask's name, which is what the `plan` line prints.
PLAN_TEXT = "# Ship the widget\n\n1. read main.go\n2. write the widget\n"


def test_native_plan_mode_ends_the_turn_and_leaves_the_workspace_alone(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """`craze prompt --provider native --plan --json`, end to end (plan 023 A6).

    --plan reaches the harness now (refuseInProcess is keyed on the provider's
    modes), so this is the whole of plan mode from outside: the reminder in the
    first request names the plan file, the model writes to it -- the one
    edit-kind call the gate allows there -- and calls exit_plan_mode; `craze
    prompt` accepts the plan itself, as it does for cursor, and the turn ENDS
    there, which is what makes a headless --plan mean plan-only (D-51).

    The plan file's path is read out of the request rather than guessed: it is
    the transcript's sibling under the craze home and its name carries the
    session's own id.

    "No file under the workspace changed" is the acceptance criterion this is
    for: the plan and the transcript live under the home, and the repository the
    run was pointed at is untouched.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)

    seen: list[str] = []

    def write_the_plan(request: RecordedRequest) -> Step:
        path = plan_path_in(request)
        seen.append(path)
        # An empty path would write to "" and fail loudly rather than
        # silently scripting something else.
        return call_step("write", {"filePath": path, "content": PLAN_TEXT})

    # Two entries and no more: a third request means the approval did not end
    # the turn, and the fixture answers it with a 400 that says so.
    fixture_server.set_script([write_the_plan, call_step("exit_plan_mode", {})])

    before = tree(ws)
    proc = run_native(craze_bin, craze_home, ws, "--plan", "plan the widget", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}, events[-3:]
    assert CANARY not in proc.stdout and CANARY not in proc.stderr

    # The reminder really carried the path, and it is under the craze home.
    assert seen and seen[0].endswith(".plan.md"), seen
    plan_file = Path(seen[0])
    assert plan_file.is_relative_to(craze_home), (plan_file, craze_home)
    assert plan_file.read_text(encoding="utf-8") == PLAN_TEXT

    # The plan opening, answered by craze's own policy exactly as cursor's is.
    plans = [e for e in events if e["type"] == "plan"]
    assert len(plans) == 1, plans
    assert plans[0]["id"] == "plan-1", plans[0]
    assert plans[0]["auto"] is True and plans[0]["accepted"] is True, plans[0]
    assert plans[0]["name"] == "Ship the widget", plans[0]

    # Two requests: the one the plan was written from and the one that offered
    # it. The approval's own result reaches no third, because the turn ended.
    assert len(fixture_server.requests) == 2, len(fixture_server.requests)
    assert fixture_server.script_remaining == 0 and fixture_server.unscripted == 0
    wire = json.dumps([r.body for r in fixture_server.requests])
    assert "The user approved the plan" not in wire

    # The write reached the plan file and was allowed there.
    results = {
        m["tool_call_id"]: m["content"]
        for m in fixture_server.requests[-1].messages
        if m.get("role") == "tool"
    }
    assert set(results) == {"write"}, sorted(results)
    assert "Rejected" not in results["write"], results["write"]

    assert tree(ws) == before, "plan mode wrote to the workspace"


def test_native_ask_mode_denies_a_write_and_leaves_the_workspace_alone(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """`--ask` on native: every non-read-only call is denied by the gate, the
    model is told why in the next request's tool result, and the workspace is
    untouched (plan 023 A1, A6).
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    fixture_server.set_script(
        [
            call_step("write", {"filePath": "notes.txt", "content": "beta\n"}),
            answer("I cannot write in ask mode"),
        ]
    )

    before = tree(ws)
    proc = run_native(craze_bin, craze_home, ws, "--ask", "what does notes.txt say", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert joined(events, "text") == "I cannot write in ask mode"
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}, events[-3:]

    rows = last_tools(events)
    assert [(r["name"], r["status"]) for r in rows] == [("write", "failed")], rows

    assert len(fixture_server.requests) == 2, len(fixture_server.requests)
    results = {
        m["tool_call_id"]: m["content"]
        for m in fixture_server.requests[-1].messages
        if m.get("role") == "tool"
    }
    assert "ask mode is read-only" in results["write"], results["write"]

    assert tree(ws) == before, "ask mode wrote to the workspace"


def test_sse_fixture_interleaves_parallel_calls() -> None:
    """The fixture's own wire order, pinned (plan 019 C11).

    The test below is only worth two parametrizations if the two really are
    two shapes, and that is a property of the bytes, not of the flag: the
    interleaved body must open both calls before either one's arguments
    follow, which is what a reader keyed by anything other than `index`
    cannot reassemble. Without this, the layout could quietly collapse back
    into one order and both cases would still pass.
    """
    calls = (ToolCall("a", "read", '{"filePath":"main.go"}'), ToolCall("b", "read", '{"filePath":"notes.txt"}'))

    def shape(step) -> list[str]:
        """Each tool_calls delta as "<index>:head" or "<index>:args"."""
        out = []
        for chunk in sse_call_chunks(step):
            d = json.loads(chunk)["choices"][0]["delta"]["tool_calls"][0]
            out.append(f"{d['index']}:{'head' if 'name' in d['function'] else 'args'}")
        return out

    assert shape(parallel_step(*calls)) == [
        "0:head", "1:head", "0:args", "1:args", "0:args", "1:args",
    ]
    assert shape(parallel_step(*calls, interleaved=False)) == [
        "0:head", "0:args", "0:args", "1:head", "1:args", "1:args",
    ]


@pytest.mark.parametrize("interleaved", [True, False], ids=["interleaved", "sequential"])
def test_native_two_calls_in_one_step(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture, interleaved: bool
) -> None:
    """Two calls in one response, each on its own tool_calls index.

    openaicompat accumulates a streamed call by index, not by id and not by
    "the call opened last", so both wire orders a provider may send must
    arrive as two whole calls. The interleaved case -- head-0, head-1,
    args-0, args-1 -- is the one that can tell those readers apart: a reader
    ignoring index would join index 1's arguments onto index 0 and lose a
    call. The sequential case is the other order providers really send.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    fixture_server.set_script(
        [
            parallel_step(
                ToolCall("a", "read", '{"filePath":"main.go"}'),
                ToolCall("b", "read", '{"filePath":"notes.txt"}'),
                interleaved=interleaved,
            ),
            answer("both ran"),
        ]
    )

    proc = run_native(craze_bin, craze_home, ws, "read both", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert joined(events, "text") == "both ran"
    assert len(fixture_server.requests) == 2

    rows = last_tools(events)
    assert len(rows) == 2, rows
    assert {r["title"] for r in rows} == {"main.go", "notes.txt"}, rows
    assert [r["status"] for r in rows] == ["completed", "completed"], rows

    results = {
        m["tool_call_id"]: m["content"]
        for m in fixture_server.requests[-1].messages
        if m.get("role") == "tool"
    }
    assert set(results) == {"a", "b"}, sorted(results)
    assert "package main" in results["a"], results["a"]
    assert "alpha" in results["b"], results["b"]


def test_native_unscripted_request_fails_the_turn(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """The fixture's own control: a turn that asks for one request more than
    the script has must fail loudly, not quietly reuse the last answer.
    Without this, a scripted loop that stopped early would still look green.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    # One call and no answer behind it: the harness sends the result back and
    # there is nothing scripted for that second request.
    fixture_server.set_script([call_step("bash", {"command": "echo one"})])

    proc = run_native(craze_bin, craze_home, ws, "go", timeout=60)
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert fixture_server.unscripted == 1
    events = parse_events(proc.stdout)
    errors = [e for e in events if e.get("type") == "error"]
    assert errors, events
    assert "no scripted response for request 1" in errors[0]["message"], errors[0]
    assert not any(e.get("type") == "done" for e in events), events


def test_native_max_tokens_mid_call_leaves_no_pending_row(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A response that hits the output ceiling while the arguments are still
    streaming (plan 019 §3.10's settling case).

    The wire carries a tool_calls delta whose arguments are cut off and a
    finish_reason of "length": openaicompat announces the call
    (ToolInputStart) and then suppresses it, so the harness sees a call that
    never arrived and settles it, and the adapter closes the row. The turn
    ends max_tokens, and `craze prompt` exits 1 for any stop but end_turn.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    fixture_server.set_script(
        [call_step("read", '{"filePath":"not', call_id="cut-off", finish="length")]
    )

    proc = run_native(craze_bin, craze_home, ws, "go", timeout=60)
    assert proc.returncode == 1, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "max_tokens"}, events[-3:]

    # The row opened pending, as a call whose arguments are still streaming
    # does, and was closed -- not left pending -- by the harness settling a
    # call that never arrived, in the words it hands the model.
    rows = [e for e in events if e.get("type") == "tool"]
    assert [r["status"] for r in rows] == ["pending", "failed"], rows
    assert "output token limit" in rows[-1]["output"]["content"], rows[-1]
    # One request and no more: a suppressed call is not a tool-calls finish,
    # so the turn ended rather than looping on a call that never arrived.
    assert len(fixture_server.requests) == 1


# The cancelled command's two processes. It backgrounds a child and waits,
# so the group really has both -- a shell that ran a single command could be
# exec-optimized into it, and then "the leader is gone" would say nothing
# about a child left behind, which is the leak that matters (the bash tool
# kills the group, not the leader: opencode/process.go).
#
# Each process is matched on its pid *and* a marker in its argv, as
# internal/harness/tool/opencode/bash_test.go's alive does, so a pid the OS
# has since handed to something else reads as gone rather than as a leak.
# The child's marker is its own distinctive sleep; the leader's is a token
# only the shell's argv carries. The sleep is long enough that nothing here
# can pass because the command simply ended.
CANCEL_CHILD_MARKER = "sleep 9771"
CANCEL_LEADER_MARKER = "craze-c11-cancel-leader"


def test_native_cancel_during_bash_reports_cancelled_and_leaves_nothing_running(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """SIGINT while a command runs: the turn says it was cancelled, the row
    is closed, and both processes of the command's group are gone.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    leader_file, child_file = ws / "leader.pid", ws / "child.pid"
    command = (
        f"{CANCEL_CHILD_MARKER} & echo $! > {child_file.name}; "
        f"echo $$ > {leader_file.name}; wait  # {CANCEL_LEADER_MARKER}"
    )
    fixture_server.set_script(
        [call_step("bash", {"command": command}), answer("never reached")]
    )

    proc = subprocess.Popen(
        [
            str(craze_bin),
            "prompt",
            "--json",
            "--provider",
            "native",
            "--workspace",
            str(ws),
            "go",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=native_env(craze_home),
    )
    leader = child = 0
    leaked: list[str] = []
    try:
        # A pid file that never appears fails here rather than leaving the
        # checks below with nothing to look at.
        leader = wait_for_pid(leader_file, proc)
        child = wait_for_pid(child_file, proc)
        # The control for the `gone` calls: both are running now, so finding
        # nothing afterwards is finding killed processes and not pids this
        # test never could see. The child's pid is written as soon as the
        # shell has forked it, so it is awaited as itself, bounded, rather
        # than read once: a process can read as no command line at all for
        # the instant of its exec (plan 036: CI's Linux push run of 69fb29b
        # failed one such read, the shell already seen alive). A child that
        # really died fails the wait just as it failed the read, and says
        # whether craze was still running.
        assert alive(leader, CANCEL_LEADER_MARKER), f"the shell ({leader}) was not running"
        assert until_alive(child, CANCEL_CHILD_MARKER), (
            f"the child ({child}) was not running; craze "
            + ("still running" if proc.poll() is None else f"exited {proc.returncode}")
            + f"; {until_alive.evidence}"
        )

        proc.send_signal(signal.SIGINT)
        stdout, stderr = proc.communicate(timeout=60)

        assert proc.returncode != 0, stdout + stderr
        events = parse_events(stdout)
        # Either shape reports the cancel: the session's own EventDone when
        # its Cancel won the race with the signal's context, and an error
        # saying "prompt cancelled" when the caller's context did (native.go's
        # callerEnded). Both are the cancel; neither is a turn that ran on,
        # which is what the scripted second step would have said.
        dones = [e for e in events if e.get("type") == "done"]
        errors = [e for e in events if e.get("type") == "error"]
        reported = [e.get("stopReason", "") for e in dones] + [e.get("message", "") for e in errors]
        assert any("cancelled" in r for r in reported), events
        assert joined(events, "text") != "never reached", events

        # The command really was running when the signal arrived, and the row
        # it left behind is closed.
        statuses = [e["status"] for e in events if e.get("type") == "tool"]
        assert "in_progress" in statuses, statuses
        assert statuses[-1] in TERMINAL, statuses

        # Both processes are gone -- asserted before anything below can kill
        # them, so a craze that left one running fails here instead of being
        # tidied up into a pass.
        gone(leader, CANCEL_LEADER_MARKER)
        gone(child, CANCEL_CHILD_MARKER)
    finally:
        # Last resort, reached only when something above failed or never got
        # as far as the checks: whatever it has to kill is recorded, so the
        # cleanup can never quietly turn a leak into a green run.
        leaked = reap_cancelled(proc, leader, child)
    assert not leaked, leaked


def reap_cancelled(proc: subprocess.Popen, leader: int, child: int) -> list[str]:
    """Kill anything the cancelled turn left behind, and say what that was.

    The group is killed through the shell, which the bash tool makes its own
    group's leader, so a grandchild this test never recorded goes too; the
    kill is guarded by the marker check, so a pid the OS has reused is never
    signalled. An empty list means there was nothing to do.
    """
    found = []
    if proc.poll() is None:
        found.append("craze had to be killed: it did not exit after the signal")
        proc.kill()
        proc.communicate(timeout=10)

    survivors = [
        (pid, marker, what)
        for pid, marker, what in (
            (leader, CANCEL_LEADER_MARKER, "shell"),
            (child, CANCEL_CHILD_MARKER, "child"),
        )
        if pid and alive(pid, marker)
    ]
    for pid, marker, what in survivors:
        found.append(f"the command's {what} (pid {pid}, {marker!r}) survived the cancel")
    if not survivors:
        return found

    # The group first, through the shell: it is its own group's leader, so
    # this also takes a grandchild the test never recorded. Only when the
    # marker says that pid is still the shell -- killpg on a reused pid, or
    # on the 0 that means "never observed", would signal the wrong group.
    if leader and alive(leader, CANCEL_LEADER_MARKER):
        _kill(os.killpg, leader)
    for pid, _, _ in survivors:
        _kill(os.kill, pid)
    return found


def _kill(how, pid: int) -> None:
    """SIGKILL pid (or its group), ignoring one that died in between."""
    try:
        how(pid, signal.SIGKILL)
    except OSError:  # ProcessLookupError and PermissionError are both OSError
        pass


def wait_for_pid(pid_file: Path, proc: subprocess.Popen, timeout: float = 60) -> int:
    """The pid the scripted command wrote, once the whole line is there.

    The trailing newline is what says the write finished: without it a read
    can catch a half-written number and turn into a flake on a loaded runner.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            raw = pid_file.read_text(encoding="utf-8")
        except OSError:
            raw = ""
        if raw.endswith("\n") and raw.strip().isdigit():
            return int(raw.strip())
        if proc.poll() is not None:
            raise AssertionError(f"craze exited {proc.returncode} before the command ran")
        time.sleep(0.05)
    raise AssertionError(f"the command never wrote a pid to {pid_file}")


def alive(pid: int, marker: str) -> bool:
    """Whether pid is a running process whose argv holds marker.

    Ported from internal/harness/tool/opencode/bash_test.go's alive: a zombie
    reads as dead (it has been killed and is only waiting to be reaped), and
    the marker means a pid the OS has reused is not mistaken for the command.
    """
    # -ww: ps truncates argv to the screen width by default, and the marker
    # is at the end of a long command line, so without it a running process
    # reads as dead on a narrow terminal — which is how CI first failed.
    out = subprocess.run(
        ["ps", "-ww", "-o", "stat=", "-o", "args=", "-p", str(pid)],
        capture_output=True,
        text=True,
        check=False,
    )
    fields = out.stdout.split()
    if out.returncode != 0 or not fields:
        return False
    return not fields[0].startswith("Z") and marker in " ".join(fields[1:])


def until_alive(pid: int, marker: str, timeout: float = 10) -> bool:
    """Whether pid comes to run as marker within timeout: a process just
    forked is awaited as the command it is about to become.

    A read that missed is kept: what the kernel showed for pid then is a
    warning when pid came alive after all (the flake's mechanism, on a green
    run) and part of the failure when it never did (plan 036 F3). Collecting
    it is bounded (proc_evidence) and never decides the answer: pid is read
    once more after the last of it, so a child that came alive while it was
    collected -- past the deadline, on a slow machine -- is found alive
    (plan 037 C3r)."""
    deadline = time.monotonic() + timeout
    start, first_miss, misses = time.monotonic(), "", 0
    until_alive.evidence = ""
    while not alive(pid, marker):
        misses += 1
        if not first_miss:
            first_miss = proc_evidence(pid)
        if time.monotonic() >= deadline:
            last = proc_evidence(pid)
            if alive(pid, marker):
                break
            until_alive.evidence = f"first miss: {first_miss}; last: {last}"
            return False
        time.sleep(0.05)
    if misses:
        warnings.warn(
            f"pid {pid} read as not running {misses}x before it ran as {marker!r} "
            f"({time.monotonic() - start:.3f}s); first miss: {first_miss}"
        )
    return True


until_alive.evidence = ""


# EVIDENCE_TIMEOUT bounds proc_evidence's ps, in seconds.
EVIDENCE_TIMEOUT = 2


def proc_evidence(pid: int) -> str:
    """What the kernel shows for pid, for a flake's record: ps's own answer,
    and on Linux the executable, raw command line and stat. An empty command
    line beside the new executable is the instant of an exec; no /proc entry
    is a process gone and reaped; a Z state is a zombie. A ps that does not
    answer within EVIDENCE_TIMEOUT, or cannot be run, is recorded as such:
    the record never fails the test, and its ps holds it up no longer than
    that."""
    try:
        ps = subprocess.run(
            ["ps", "-ww", "-o", "pid=,ppid=,pgid=,stat=,etime=,args=", "-p", str(pid)],
            capture_output=True, text=True, check=False, timeout=EVIDENCE_TIMEOUT,
        )
        out = [f"ps rc={ps.returncode} {ps.stdout.strip()!r}"]
    except subprocess.TimeoutExpired:
        out = [f"ps=<no answer in {EVIDENCE_TIMEOUT}s>"]
    except OSError as e:
        out = [f"ps=<{e}>"]
    base = Path(f"/proc/{pid}")
    if Path("/proc/self").exists():
        try:
            out.append(f"exe={os.readlink(base / 'exe')}")
        except OSError as e:
            out.append(f"exe=<{e.strerror}>")
        for name in ("cmdline", "stat"):
            try:
                out.append(f"{name}={(base / name).read_bytes()[:200]!r}")
            except OSError as e:
                out.append(f"{name}=<{e.strerror}>")
    return " ".join(out)


def gone(pid: int, marker: str, timeout: float = 10) -> None:
    """Fail unless pid, which ran as marker, dies soon: a killed process takes
    a moment to die and longer still to be reaped."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not alive(pid, marker):
            return
        time.sleep(0.05)
    raise AssertionError(f"pid {pid} ({marker}) is still running after the cancel")


# --- Plan 026 C5: sub-agents, end to end -----------------------------------
#
# The parent calls the agent tool and the harness runs a real child session
# against the same fixture. Children run at once, so each child's requests are
# answered from a route of its own (SSEFixture.route, matched on the request
# body: the child's first user message is its task), and the parent's from the
# script. What is checked is what leaves craze: the `--json` lines -- subagent
# lines and the child's own, tagged with its id -- and the requests the children
# sent.

# The tools a child is offered: the profile's, less agent, todo_write,
# ask_user_question and exit_plan_mode (plan 026 §3.2), in the profile's order.
CHILD_TOOLS = ["bash", "read", "glob", "grep", "edit", "write"]


def agent_call(call_id: str, description: str, prompt: str, **more: str) -> ToolCall:
    """One agent call in a parent's step."""
    return ToolCall(call_id, "agent", json.dumps({"description": description, "prompt": prompt, **more}))


def subagent_lines(events: list[dict]) -> dict[str, list[dict]]:
    """The subagent lines by child id, each child's in order."""
    out: dict[str, list[dict]] = {}
    for e in events:
        if e.get("type") == "subagent":
            out.setdefault(e["id"], []).append(e)
    return out


def child_of(lines: dict[str, list[dict]], description: str) -> tuple[str, list[dict]]:
    """The id and the subagent lines of the child whose description it is."""
    for cid, own in lines.items():
        if own[0].get("description") == description:
            return cid, own
    raise AssertionError(f"no child described {description!r} among {lines}")


def requests_of(server: SSEFixture, prompt: str) -> list[RecordedRequest]:
    """The requests of the turn whose prompt is prompt: a child's are its task's."""
    return [r for r in server.requests if user_prompt(r) == prompt]


def tool_results(request: RecordedRequest) -> dict[str, str]:
    """A request's tool results by call id."""
    return {m["tool_call_id"]: m["content"] for m in request.messages if m.get("role") == "tool"}


@pytest.mark.parametrize("routed", [True, False], ids=["route", "script"])
def test_sse_fixture_answers_in_recorded_order(fixture_server: SSEFixture, routed: bool) -> None:
    """The fixture's record of its requests and its answers to them agree on
    the order (review r8, finding 5), for a route's queue and the script alike.

    Two requests race: the first is held inside the route's predicate -- a
    test's own code, which the fixture runs outside its lock -- until the
    second has been answered. Whichever is recorded first must be the one given
    the first step. Recorded before its predicate ran and given its step after,
    in a second critical section, the held request was recorded first and
    answered with the second step. A predicate that owns neither request sends
    both to the script, which the same split broke the same way.
    """
    first_in = threading.Event()
    second_answered = threading.Event()

    def holds_the_first(request: RecordedRequest) -> bool:
        if request.body.get("tag") == "first":
            first_in.set()
            second_answered.wait(timeout=10)
        return routed

    steps = [answer("step 0"), answer("step 1")]
    fixture_server.route(holds_the_first, steps if routed else [])
    if not routed:
        fixture_server.set_script(steps)

    replies: dict[str, str] = {}

    def post(tag: str) -> None:
        body = json.dumps({"tag": tag, "messages": [{"role": "user", "content": "race"}]}).encode("utf-8")
        request = urllib.request.Request(
            fixture_server.base_url + "/chat/completions",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=10) as response:
            replies[tag] = response.read().decode("utf-8")

    first = threading.Thread(target=post, args=("first",))
    first.start()
    assert first_in.wait(timeout=10), "the first request never reached its predicate"
    post("second")  # answered while the first is held in its predicate
    second_answered.set()
    first.join(timeout=10)
    assert not first.is_alive(), "the first request was never answered"

    def step_of(reply: str) -> str:
        return next(s for s in ("step 0", "step 1") if s in reply)

    recorded = [r.body["tag"] for r in fixture_server.requests]
    assert sorted(recorded) == ["first", "second"], recorded
    assert [step_of(replies[tag]) for tag in recorded] == ["step 0", "step 1"], (recorded, replies)
    assert fixture_server.unscripted == 0


def test_native_agent_fans_out_to_two_children(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """Two agent calls in one step start two children that run at once; each
    child's task, tools and answer are its own; the parent reads both answers
    (plan 026 A6, A11).

    The `--json` stream prints, per child, a subagent line when it spawns,
    progress lines, and a finished line last carrying the row whole -- its id,
    the parent's call, its type, its model, its answer -- and the child's own
    lines tagged with its id: its task as a user line, its tool rows, its text.
    The parent's two agent calls are task-kind tool lines whose task names
    their child once they have finished.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    one, two = "Read notes.txt and say what it holds.", "Say hello."
    fixture_server.set_script(
        [
            parallel_step(agent_call("a1", "read the notes", one), agent_call("a2", "say hello", two)),
            answer("both children answered"),
        ]
    )
    fixture_server.route(prompt_is(one), [call_step("read", {"filePath": "notes.txt"}), answer("the notes say alpha")])
    fixture_server.route(prompt_is(two), [answer("hello")])

    proc = run_native(craze_bin, craze_home, ws, "fan out", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert joined([e for e in events if "agent" not in e], "text") == "both children answered"
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}, events[-3:]
    assert CANARY not in proc.stdout and CANARY not in proc.stderr
    assert fixture_server.unscripted == 0 and fixture_server.script_remaining == 0

    lines = subagent_lines(events)
    assert len(lines) == 2, lines
    answers = {}
    for description, task, call, answer_text in (
        ("read the notes", one, "t1.1.1", "the notes say alpha"),
        ("say hello", two, "t1.1.2", "hello"),
    ):
        cid, own = child_of(lines, description)
        assert own[0]["event"] == "spawned" and own[-1]["event"] == "finished", own
        assert all(e["event"] == "progress" for e in own[1:-1]), own
        fin = own[-1]
        assert fin["status"] == "completed" and fin["output"] == answer_text, fin
        assert fin["toolCallId"] == call and fin["subagentType"] == "general-purpose", fin
        assert fin["model"] == "fixture-model" and fin["transcript"] is True, fin

        mine = [e for e in events if e.get("agent") == cid]
        assert mine[0] == {"type": "user", "seq": mine[0]["seq"], "text": task, "agent": cid}, mine[0]
        assert joined(mine, "text") == answer_text, mine
        answers[call] = answer_text

        # The child's lines all fall between its spawn and its finish, and the
        # parent's line for the call that ran it closes after them all.
        seqs = [e["seq"] for e in mine]
        assert own[0]["seq"] < min(seqs) and max(seqs) < fin["seq"], (own, seqs)
        rows = [e for e in events if e.get("type") == "tool" and "agent" not in e and e.get("id") == call]
        assert rows[-1]["seq"] > fin["seq"], (rows[-1], fin)
        last = rows[-1]
        assert last["kind"] == "task" and last["status"] == "completed", last
        assert last["task"]["agentId"] == cid and last["task"]["model"] == "fixture-model", last

    child_rows = [e for e in events if e.get("type") == "tool" and e.get("agent")]
    assert {e["name"] for e in child_rows} == {"read"}, child_rows
    assert all(e["agent"] == child_of(lines, "read the notes")[0] for e in child_rows), child_rows

    # The wire: two parent requests, the reading child's two and the other's
    # one; the parent's second carries each child's answer as its call's result.
    assert len(requests_of(fixture_server, "fan out")) == 2
    assert len(requests_of(fixture_server, one)) == 2 and len(requests_of(fixture_server, two)) == 1
    results = tool_results(requests_of(fixture_server, "fan out")[-1])
    assert results == {"a1": answers["t1.1.1"], "a2": answers["t1.1.2"]}, results


def test_native_agent_child_failure(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A child whose provider fails is a failed sub-agent, not a failed turn
    (plan 026 §3.7): its finished line says failed and why, the parent's call
    for it fails with the runner's text, and the parent's model reads that and
    answers. The route with no steps is the provider failing: the fixture
    answers the child's request with its 400.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    task = "Look at the notes."
    fixture_server.set_script(
        [call_step("agent", {"description": "look", "prompt": task}, call_id="a1"), answer("the child failed")]
    )
    fixture_server.route(prompt_is(task), [])

    proc = run_native(craze_bin, craze_home, ws, "delegate", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}, events[-3:]
    assert CANARY not in proc.stdout and CANARY not in proc.stderr
    assert fixture_server.unscripted == 1

    ((cid, own),) = subagent_lines(events).items()
    fin = own[-1]
    assert fin["event"] == "finished" and fin["status"] == "failed", fin
    assert "no scripted response" in fin["error"], fin
    call = [e for e in events if e.get("type") == "tool" and "agent" not in e and e.get("id") == "t1.1.1"][-1]
    assert call["status"] == "failed" and call["task"]["status"] == "failed" and call["task"]["agentId"] == cid, call
    result = tool_results(requests_of(fixture_server, "delegate")[-1])["a1"]
    assert result.startswith("The sub-agent failed:"), result


def test_native_agent_child_tools_on_the_wire(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A child's request offers the profile's tools less the four a child never
    gets -- agent, todo_write, ask_user_question and exit_plan_mode -- while the
    parent's offers all of them (plan 026 §3.2, A1), and its system prompt ends
    in the sub-agent role section, which the parent's has not.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    task = "Answer at once."
    fixture_server.set_script(
        [call_step("agent", {"description": "at once", "prompt": task}, call_id="a1"), answer("done")]
    )
    fixture_server.route(prompt_is(task), [answer("answered")])

    proc = run_native(craze_bin, craze_home, ws, "go", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    (child,) = requests_of(fixture_server, task)
    parent = requests_of(fixture_server, "go")[0]
    assert child.tool_names == CHILD_TOOLS, child.tool_names
    assert "agent" in parent.tool_names and "todo_write" in parent.tool_names, parent.tool_names
    assert "# Your role as a sub-agent" in system_text(child)
    assert "# Your role as a sub-agent" not in system_text(parent)


def test_native_agent_workspace_persona(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A persona the workspace defines in .claude/agents/x.md is an agent type
    the parent can call by name (plan 026 §3.4, A10): the child runs with the
    persona's body as its role and only the tools it names, mapped to native
    ids, and its subagent lines carry the type.
    """
    ws = tool_workspace(tmp_path)
    (ws / ".claude" / "agents").mkdir(parents=True)
    (ws / ".claude" / "agents" / "x.md").write_text(
        "---\nname: x\ndescription: reads the notes and nothing else\ntools: Read\n---\n"
        "You are X, who only ever reads the notes.\n",
        encoding="utf-8",
    )
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    task = "Read the notes as X."
    fixture_server.set_script(
        [call_step("agent", {"description": "as x", "prompt": task, "subagent_type": "x"}, call_id="a1"), answer("done")]
    )
    fixture_server.route(prompt_is(task), [answer("X read them")])

    proc = run_native(craze_bin, craze_home, ws, "go", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    (child,) = requests_of(fixture_server, task)
    assert child.tool_names == ["read"], child.tool_names
    assert "You are X, who only ever reads the notes." in system_text(child)
    ((_, own),) = subagent_lines(parse_events(proc.stdout)).items()
    assert {e["subagentType"] for e in own} == {"x"}, own
    assert own[-1]["status"] == "completed" and own[-1]["output"] == "X read them", own[-1]


@pytest.mark.parametrize(
    ("mode", "call", "denial"),
    [
        (
            "--plan",
            call_step("write", {"filePath": "notes.txt", "content": "beta\n"}),
            "Rejected: file edits are not allowed — the agent that started you is in plan mode.",
        ),
        (
            "--ask",
            call_step("bash", {"command": "echo hi > notes.txt"}),
            "Rejected: ask mode is read-only - no edits, writes, or shell commands.",
        ),
    ],
    ids=["plan", "ask"],
)
def test_native_agent_plan_and_ask_children(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture, mode: str, call: Step, denial: str
) -> None:
    """A child inherits its parent's mode (plan 026 §3.5, A3): under --plan a
    child's write is denied with the child's own text -- it has no plan file to
    write to -- and under --ask its command is denied as read-only; the child
    reads the denial, answers, and the workspace is untouched.
    """
    ws = tool_workspace(tmp_path)
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    task = "Change the notes."
    fixture_server.set_script(
        [call_step("agent", {"description": "change", "prompt": task}, call_id="a1"), answer("the child could not")]
    )
    fixture_server.route(prompt_is(task), [call, answer("I was not allowed")])

    before = tree(ws)
    proc = run_native(craze_bin, craze_home, ws, mode, "delegate", timeout=60)
    assert proc.returncode == 0, proc.stdout + proc.stderr
    events = parse_events(proc.stdout)
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}, events[-3:]
    first, second = requests_of(fixture_server, task)
    assert tool_results(second) == {call.calls[0].id: denial}, tool_results(second)
    ((cid, own),) = subagent_lines(events).items()
    assert own[-1]["status"] == "completed" and own[-1]["output"] == "I was not allowed", own[-1]
    rows = [e for e in events if e.get("type") == "tool" and e.get("agent") == cid]
    assert rows and rows[-1]["status"] == "failed", rows
    assert tree(ws) == before, f"{mode} let a child change the workspace"
    assert first.tool_names == CHILD_TOOLS, first.tool_names


# --- Plan 028 C5: native resume, end to end --------------------------------
#
# A native session is resumable from plan 028 §3.5 on: its first prompt writes
# a sessions.jsonl row, and its transcript is the one store the harness
# reopens on `-c`/`-r`. These drive the real TUI over a pty (test_tui.PTYCraze,
# the class every cursor/grok resume test in test_tui.py already uses), the
# loopback SSE fixture answering it exactly as the headless tests above do.


def _seed_native_row(craze_home: Path, workspace: Path, session_id: str, title: str) -> Path:
    """One sessions.jsonl row directly under craze_home, no ".craze" folder:
    what `-c`/`-r` resolve against whenever CRAZE_HOME names the directory
    itself, as every other test in this file sets it up (write_native_config's
    sibling). test_tui._seed_index writes the other layout craze falls back to
    when CRAZE_HOME is unset (HOME/.craze), which does not apply here.
    """
    craze_home.mkdir(parents=True, exist_ok=True)
    stamp = (
        datetime.now(timezone.utc).replace(tzinfo=None).isoformat(timespec="microseconds") + "Z"
    )
    row = {
        "sessionId": session_id,
        "provider": "native",
        "cwd": str(workspace),
        "title": title,
        "pinned": False,
        "createdAt": stamp,
        "updatedAt": stamp,
    }
    index = craze_home / "sessions.jsonl"
    index.write_text(json.dumps(row) + "\n", encoding="utf-8")
    return index


def _transcripts_of(craze_home: Path, session_id: str) -> list[Path]:
    """Every native transcript file filed under session_id (§3.2's naming)."""
    return list((craze_home / "native" / "sessions").rglob(f"*_{session_id}.jsonl"))


def _entries_of(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text(encoding="utf-8").splitlines()]


def _journal_lines(craze_home: Path) -> list[dict]:
    """Every journal line under craze_home, file by file. A last line that does
    not decode is skipped, as the journal's own reader skips it: a craze whose
    bounded close gave up on a stalled writer can leave its file torn there."""
    out: list[dict] = []
    for jf in sorted((craze_home / "journal").glob("*/*.jsonl")):
        raw = jf.read_text(encoding="utf-8").splitlines()
        for i, line in enumerate(raw):
            try:
                out.append(json.loads(line))
            except json.JSONDecodeError:
                if i != len(raw) - 1:
                    raise
    return out


def _resume_empties(craze_home: Path) -> list[dict]:
    return [
        ln
        for ln in _journal_lines(craze_home)
        if ln.get("type") == "diag" and ln.get("kind") == "resume_empty"
    ]


def _wait_resume_empty(craze_home: Path, timeout: float = 10) -> None:
    """Wait, while craze still runs, for the resume_empty diag to reach the
    journal file. The writer puts buffered lines on disk every 250 ms
    (journal.defaultFlushInterval); a quit only waits 500 ms for it
    (defaultCloseWait), so a -c run as short as this one, quit under load,
    can exit before its first line is written. Reading the journal only after
    the quit would test that race, not the diag."""
    deadline = time.monotonic() + timeout
    while not _resume_empties(craze_home):
        if time.monotonic() > deadline:
            raise AssertionError(f"no resume_empty diag reached the journal: {_journal_lines(craze_home)}")
        time.sleep(0.05)


def _listing(root: Path) -> list[tuple[str, int]]:
    """Every file under root with its size: failure context for the index."""
    return sorted((str(p.relative_to(root)), p.stat().st_size) for p in root.rglob("*") if p.is_file())


def test_native_resume_round_trip(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A12(1): a native PTY session survives `-c`, transcript and all.

    Turn one calls bash and answers in words; quitting and `-c` in the same
    workspace restores the whole thing on screen -- the user row, the tool
    card with its output, the model's text, and the `restored` note that
    closes a replay (transcript.NoteRestored) -- and a second prompt continues
    the session: the wire carries the whole first turn back to the fixture
    (plan 028 §3.4), and the transcript is still the one file, now carrying
    the `resume` entry §3.2 records for each later incarnation.
    """
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    workspace = tmp_path / "ws"
    workspace.mkdir()

    fixture_server.set_script(
        [call_step("bash", {"command": "echo hi"}), answer("done checking")]
    )
    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="native",
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        tui.wait_contains("native")
        tui.write(b"check it\r")
        tui.wait_contains("✓ bash  echo hi", timeout=30)
        tui.wait_contains("  hi", timeout=10)
        tui.wait_contains("done checking", timeout=30)
        quit_craze(tui)

    index = craze_home / "sessions.jsonl"
    rows = [json.loads(line) for line in index.read_text(encoding="utf-8").splitlines()]
    assert len(rows) == 1, rows
    row = rows[0]
    assert row["provider"] == "native", row
    assert row["title"] == "check it", row
    session_id = row["sessionId"]

    before = _transcripts_of(craze_home, session_id)
    assert len(before) == 1, before

    # `-c`: no --provider, no --agent-bin -- the row's own provider loads it
    # (resolveLoad, plan 028 §3.5).
    fixture_server.set_script([answer("second turn done")])
    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="",
        extra_args=["-c"],
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        tui.wait_contains("restored", timeout=30)
        restored = _ANSI.sub("", tui.screen())
        # The gutter mark proves the replayed USER ROW was drawn, not just the
        # composer rule's title (which also reads "check it" -- the title is
        # the first prompt's text, plan 028 §3.4): "❯ " is how craze draws a
        # user row (TestFrameGoldenNativeResume100x30 in internal/tui).
        assert "❯ check it" in restored, restored[-3000:]
        assert "✓ bash  echo hi" in restored, restored[-3000:]
        assert "  hi" in restored, restored[-3000:]
        assert "done checking" in restored, restored[-3000:]

        tui.write(b"keep going now\r")
        tui.wait_contains("second turn done", timeout=30)
        quit_craze(tui)

    # The wire: the continued turn's request carries the whole first turn.
    last = fixture_server.requests[-1]
    users = user_contents(last)
    assert "check it" in users, users
    assert "keep going now" in users, users
    tool_calls = [m for m in last.messages if m.get("role") == "assistant" and m.get("tool_calls")]
    assert tool_calls, last.messages
    tool_results = {m["tool_call_id"]: m["content"] for m in last.messages if m.get("role") == "tool"}
    assert any("hi" in v for v in tool_results.values()), tool_results
    assert any(
        m.get("role") == "assistant" and "done checking" in (m.get("content") or "")
        for m in last.messages
    ), last.messages

    # One file, one header, one resume entry: reopened, never duplicated.
    after = _transcripts_of(craze_home, session_id)
    assert len(after) == 1, after
    entries = _entries_of(after[0])
    assert sum(1 for e in entries if e.get("type") == "session") == 1, entries
    assert sum(1 for e in entries if e.get("type") == "resume") == 1, entries


def test_native_resume_picker_loads_the_row(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A12(2): `-r` offers the native row (resumeRows keeps it: native is
    resumable even while it is still hidden, plan 028 §3.5) and Enter restores
    it, the same as it does for cursor and grok.
    """
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    workspace = tmp_path / "ws"
    workspace.mkdir()

    fixture_server.set_ok(text_parts=["picker fixture reply"])
    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="native",
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        tui.wait_contains("native")
        tui.write(b"pick me\r")
        tui.wait_contains("picker fixture reply", timeout=30)
        quit_craze(tui)

    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="",
        extra_args=["-r"],
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        tui.wait_contains("resume")
        tui.wait_contains("pick me")
        tui.wait_contains("native")
        tui.write(b"\r")
        tui.wait_contains("restored", timeout=30)
        quit_craze(tui)


def test_native_resume_of_an_empty_session(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A12(3): "prompt, Esc, quit, `-c`" -- P35/PD8/X25 end to end.

    The turn is cancelled before the fixture ever answers, so the index row
    exists (a native session's first prompt writes it, §3.5) but no transcript
    file does (only its first output writes that, §3.2). `-c` then opens a NEW
    session under the SAME id -- the deliberate exception to "a load never
    falls back to session/new" (D-60) -- journaling `resume_empty`, and a
    following prompt creates the transcript there.
    """
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    workspace = tmp_path / "ws"
    workspace.mkdir()

    got_request = threading.Event()
    release = threading.Event()

    def stall(_request: RecordedRequest) -> Step:
        # Scripted as a callable so it runs outside the fixture's lock (the
        # race test above this section relies on the same property): it can
        # block here for as long as it likes without wedging other requests.
        got_request.set()
        release.wait(timeout=30)
        return answer("never reached")

    fixture_server.set_script([stall])

    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="native",
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        tui.wait_contains("native")
        tui.write(b"go\r")
        assert got_request.wait(timeout=10), "the fixture never saw the request"
        tui.write(b"\x1b")  # Esc: cancel the turn (help_dialog.go's own binding)
        tui.wait_contains("cancelled", timeout=10)
        quit_craze(tui)
    release.set()

    index = craze_home / "sessions.jsonl"
    rows = [json.loads(line) for line in index.read_text(encoding="utf-8").splitlines()]
    assert len(rows) == 1, (rows, _listing(craze_home))
    session_id = rows[0]["sessionId"]
    assert rows[0]["provider"] == "native", rows[0]

    assert _transcripts_of(craze_home, session_id) == []

    fixture_server.set_script([answer("fresh after empty resume")])
    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="",
        extra_args=["-c"],
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        # The load's replay bracket must close before a prompt is accepted
        # (X19/X27: `prompt()` refuses "session not started" while it holds),
        # which "restored" -- drawn once it does -- is the synchronisation for.
        tui.wait_contains("restored", timeout=30)
        tui.write(b"try again\r")
        tui.wait_contains("fresh after empty resume", timeout=30)
        _wait_resume_empty(craze_home)
        quit_craze(tui)

    rows2 = [json.loads(line) for line in index.read_text(encoding="utf-8").splitlines()]
    assert len(rows2) == 1, (rows2, _listing(craze_home))
    assert rows2[0]["sessionId"] == session_id, rows2

    after = _transcripts_of(craze_home, session_id)
    assert len(after) == 1, after

    resume_empties = _resume_empties(craze_home)
    assert len(resume_empties) == 1, _journal_lines(craze_home)
    assert resume_empties[0]["fields"]["session"] == session_id, resume_empties[0]


def test_native_resume_refuses_agent_bin(craze_bin: Path, tmp_path: Path) -> None:
    """A12(4): `--agent-bin`/`CRAZE_AGENT_BIN` with a native row exits 2 with
    the in-process message, before any claim or index write (plan 028 §3.5's
    refuseInProcess, run before S2's EnsureCrazeID + claim) -- so the index
    file is untouched and no session lock is ever created.
    """
    workspace = tmp_path / "ws"
    workspace.mkdir()
    craze_home = tmp_path / "craze-home"
    index = _seed_native_row(craze_home, workspace, "sess-native-refuse", "hi")
    before = index.read_bytes()
    locks_dir = workspace / ".cache" / "craze" / "locks"

    with PTYCraze(
        craze_bin,
        Path("/bin/true"),
        workspace,
        provider="",
        extra_args=["-c"],
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        code = tui.wait_exit(timeout=10)
        text = _ANSI.sub("", tui.screen())
        assert code == 2, text[-2000:]
        assert "--agent-bin" in text and "native" in text, text

    assert index.read_bytes() == before
    assert not locks_dir.exists() or list(locks_dir.iterdir()) == []

    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="",
        extra_args=["-c"],
        env_extra={"CRAZE_HOME": str(craze_home), "CRAZE_AGENT_BIN": "/bin/true"},
    ) as tui:
        code = tui.wait_exit(timeout=10)
        text = _ANSI.sub("", tui.screen())
        assert code == 2, text[-2000:]
        assert "CRAZE_AGENT_BIN" in text and "native" in text, text

    assert index.read_bytes() == before
    assert not locks_dir.exists() or list(locks_dir.iterdir()) == []


# ---------------------------------------------------------------------------
# Plan 028 PR 2 (compaction, C14, A29): the harness compacting on its own
# mid-turn, a resumed session showing the summary and its kept tail, and
# /compact's own compaction lines over --json.


def _compaction_entries(entries: list[dict]) -> list[dict]:
    return [e for e in entries if e.get("type") == "compaction"]


def _run_mid_turn_compaction(
    craze_bin: Path, craze_home: Path, workspace: Path, fixture_server: SSEFixture
) -> str:
    """Runs the scripted turn that crosses a small model's compaction
    threshold mid-turn (plan 028 §3.6, §3.7, §3.9, §3.11), quits, and returns
    the session id. Both compaction tests below build their own session from
    this: one asserts what it left on disk, the other resumes it.

    The window is 4000 tokens (threshold 3400; the tail budget is a quarter of
    that, 850, since 850 is under the default tail_tokens 20000, plan 028
    §3.9). Reading big.txt (step one) reports the fixture's fixed, tiny
    default usage and stays far under the threshold, so the turn goes on;
    reading small.txt (step two) is scripted with an inflated reported
    prompt_tokens -- a real provider's own count, which is what the harness's
    threshold check actually reads (plan 028 §3.7) -- that alone crosses it.
    big.txt (~6 KB) is far bigger than the tail budget in bytes, so the cut
    drops it into the segment file and keeps small.txt's own tiny step as the
    tail: a resumed session then shows a real kept tail, not just a bare
    summary.
    """
    write_native_config(craze_home / "native", fixture_server.base_url, context_window=4000)
    workspace.mkdir(parents=True, exist_ok=True)
    (workspace / "big.txt").write_text(("B" * 80 + "\n") * 75, encoding="utf-8")
    (workspace / "small.txt").write_text("ok\n", encoding="utf-8")

    summary_text = "<summary>" + ("Work summary detail. " * 30) + "</summary>"
    fixture_server.set_script(
        [
            call_step("read", {"filePath": "big.txt"}),
            call_step("read", {"filePath": "small.txt"}, usage=(3500, 0)),
            answer(summary_text),
            answer("compaction done, continuing now"),
        ]
    )

    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="native",
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        tui.wait_contains("native")
        tui.write(b"read the two files\r")
        tui.wait_contains("✓ read  big.txt", timeout=30)
        tui.wait_contains("✓ read  small.txt", timeout=30)
        tui.wait_contains("context compacted", timeout=30)
        tui.wait_contains("compaction done, continuing now", timeout=30)
        quit_craze(tui)

    assert fixture_server.script_remaining == 0
    assert fixture_server.unscripted == 0

    index = craze_home / "sessions.jsonl"
    rows = [json.loads(line) for line in index.read_text(encoding="utf-8").splitlines()]
    assert len(rows) == 1, rows
    return rows[0]["sessionId"]


def test_native_mid_turn_compaction(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A29(1): the scripted turn above (_run_mid_turn_compaction) compacts
    mid-turn and still ends as ONE turn: the transcript holds its
    `compaction` entry (§3.2, §3.9) and the segment file it wrote (§3.10)
    holds the dropped step -- big.txt's read, never small.txt's, which the
    cut kept as the tail.
    """
    craze_home = tmp_path / "craze-home"
    workspace = tmp_path / "ws"
    session_id = _run_mid_turn_compaction(craze_bin, craze_home, workspace, fixture_server)

    transcripts = _transcripts_of(craze_home, session_id)
    assert len(transcripts) == 1, transcripts
    entries = _entries_of(transcripts[0])
    compactions = _compaction_entries(entries)
    assert len(compactions) == 1, entries
    c = compactions[0]
    assert c["reason"] == "auto", c
    assert "error" not in c, c
    assert c["summary"], c
    assert c["tokensBefore"] >= 3400, c
    assert 0 < c["tokensAfter"] < c["tokensBefore"], c
    segment_name = c.get("segment")
    assert segment_name, c

    path = str(transcripts[0])
    assert path.endswith(".jsonl"), path
    segment_dir = Path(path[: -len(".jsonl")] + ".compaction")
    segment_file = segment_dir / segment_name
    assert segment_file.is_file(), sorted(p.name for p in segment_dir.iterdir())
    content = segment_file.read_text(encoding="utf-8")
    assert "# craze session" in content, content[:2000]
    # The dropped step (big.txt) is in the segment; the kept tail (small.txt)
    # never duplicates into it.
    assert "big.txt" in content, content
    assert "small.txt" not in content, content

    # review r1-c14-c9e finding 2: ONE turn, precisely -- not just an entry
    # and a segment consistent with staying in one turn, but the transcript
    # itself showing exactly one turn opened and exactly one ended, and the
    # final scripted answer belonging to that same turn (turn=1). Only a user
    # entry ever carries "turn" (it is what opens one, store/entry.go), so a
    # second one would mean a second turn started; there is none.
    messages = [e for e in entries if e.get("type") == "message"]
    user_entries = [e for e in messages if e.get("message", {}).get("role") == "user"]
    assert len(user_entries) == 1, entries
    assert user_entries[0].get("turn") == 1, user_entries[0]
    # The mid-turn compaction ran inside that same turn.
    assert c["turn"] == 1, c
    # Exactly one turn ending for the whole prompt, and it is the one
    # carrying the final scripted answer -- a regression that ended turn 1 at
    # the compaction and ran the final answer as a turn of its own would
    # either show a second "turn"-opening user entry, or a second end_turn,
    # or the wrong one holding the final text.
    end_turns = [e for e in messages if e.get("stopReason") == "end_turn"]
    assert len(end_turns) == 1, entries
    assert "compaction done, continuing now" in json.dumps(end_turns[0]["message"]), end_turns[0]


def test_native_resume_of_a_compacted_session(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A29(2): the same session, quit and `craze -c`: the compaction note
    replays in place (plan 028 §3.13, "Replayed Compacted{ended} draws the
    note in place"), and the next prompt's request -- what the fixture
    actually received -- starts with the summary message (`<compacted_context>`)
    and then the kept tail (small.txt's call and result), never big.txt's,
    which the cut dropped.
    """
    craze_home = tmp_path / "craze-home"
    workspace = tmp_path / "ws"
    session_id = _run_mid_turn_compaction(craze_bin, craze_home, workspace, fixture_server)

    # review r1-c14-c9e finding 2: the STORED summary text, read straight from
    # the transcript's own compaction entry -- what the resumed request's
    # summary message must be checked against, not just the wrapper's fixed
    # wording.
    stored = _compaction_entries(_entries_of(_transcripts_of(craze_home, session_id)[0]))
    assert len(stored) == 1, stored
    stored_summary = stored[0]["summary"]
    assert stored_summary, stored[0]

    fixture_server.set_script([answer("second turn done")])
    with PTYCraze(
        craze_bin,
        None,
        workspace,
        provider="",
        extra_args=["-c"],
        env_extra={"CRAZE_HOME": str(craze_home)},
    ) as tui:
        tui.wait_contains("restored", timeout=30)
        restored = _ANSI.sub("", tui.screen())
        assert "read the two files" in restored, restored[-4000:]
        assert "✓ read  big.txt" in restored, restored[-4000:]
        assert "✓ read  small.txt" in restored, restored[-4000:]
        assert "context compacted" in restored, restored[-4000:]
        assert "compaction done, continuing now" in restored, restored[-4000:]

        # The replayed note is IN PLACE: after the rows of the steps it
        # followed and before the row that came after it, in the restored
        # screen's own order -- not merely present somewhere in the
        # accumulated output (review r1-c14-c9e finding 2).
        i_big = restored.index("✓ read  big.txt")
        i_small = restored.index("✓ read  small.txt")
        i_note = restored.index("context compacted")
        i_answer = restored.index("compaction done, continuing now")
        assert i_big < i_small < i_note < i_answer, restored[-4000:]

        tui.write(b"keep going now\r")
        tui.wait_contains("second turn done", timeout=30)
        quit_craze(tui)

    last = fixture_server.requests[-1]
    users = user_contents(last)
    assert users, last.messages
    assert users[0].startswith("<compacted_context>"), users[0][:500]
    assert "compacted to fit the model" in users[0], users[0][:500]
    assert stored_summary in users[0], (stored_summary, users[0][:2000])
    assert "keep going now" in users, users

    # The tail is small.txt's own step, verbatim; big.txt's is gone (it is in
    # the segment file, not the request).
    tool_calls = [m for m in last.messages if m.get("role") == "assistant" and m.get("tool_calls")]
    assert any("small.txt" in json.dumps(m["tool_calls"]) for m in tool_calls), tool_calls
    assert not any("big.txt" in json.dumps(m["tool_calls"]) for m in tool_calls), tool_calls
    tool_results = {m["tool_call_id"]: m["content"] for m in last.messages if m.get("role") == "tool"}
    assert any("ok" in v for v in tool_results.values()), tool_results


def test_native_compact_command_json_lines(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    """A29(3): `/compact` and `/compact <focus>` over `craze prompt --json`
    (plan 028 §3.12, §3.13) -- each a turn of its own, with no model turn
    after it: the `compaction` lines carry `seq`, no `agent` (the parent's),
    and `error` omitted on a successful summary but present when the
    summarizer's replies are all too short to accept (plan 028 §3.8 item 5).
    """
    craze_home = tmp_path / "craze-home"
    write_native_config(craze_home / "native", fixture_server.base_url)
    workspace = tmp_path / "ws"
    workspace.mkdir()

    summary_text = "<summary>" + ("Everything so far, in detail. " * 20) + "</summary>"
    fixture_server.set_script(
        [
            answer("hello"),
            answer(summary_text),
            answer("too short"),
            answer("still short"),
            answer("nope"),
        ]
    )

    proc = run_native(
        craze_bin,
        craze_home,
        workspace,
        "hi",
        "--follow-up",
        "/compact",
        "--follow-up",
        "/compact keep the summary",
        timeout=30,
    )
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert fixture_server.script_remaining == 0
    assert fixture_server.unscripted == 0
    assert len(fixture_server.requests) == 5, len(fixture_server.requests)

    events = parse_events(proc.stdout)
    compactions = [e for e in events if e.get("type") == "compaction"]
    assert len(compactions) == 4, events
    for c in compactions:
        assert "seq" in c, c
        assert "agent" not in c, c

    started1, ended1, started2, ended2 = compactions
    assert started1["phase"] == "started" and started1["reason"] == "manual", started1
    assert ended1["phase"] == "ended" and ended1["reason"] == "manual", ended1
    assert "error" not in ended1, ended1
    assert ended1["tokensAfter"] > 0, ended1

    assert started2["phase"] == "started" and started2["reason"] == "manual", started2
    assert ended2["phase"] == "ended" and ended2["reason"] == "manual", ended2
    assert "too short to be a real summary" in ended2["error"], ended2

    # No model turn follows either /compact: the only "text" event is the
    # first, ordinary turn's.
    assert joined(events, "text") == "hello"

    dones = [e for e in events if e.get("type") == "done"]
    assert len(dones) == 2, events
    assert all(d["stopReason"] == "end_turn" for d in dones), dones

    errors = [e for e in events if e.get("type") == "error"]
    assert len(errors) == 1, events
    assert "too short to be a real summary" in errors[0]["message"], errors[0]
