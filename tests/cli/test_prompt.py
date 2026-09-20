from __future__ import annotations

import json
import os
import signal
import subprocess
import time
from pathlib import Path

import pytest

from conftest import REMOVED_CONFIG_ENV, without_seq


def parse_events(stdout: str) -> list[dict]:
    events = []
    for line in stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        events.append(json.loads(line))
    return events


def run_prompt(
    craze_bin: Path,
    fake_agent_bin: Path,
    workspace: Path,
    *args: str,
    script: str = "echo",
    input_text: str | None = None,
    timeout: float = 10,
    json_mode: bool = True,
) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = script
    env.pop("CRAZE_PROVIDER", None)
    # Per workspace, and with no config.toml in it until a run saves one.
    env["CRAZE_HOME"] = str(workspace / "craze-home")
    env["XAI_API_KEY"] = ""
    env["GROK_CODE_XAI_API_KEY"] = ""
    cmd = [str(craze_bin), "prompt"]
    if json_mode:
        cmd.append("--json")
    cmd += [
        "--agent-bin",
        str(fake_agent_bin),
        "--workspace",
        str(workspace),
        *args,
    ]
    return subprocess.run(
        cmd,
        check=False,
        capture_output=True,
        text=True,
        input=input_text,
        env=env,
        timeout=timeout,
    )


