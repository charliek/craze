"""Plan 018 C11: `craze --provider native` from the outside.

The native provider is hidden (plan 018 §3.4): never listed, never persisted,
never indexed. These tests drive the real `craze` binary against a loopback
SSE fixture standing in for a real OpenAI-compatible provider (sse_fixture.py)
and check the CLI-visible contract: text streams and the turn ends end_turn,
`--help` and an unknown-provider error never say "native", a run never
touches config.toml's provider or creates sessions.jsonl, and a canary key
never reaches stdout or stderr -- including when the provider answers 401 or
500 with the key echoed back, the realistic leak path (plan 018 §3.5).
"""

from __future__ import annotations

import json
import os
import signal
import subprocess
import time
from pathlib import Path

import pytest

from conftest import require_rg, without_seq
from sse_fixture import (
    CANARY,
    UNUSED_ENV_KEY,
    SSEFixture,
    ToolCall,
    answer,
    call_step,
    parallel_step,
    sse_call_chunks,
    write_native_config,
)


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


def test_native_absent_from_help_and_unknown_provider_error(
    craze_bin: Path, tmp_path: Path
) -> None:
    help_proc = subprocess.run(
        [str(craze_bin), "--help"], capture_output=True, text=True, timeout=5
    )
    assert help_proc.returncode == 0, help_proc.stderr
    assert "native" not in help_proc.stdout.lower(), help_proc.stdout

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
    assert "native" not in unknown_proc.stderr.lower(), unknown_proc.stderr
    assert unknown_proc.stdout == ""


def test_native_run_leaves_config_and_sessions_alone(
    craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture
) -> None:
    fixture_server.set_ok(text_parts=["ok"])
    craze_home = tmp_path / "craze-home"
    craze_home.mkdir()
    config_path = craze_home / "config.toml"
    before = 'provider = "grok"\ntheme = "tokyo-night"\n'
    config_path.write_text(before, encoding="utf-8")
    write_native_config(craze_home / "native", fixture_server.base_url)

    proc = run_native(craze_bin, craze_home, tmp_path, "hi")
    assert proc.returncode == 0, proc.stderr

    assert config_path.read_text(encoding="utf-8") == before
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


@pytest.mark.parametrize("flag", ["--ask", "--plan"])
def test_native_ask_or_plan_is_a_usage_error(
    craze_bin: Path, tmp_path: Path, flag: str
) -> None:
    craze_home = tmp_path / "craze-home"
    proc = run_native(craze_bin, craze_home, tmp_path, flag, "hi", json_mode=False)
    assert proc.returncode == 2, proc.stderr
    assert "native" in proc.stderr, proc.stderr
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
    # profile's nine tools in its registry order (opencode.Profile), and the
    # last one carries every call's result back, each tied to its call id.
    first, last = fixture_server.requests[0], fixture_server.requests[-1]
    assert first.tool_names == [
        "bash",
        "read",
        "glob",
        "grep",
        "edit",
        "write",
        "todo_write",
        "ask_user_question",
        "exit_plan_mode",
    ], first.tool_names
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
        # test never could see.
        assert alive(leader, CANCEL_LEADER_MARKER), f"the shell ({leader}) was not running"
        assert alive(child, CANCEL_CHILD_MARKER), f"the child ({child}) was not running"

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


def gone(pid: int, marker: str, timeout: float = 10) -> None:
    """Fail unless pid, which ran as marker, dies soon: a killed process takes
    a moment to die and longer still to be reaped."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not alive(pid, marker):
            return
        time.sleep(0.05)
    raise AssertionError(f"pid {pid} ({marker}) is still running after the cancel")
