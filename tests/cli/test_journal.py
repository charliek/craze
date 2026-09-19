"""The session journal through the binary — plan 020 §3.4–3.5, A18/A19/A24.

The Go tests own the file's contents event by event (they can decode a body
with the codec, which nothing here can). This owns the command: that a real
`craze prompt` run writes one journal where craze says it does, with modes a
user can live with and lines a script can read; that each opt-out leaves
nothing on disk and says why exactly once when it fires on a mistake; and
that `craze frame` — hermetic by design — never journals at all.
"""

from __future__ import annotations

import json
import os
import re
import stat
import subprocess
from pathlib import Path

import pytest

# <UTC yyyymmddThhmmssZ>_<incarnation>.jsonl
JOURNAL_NAME = re.compile(r"^\d{8}T\d{6}Z_[0-9a-fA-F-]{36}\.jsonl$")

# The keys every line of a type must carry, beyond ts and type.
REQUIRED_KEYS = {
    "header": {
        "format",
        "eventCodec",
        "incarnation",
        "crazeVersion",
        "os",
        "arch",
        "provider",
        "agentBinary",
        "cwd",
        "force",
        "interactive",
        "mode",
        "pid",
    },
    "session": {"providerSessionId"},
    "event": {"seq", "eventType"},
    "diag": {"kind", "fields"},
    "gap": {"droppedEvents", "droppedNotes", "error"},
    "prompt": {"attempt", "text", "kind"},
    "prompt_end": {"attempt", "durationMs"},
}


def run_prompt(
    craze_bin: Path,
    fake_agent_bin: Path,
    workspace: Path,
    craze_home: Path,
    *args: str,
    script: str = "echo",
    env_extra: dict[str, str] | None = None,
    timeout: float = 10,
) -> subprocess.CompletedProcess[str]:
    """One headless turn against the fake agent, journaling into craze_home."""
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = script
    env.pop("CRAZE_PROVIDER", None)
    env["CRAZE_HOME"] = str(craze_home)
    env["XAI_API_KEY"] = ""
    env["GROK_CODE_XAI_API_KEY"] = ""
    env.update(env_extra or {})
    return subprocess.run(
        [
            str(craze_bin),
            "prompt",
            "--json",
            "--agent-bin",
            str(fake_agent_bin),
            "--workspace",
            str(workspace),
            *args,
            "hello",
        ],
        check=False,
        capture_output=True,
        text=True,
        env=env,
        timeout=timeout,
    )


def journals(craze_home: Path) -> list[Path]:
    """Every journal file under craze_home: <journal>/<slug>/<stamp>_<id>.jsonl."""
    return sorted((craze_home / "journal").glob("*/*.jsonl"))


def lines_of(path: Path) -> list[dict]:
    """Every line of a journal, parsed, with the keys its type owes present."""
    out = []
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = json.loads(raw)
        assert line.get("ts"), f"a line has no ts: {raw}"
        kind = line.get("type")
        assert kind in REQUIRED_KEYS, f"unknown line type: {raw}"
        missing = REQUIRED_KEYS[kind] - line.keys()
        assert not missing, f"a {kind} line is missing {sorted(missing)}: {raw}"
        out.append(line)
    return out


def journal_diags(stderr: str) -> list[str]:
    """The lines craze wrote about the journal being off."""
    return [ln for ln in stderr.splitlines() if "journal off" in ln]