def test_grok_echo(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin, fake_agent_bin, tmp_path, "--provider", "grok", "hello", script="grok-echo"
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "echo: hello"
    terminals = [e for e in events if e.get("type") in ("done", "error")]
    assert without_seq(events, terminals) == [{"type": "done", "stopReason": "end_turn"}]


def test_gx_echo(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    # gx is grok on the wire (plan 012), so the fake agent's grok-shaped
    # script applies unchanged: it keys off --script, never --provider.
    proc = run_prompt(
        craze_bin, fake_agent_bin, tmp_path, "--provider", "gx", "hello", script="grok-echo"
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "echo: hello"
    terminals = [e for e in events if e.get("type") in ("done", "error")]
    assert without_seq(events, terminals) == [{"type": "done", "stopReason": "end_turn"}]


def test_first_turn_echo(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(craze_bin, fake_agent_bin, tmp_path, "hello")
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "echo: hello"
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}
    assert all(e.get("type") != "error" for e in events)
    assert "NOPE" not in texts
    assert proc.stdout.endswith("\n")
    # json mode: stdout is only event lines
    for line in proc.stdout.splitlines():
        json.loads(line)


def test_follow_up(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "one",
        "--follow-up",
        "two",
        script="followup",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "first replysecond reply"
    dones = [e for e in events if e.get("type") == "done"]
    assert len(dones) == 2


def test_tool_row(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(craze_bin, fake_agent_bin, tmp_path, "run", script="tool")
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    tools = [e for e in events if e.get("type") == "tool"]
    assert tools, events
    assert tools[0]["name"] == "Shell"
    assert tools[0]["id"] == "call-1"
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert "after tool" in texts


def test_cancel_hang(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = "hang"
    proc = subprocess.Popen(
        [
            str(craze_bin),
            "prompt",
            "--json",
            "--agent-bin",
            str(fake_agent_bin),
            "--workspace",
            str(tmp_path),
            "wait",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=env,
    )
    time.sleep(0.4)
    proc.send_signal(signal.SIGINT)
    stdout, stderr = proc.communicate(timeout=10)
    assert proc.returncode != 0, stdout + stderr
    events = parse_events(stdout)
    types = [e.get("type") for e in events]
    assert "done" in types or "error" in types
    if "done" in types:
        assert any(e.get("stopReason") == "cancelled" for e in events)


def test_permission_allow_once(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "go",
        "--no-force",
        "--permission-decision",
        "allow-once",
        script="permission",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    perms = [e for e in events if e.get("type") == "permission"]
    assert perms, events
    assert "opt-once" in perms[0]["optionIds"]
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "decision:opt-once"


def test_permission_reject_once(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "go",
        "--no-force",
        "--permission-decision",
        "reject-once",
        script="permission",
    )
    assert proc.returncode == 1, proc.stderr
    events = parse_events(proc.stdout)
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "decision:opt-reject"


def test_auth_fail(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(craze_bin, fake_agent_bin, tmp_path, "hi", script="authfail")
    assert proc.returncode == 1, proc.stderr
    events = parse_events(proc.stdout)
    errors = [e for e in events if e.get("type") == "error"]
    assert errors, proc.stdout + proc.stderr
    assert "agent login" in errors[0]["message"]
    # stdout is still only JSON lines
    for line in proc.stdout.splitlines():
        json.loads(line)


def test_stdin_prompt(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        script="echo",
        input_text="from stdin\n",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "echo: from stdin"


def test_grok_ask_json(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--provider",
        "grok",
        "q",
        script="grok-ask",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    questions = [e for e in events if e.get("type") == "question"]
    assert questions, events
    assert questions[0].get("auto") is True
    assert questions[0]["answers"]["Pick one"] == ["A"]
    assert questions[0]["answers"]["Pick any"] == ["X"]
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "asked:accepted:Pick one=A;Pick any=X"
    assert without_seq(events, events[-1]) == {"type": "done", "stopReason": "end_turn"}


def test_unknown_provider_exits_2(craze_bin: Path, tmp_path: Path) -> None:
    proc = subprocess.run(
        [str(craze_bin), "prompt", "--provider", "codex", "--workspace", str(tmp_path), "hi"],
        check=False,
        capture_output=True,
        text=True,
        timeout=5,
    )
    assert proc.returncode == 2
    assert "unknown provider" in proc.stderr
    assert proc.stdout == ""


def test_usage_exit_2(craze_bin: Path, tmp_path: Path) -> None:
    proc = subprocess.run(
        [str(craze_bin), "prompt", "--workspace", str(tmp_path)],
        check=False,
        capture_output=True,
        text=True,
        input="",
        timeout=5,
    )
    assert proc.returncode == 2
    assert "prompt text required" in proc.stderr
    assert proc.stdout == ""


def test_removed_config_env_exits_2(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """The variable CRAZE_HOME replaced is a usage error that names CRAZE_HOME.

    The run would otherwise succeed and save its provider; it refuses before
    reading or writing anything, so neither HOME nor CRAZE_HOME gains a file.
    """
    env = os.environ.copy()
    env[REMOVED_CONFIG_ENV] = str(tmp_path / "old" / "config.toml")
    env["CRAZE_FAKE_SCRIPT"] = "echo"
    proc = subprocess.run(
        [
            str(craze_bin),
            "prompt",
            "--json",
            "--provider",
            "cursor",
            "--agent-bin",
            str(fake_agent_bin),
            "--workspace",
            str(tmp_path),
            "hi",
        ],
        check=False,
        capture_output=True,
        text=True,
        env=env,
        timeout=5,
    )
    assert proc.returncode == 2, proc.stderr
    assert REMOVED_CONFIG_ENV in proc.stderr and "CRAZE_HOME" in proc.stderr, proc.stderr
    assert proc.stdout == ""
    assert not Path(env["CRAZE_HOME"]).exists()
    assert list(Path(env["HOME"]).iterdir()) == []


def _lifecycle(events: list[dict], agent_id: str | None = None) -> list[str]:
    out = []
    for e in events:
        if e.get("type") != "subagent":
            continue
        if agent_id is not None and e.get("id") != agent_id:
            continue
        out.append(e.get("event"))
    return out


def test_grok_subagent_json(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin, fake_agent_bin, tmp_path, "--provider", "grok", "go", script="grok-subagent"
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    life = _lifecycle(events, "sub-1")
    assert "spawned" in life and "progress" in life and "finished" in life
    assert life.index("spawned") < life.index("progress") < life.index("finished")
    users = [e for e in events if e.get("type") == "user" and e.get("agent") == "sub-1"]
    assert users
    child_text = [e for e in events if e.get("type") == "text" and e.get("agent") == "sub-1"]
    assert child_text
    assert any(e.get("type") == "done" for e in events)
    assert any(e.get("type") == "subagent" and e.get("event") == "finished" for e in events)


def test_grok_subagent_late_json(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin, fake_agent_bin, tmp_path, "--provider", "grok", "go", script="grok-subagent-late"
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    assert any(e.get("type") == "done" for e in events)
    assert any(e.get("type") == "subagent" and e.get("event") == "finished" for e in events)


def test_grok_subagent_cancel_json(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = "grok-subagent-cancel"
    env.pop("CRAZE_PROVIDER", None)
    env["CRAZE_HOME"] = str(tmp_path / "craze-home")
    env["XAI_API_KEY"] = ""
    env["GROK_CODE_XAI_API_KEY"] = ""
    proc = subprocess.Popen(
        [
            str(craze_bin),
            "prompt",
            "--json",
            "--provider",
            "grok",
            "--agent-bin",
            str(fake_agent_bin),
            "--workspace",
            str(tmp_path),
            "go",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=env,
    )
    time.sleep(0.5)
    proc.send_signal(signal.SIGINT)
    stdout, stderr = proc.communicate(timeout=10)
    assert proc.returncode != 0, stdout + stderr
    events = parse_events(stdout)
    finished = [e for e in events if e.get("type") == "subagent" and e.get("event") == "finished"]
    assert finished, events
    assert finished[-1].get("status") == "cancelled"


def test_prompt_json_task_subagent(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(craze_bin, fake_agent_bin, tmp_path, "go", script="task")
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    life = _lifecycle(events)
    assert life.index("spawned") < life.index("progress") < life.index("finished"), life
    tools = [e for e in events if e.get("type") == "tool" and e.get("task")]
    assert tools
    assert tools[-1]["task"].get("status") == "completed"


def test_plain_prompt_excludes_child_text(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--provider",
        "grok",
        "go",
        script="grok-subagent",
        json_mode=False,
    )
    assert proc.returncode == 0, proc.stderr
    assert "DONE: main.py README.md" in proc.stdout
    assert "Listing files." not in proc.stdout


def test_follow_up_queue_lines(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """--follow-up is the headless queue: a queued line each, then a sent line
    before each turn, in order."""
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "one",
        "--follow-up",
        "Reply PINEAPPLE",
        "--follow-up",
        "Reply MANGO",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    dones = [e for e in events if e.get("type") == "done"]
    assert len(dones) == 3, events

    queued = [e for e in events if e.get("type") == "queue" and e["event"] == "queued"]
    sent = [e for e in events if e.get("type") == "queue" and e["event"] == "sent"]
    assert [e["text"] for e in queued] == ["Reply PINEAPPLE", "Reply MANGO"]
    assert [e["position"] for e in queued] == [0, 1]
    assert [e["id"] for e in sent] == [e["id"] for e in queued]
    assert all(e["position"] == 0 for e in sent)

    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts.index("PINEAPPLE") < texts.index("MANGO")


def test_follow_up_queue_silent_in_plain_mode(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "one",
        "--follow-up",
        "two",
        json_mode=False,
    )
    assert proc.returncode == 0, proc.stderr
    assert "queue" not in proc.stdout
    assert "echo: one" in proc.stdout
    assert "two" in proc.stdout


def test_grok_interject_fallback_is_waited_out(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """A turn grok starts on its own is bracketed in the JSON, and the queued
    follow-up runs after it rather than into it."""
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--provider",
        "grok",
        "do the steps STRAND-INTERJECTION",
        "--follow-up",
        "Reply PINEAPPLE",
        script="grok-long-turn-fallback",
        timeout=30,
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    assert len([e for e in events if e.get("type") == "done"]) == 2, events
    foreign = [e for e in events if e.get("type") == "foreign_turn"]
    assert [e["event"] for e in foreign] == ["started", "ended"], foreign
    assert foreign[0]["id"].startswith("interject-fallback-"), foreign

    order = [
        e["event"] if e.get("type") == "foreign_turn" else "sent"
        for e in events
        if e.get("type") == "foreign_turn"
        or (e.get("type") == "queue" and e["event"] == "sent")
    ]
    assert order == ["started", "ended", "sent"], order
    assert [e.get("type") for e in events].count("done") == 2, events

    users = [e for e in events if e.get("type") == "user" and e.get("interjection")]
    assert users, events


@pytest.mark.parametrize("sig", [signal.SIGINT, signal.SIGHUP], ids=lambda s: s.name)
def test_signal_clears_the_queue(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, sig: signal.Signals
) -> None:
    """A signal stops everything pending, not just the running turn. SIGHUP,
    a closed terminal, is one more way to end the run: left to its default
    action it would kill craze with the agent still running."""
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = "hang"
    env.pop("CRAZE_PROVIDER", None)
    env["CRAZE_HOME"] = str(tmp_path / "craze-home")
    proc = subprocess.Popen(
        [
            str(craze_bin),
            "prompt",
            "--json",
            "--agent-bin",
            str(fake_agent_bin),
            "--workspace",
            str(tmp_path),
            "--follow-up",
            "never runs",
            "go",
        ],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=env,
    )
    time.sleep(1.0)
    proc.send_signal(sig)
    stdout, stderr = proc.communicate(timeout=10)
    assert proc.returncode == 1, stderr
    events = parse_events(stdout)
    removed = [e for e in events if e.get("type") == "queue" and e["event"] == "removed"]
    assert len(removed) == 1, events
    assert not [e for e in events if e.get("type") == "queue" and e["event"] == "sent"]
    assert len([e for e in events if e.get("type") == "done"]) == 1, events


# The probe plugin this plan was built against: one command that spends its
# arguments and one skill that does not.
PROBE_PLUGIN = Path(__file__).resolve().parent / "fixtures" / "probe-plugin"


def command_events(events: list[dict]) -> list[dict]:
    return [e for e in events if e.get("type") == "command"]


def joined_text(events: list[dict]) -> str:
    return "".join(e.get("text", "") for e in events if e.get("type") == "text")


def test_plugin_command_expands(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    # A bare name on the very first turn, which is the whole of a one-shot
    # headless run. The bare spelling only exists once the agent's first
    # available_commands_update has been applied -- until then every plugin row
    # is qualified, so that a name accepted early cannot change meaning when the
    # catalog arrives.
    #
    # What this proves is first-turn expansion end to end, against a fake whose
    # catalog lands before the prompt does: the `commands` script advertises one
    # from session/new. It does not prove the wait of plan 010 section 3.3, and
    # is not meant to -- the wait itself, what ends it and what a cancel does to
    # it are pinned by the Go tests in internal/agent/expand_test.go, which can
    # drive the session's state directly. Against `nocommands` the same draft
    # would sit out the window and then go out verbatim.
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--plugin-dir",
        str(PROBE_PLUGIN),
        "/probe-echo banana",
        script="commands",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    cmds = command_events(events)
    assert len(cmds) == 1, events
    cmd = cmds[0]
    assert cmd["name"] == "probe-echo"
    assert cmd["qualified"] == "probe-plugin:probe-echo"
    assert cmd["plugin"] == "probe-plugin"
    assert cmd["kind"] == "command"
    assert cmd["path"].endswith("commands/probe-echo.md")
    assert "PROBE-COMMAND-EXPANDED args=[banana]" in cmd["text"]
    # The draft goes first and the block after it, which the fake's newline
    # between text blocks is what makes visible.
    text = joined_text(events)
    assert text.startswith("echo: /probe-echo banana\nThe user invoked "), text
    assert "PROBE-COMMAND-EXPANDED args=[banana]" in text


def test_plugin_qualified_name(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--plugin-dir",
        str(PROBE_PLUGIN),
        "/probe-plugin:probe-echo banana",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    cmds = command_events(events)
    assert len(cmds) == 1, events
    assert cmds[0]["qualified"] == "probe-plugin:probe-echo"
    assert "PROBE-COMMAND-EXPANDED args=[banana]" in cmds[0]["text"]
    text = joined_text(events)
    assert text.startswith("echo: /probe-plugin:probe-echo banana\nThe user invoked "), text


def test_plugin_skill_expands(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--plugin-dir",
        str(PROBE_PLUGIN),
        "/probe-plugin:probe-skill kiwi",
    )
    assert proc.returncode == 0, proc.stderr
    cmds = command_events(parse_events(proc.stdout))
    assert len(cmds) == 1, proc.stdout
    cmd = cmds[0]
    assert cmd["kind"] == "skill"
    assert cmd["path"].endswith("skills/probe-skill/SKILL.md")
    # A skill is attached whole and unsubstituted: frontmatter included, the
    # arguments only in the tag.
    assert '<skill name="probe-skill" plugin="probe-plugin" args="kiwi">' in cmd["text"]
    assert "---\nname: probe-skill\n" in cmd["text"]
    assert "PROBE-SKILL-EXPANDED" in cmd["text"]


def test_unknown_slash_is_verbatim(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--plugin-dir",
        str(PROBE_PLUGIN),
        "/nope banana",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    assert command_events(events) == []
    assert joined_text(events) == "echo: /nope banana"


def test_follow_up_expands(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--plugin-dir",
        str(PROBE_PLUGIN),
        "--follow-up",
        "/probe-plugin:probe-skill two",
        "/probe-plugin:probe-echo one",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    cmds = command_events(events)
    assert [c["kind"] for c in cmds] == ["command", "skill"], events
    assert "PROBE-COMMAND-EXPANDED args=[one]" in cmds[0]["text"]
    assert "PROBE-SKILL-EXPANDED" in cmds[1]["text"]


def test_plain_mode_silent(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--plugin-dir",
        str(PROBE_PLUGIN),
        "/probe-plugin:probe-echo banana",
        json_mode=False,
    )
    assert proc.returncode == 0, proc.stderr
    assert '"type":"command"' not in proc.stdout
    # The expansion still happened; only craze's own line is absent.
    assert proc.stdout.startswith("echo: /probe-plugin:probe-echo banana\nThe user invoked ")
    assert "PROBE-COMMAND-EXPANDED args=[banana]" in proc.stdout


def test_grok_ignores_plugin_dir(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(
        craze_bin,
        fake_agent_bin,
        tmp_path,
        "--provider",
        "grok",
        "--plugin-dir",
        str(PROBE_PLUGIN),
        "/probe-echo banana",
        script="grok-echo",
    )
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    assert command_events(events) == []
    # grok advertises its own plugin skills and expands them itself, so the
    # draft goes out as one block, exactly as it did before this existed.
    assert joined_text(events) == "echo: /probe-echo banana"
    assert "plugin dirs ignored for grok" in proc.stderr
