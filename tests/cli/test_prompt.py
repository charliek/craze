from __future__ import annotations

import json
import os
import signal
import subprocess
import time
from pathlib import Path

import pytest


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
) -> subprocess.CompletedProcess[str]:
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = script
    cmd = [
        str(craze_bin),
        "prompt",
        "--json",
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


def test_first_turn_echo(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    proc = run_prompt(craze_bin, fake_agent_bin, tmp_path, "hello")
    assert proc.returncode == 0, proc.stderr
    events = parse_events(proc.stdout)
    texts = "".join(e.get("text", "") for e in events if e.get("type") == "text")
    assert texts == "echo: hello"
    assert events[-1] == {"type": "done", "stopReason": "end_turn"}
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