def test_prompt_writes_one_journal(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    workspace = tmp_path / "ws"
    workspace.mkdir()
    craze_home = tmp_path / "craze-home"
    proc = run_prompt(craze_bin, fake_agent_bin, workspace, craze_home)
    assert proc.returncode == 0, proc.stderr
    assert journal_diags(proc.stderr) == [], proc.stderr

    files = journals(craze_home)
    assert len(files) == 1, files
    path = files[0]
    assert JOURNAL_NAME.match(path.name), path.name
    assert path.parent.parent == craze_home / "journal"

    # Modes are "no broader than": a stricter umask may narrow them.
    for directory in (craze_home / "journal", path.parent):
        mode = stat.S_IMODE(directory.stat().st_mode)
        assert mode & ~0o700 == 0, f"{directory} is {oct(mode)}"
    mode = stat.S_IMODE(path.stat().st_mode)
    assert mode & ~0o600 == 0, f"{path} is {oct(mode)}"

    lines = lines_of(path)
    header = lines[0]
    assert header["type"] == "header", lines[0]
    assert header["format"] == 1 and header["eventCodec"] == 1
    assert header["provider"] == "cursor"
    assert header["agentBinary"] == str(fake_agent_bin)
    assert header["cwd"] == str(workspace)
    assert header["force"] is True and header["interactive"] is False
    assert isinstance(header["pid"], int) and header["pid"] > 0
    assert header["incarnation"] and header["incarnation"] in path.name
    assert not any(ln["type"] == "header" for ln in lines[1:]), "two headers"

    sessions = [ln for ln in lines if ln["type"] == "session"]
    assert len(sessions) == 1, sessions
    assert sessions[0]["providerSessionId"], sessions[0]
    # The binary the spawn resolved, not the one asked for: here they agree,
    # because the test passes an absolute --agent-bin.
    assert sessions[0]["agentBinary"] == str(fake_agent_bin)

    seqs = [ln["seq"] for ln in lines if ln["type"] == "event"]
    assert seqs, "the turn journaled no events"
    assert seqs == sorted(set(seqs)) and seqs[0] == 1, seqs

    # The `seq` on a `--json` line is this same number, which is what lets a
    # headless caller line its stream against the journal (plan 020 §3.6). The
    # stream is a lossy projection, so its seqs are a subset -- in order, and
    # never contiguous by accident.
    stream = [json.loads(ln) for ln in proc.stdout.splitlines() if ln.strip()]
    stream_seqs = [ln["seq"] for ln in stream if "seq" in ln]
    assert stream_seqs, proc.stdout
    assert stream_seqs == sorted(stream_seqs), stream_seqs
    assert set(stream_seqs) <= set(seqs), (stream_seqs, seqs)
    types = [ln["eventType"] for ln in lines if ln["type"] == "event"]
    assert "text" in types and "done" in types, types

    assert lines[-1]["type"] == "diag" and lines[-1]["kind"] == "closing", lines[-1]
    assert [ln for ln in lines if ln["type"] == "gap"] == [], "the journal fell behind"

    # The prompt notes: the draft as it was typed, and how the attempt ended.
    prompts = [ln for ln in lines if ln["type"] == "prompt"]
    assert len(prompts) == 1, prompts
    assert prompts[0]["text"] == "hello", prompts[0]
    assert prompts[0]["kind"] == "prompt", prompts[0]
    ends = [ln for ln in lines if ln["type"] == "prompt_end"]
    assert len(ends) == 1, ends
    assert ends[0]["attempt"] == prompts[0]["attempt"], (prompts[0], ends[0])
    assert ends[0]["stopReason"] == "end_turn", ends[0]
    assert "errClass" not in ends[0], ends[0]
    assert isinstance(ends[0]["durationMs"], int), ends[0]


def test_journal_off_leaves_no_directory(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    """Either switch turns it off, and off means nothing on disk at all."""
    for name, env_extra, config in (
        ("env", {"CRAZE_JOURNAL": "0"}, None),
        ("config", {}, "journal = false\n"),
        # The env var cannot turn journaling on against the config.
        ("env against config", {"CRAZE_JOURNAL": "1"}, "journal = false\n"),
    ):
        workspace = tmp_path / name.replace(" ", "-")
        workspace.mkdir()
        craze_home = workspace / "craze-home"
        if config is not None:
            craze_home.mkdir()
            (craze_home / "config.toml").write_text(config, encoding="utf-8")
        proc = run_prompt(
            craze_bin, fake_agent_bin, workspace, craze_home, env_extra=env_extra
        )
        assert proc.returncode == 0, proc.stderr
        assert not (craze_home / "journal").exists(), name
        # An explicit opt-out is the user's own choice: nothing is printed.
        assert journal_diags(proc.stderr) == [], (name, proc.stderr)


@pytest.mark.parametrize(
    "name,env_extra,config,want",
    [
        ("env", {"CRAZE_JOURNAL": "maybe"}, None, "CRAZE_JOURNAL"),
        ("config", {}, 'journal = "false"\n', "journal is not a bool"),
    ],
)
def test_a_switch_craze_cannot_read_fails_closed(
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    name: str,
    env_extra: dict[str, str],
    config: str | None,
    want: str,
) -> None:
    """A24: a value craze cannot read is off, and says so in exactly one line."""
    workspace = tmp_path / name
    workspace.mkdir()
    craze_home = workspace / "craze-home"
    if config is not None:
        craze_home.mkdir()
        (craze_home / "config.toml").write_text(config, encoding="utf-8")
    proc = run_prompt(
        craze_bin, fake_agent_bin, workspace, craze_home, env_extra=env_extra
    )
    assert proc.returncode == 0, proc.stderr
    assert not (craze_home / "journal").exists()
    diags = journal_diags(proc.stderr)
    assert len(diags) == 1 and want in diags[0], proc.stderr


def run_frame(
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    *extra: str,
    script: str,
    keys: str,
) -> subprocess.CompletedProcess[str]:
    """`craze frame` as test_frame.py drives it, with its own craze directory.

    The runner isolates HOME and unsets CRAZE_HOME for the run itself, so
    CRAZE_HOME here is what the *fresh* construction path would journal into:
    it builds its session before the isolation. TMPDIR is this test's too, so
    the runner's own temporary home is somewhere this test can look.
    """
    work = tmp_path / "ws"
    work.mkdir(exist_ok=True)
    env = os.environ.copy()
    env["HOME"] = str(tmp_path / "home")
    env["CRAZE_HOME"] = str(tmp_path / "craze-home")
    env["TMPDIR"] = str(tmp_path / "tmp")
    Path(env["HOME"]).mkdir(exist_ok=True)
    Path(env["TMPDIR"]).mkdir(exist_ok=True)
    env.pop("CRAZE_AGENT_BIN", None)
    env.pop("CRAZE_PROVIDER", None)
    env["CRAZE_FAKE_SCRIPT"] = ""
    return subprocess.run(
        [
            str(craze_bin),
            "frame",
            "--cols",
            "100",
            "--rows",
            "30",
            "--agent-bin",
            str(fake_agent_bin),
            "--fake-script",
            script,
            "--keys",
            keys,
            "--timeout",
            "20s",
            *extra,
        ],
        capture_output=True,
        text=True,
        cwd=str(work),
        env=env,
        timeout=120,
    )


@pytest.mark.parametrize(
    "name,extra,script,keys",
    [
        ("fresh", (), "echo", "<wait:idle>go<enter><wait:text:echo: go>"),
        (
            "resumed",
            ("--seed-session", "cursor:sess-load-1:yesterday", "--continue"),
            "load",
            "<wait:text:restored><wait:idle>",
        ),
    ],
)
def test_frame_never_journals(
    craze_bin: Path,
    fake_agent_bin: Path,
    tmp_path: Path,
    name: str,
    extra: tuple[str, ...],
    script: str,
    keys: str,
) -> None:
    """`craze frame` never asks the resolver, on either construction path."""
    proc = run_frame(craze_bin, fake_agent_bin, tmp_path, *extra, script=script, keys=keys)
    assert proc.returncode == 0, f"{proc.returncode}: {proc.stderr[-2000:]}"
    assert "restored" in proc.stdout or name == "fresh", proc.stdout
    left = [p for p in tmp_path.rglob("*") if p.name == "journal"]
    assert left == [], left
    assert list(tmp_path.rglob("*.jsonl")) == [], list(tmp_path.rglob("*.jsonl"))
