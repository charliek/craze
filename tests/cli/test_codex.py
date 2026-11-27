"""Plan 054: `--provider codex` end-to-end through the built binary over the fake
codex app-server (`cmd/craze-fake-agent` with `codex-*` scripts).
"""

from __future__ import annotations

import json
import os
import subprocess
import time
from pathlib import Path

from conftest import TEST_HOST_IDLE_EXIT
from test_detach import _entries

WAIT = 10.0


def _config(fake_agent_bin: Path) -> None:
    craze_home = Path(os.environ["CRAZE_HOME"])
    craze_home.mkdir(parents=True, exist_ok=True)
    (craze_home / "config.toml").write_text(
        f'host_idle_exit = "{TEST_HOST_IDLE_EXIT}"\n'
        f'host_idle_exit_unattended = "{TEST_HOST_IDLE_EXIT}"\n\n'
        f"[agents]\n"
        f'codex = "{fake_agent_bin}"\n',
        encoding="utf-8",
    )


def _craze(
    craze_bin: Path,
    *args: str,
    script: str = "codex-basic",
    env_extra: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[bytes]:
    env = os.environ.copy()
    env["CRAZE_FAKE_SCRIPT"] = script
    if env_extra:
        env.update(env_extra)
    return subprocess.run(
        [str(craze_bin), *args],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        env=env,
        timeout=6 * WAIT,
    )


def _home() -> Path:
    return Path(os.environ["HOME"])


def _stop(craze_bin: Path, short: str) -> None:
    out = _craze(craze_bin, "stop", short)
    assert out.returncode == 0, out
    deadline = time.monotonic() + WAIT
    while any(e.get("crazeSessionId", "").endswith(short) for e in _entries(_home())):
        assert time.monotonic() < deadline, f"session {short}'s host is still registered"
        time.sleep(0.05)


def test_providers_lists_codex_between_antigravity_and_native(
    craze_bin: Path, fake_agent_bin: Path
) -> None:
    _config(fake_agent_bin)
    out = _craze(craze_bin, "providers", "--json")
    assert out.returncode == 0 and out.stderr == b"", out
    rows = json.loads(out.stdout)["providers"]
    ids = [r["id"] for r in rows]
    assert ids == ["cursor", "grok", "claude", "antigravity", "codex", "native"], rows
    codex = next(r for r in rows if r["id"] == "codex")
    assert codex["state"] == "ready", codex


def test_codex_new_wait_edits_and_usage(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()

    # codex-edits adds added.txt, moves updated.txt -> moved.txt, and declines failed.txt.
    out = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "-C",
        str(work),
        "edit files",
        script="codex-edits",
    )
    assert out.returncode == 0 and out.stderr == b"", out
    res = json.loads(out.stdout)
    assert res["status"] == "completed", res
    assert res["provider"] == "codex", res
    assert res["stopping"] is True, res
    assert sorted(res["files"]) == ["added.txt", "moved.txt", "updated.txt"], res
    assert "failed.txt" not in res["files"], res

    # codex-basic reports unpriced token usage, context window and plan.
    basic = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "-C",
        str(work),
        "hello usage",
        script="codex-basic",
    )
    assert basic.returncode == 0 and basic.stderr == b"", basic
    bres = json.loads(basic.stdout)
    assert bres["status"] == "completed", bres
    assert bres.get("usage"), bres
    assert bres["usage"]["plan"] == "prolite", bres["usage"]
    assert bres["usage"]["contextWindow"] == 475000, bres["usage"]
    assert bres["usage"]["turn"]["unpriced"] is True, bres["usage"]
    assert bres["usage"]["turn"]["reasoning"] == 25, bres["usage"]


def test_codex_no_force_approval_needs_input(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()

    # Without --exit-after-turn: host stays open with handle until stopped.
    kept = _craze(
        craze_bin,
        "new",
        "--wait",
        "--json",
        "--provider",
        "codex",
        "--no-force",
        "-C",
        str(work),
        "run command",
        script="codex-approval",
    )
    assert kept.returncode == 4, kept
    kres = json.loads(kept.stdout)
    assert kres["status"] == "needs_input", kres
    assert kres.get("ask", {}).get("kind") == "permission", kres
    assert kres.get("handle"), kres
    _stop(craze_bin, kres["short"])

    # With --exit-after-turn: ask is abandoned and host exits cleanly.
    fenced = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "--no-force",
        "-C",
        str(work),
        "run command",
        script="codex-approval",
    )
    assert fenced.returncode == 4, fenced
    fres = json.loads(fenced.stdout)
    assert fres["status"] == "needs_input", fres
    assert fres.get("stopping") is True, fres
    assert "handle" not in fres, fres
    assert fres.get("ask", {}).get("kind") == "permission", fres
    deadline = time.monotonic() + WAIT
    while any(e.get("crazeSessionId") == fres["session"] for e in _entries(_home())):
        assert time.monotonic() < deadline, "fenced needs_input host did not exit"
        time.sleep(0.05)


def test_codex_logged_out_fails_fast_with_auth_kind(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()

    out = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "-C",
        str(work),
        "hello",
        script="codex-loggedout",
    )
    assert out.returncode == 1, out
    res = json.loads(out.stdout)
    assert res["error"]["kind"] == "auth", res
    assert "codex login" in res["error"]["message"], res


def test_codex_self_delegation_guard(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()

    # Single CODEX_THREAD_ID refuses codex target with exit 2.
    refused = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "-C",
        str(work),
        "hello",
        env_extra={"CODEX_THREAD_ID": "thread-self"},
    )
    assert refused.returncode == 2, refused
    err = json.loads(refused.stdout)["error"]
    assert err["kind"] == "usage", err
    assert "this agent already runs on codex" in err["message"], err

    # Two standalone markers (CODEX_THREAD_ID + CLAUDECODE=1) stand aside.
    both = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "-C",
        str(work),
        "hello both",
        env_extra={"CODEX_THREAD_ID": "thread-self", "CLAUDECODE": "1"},
    )
    assert both.returncode == 0, both
    assert json.loads(both.stdout)["status"] == "completed", both

    # --allow-same-provider overrides single marker.
    allowed = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "--allow-same-provider",
        "-C",
        str(work),
        "hello allowed",
        env_extra={"CODEX_THREAD_ID": "thread-self"},
    )
    assert allowed.returncode == 0, allowed
    assert json.loads(allowed.stdout)["status"] == "completed", allowed


def test_codex_ask_and_plan_modes_request_readonly_sandbox(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path
) -> None:
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()

    ask_out = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "--ask",
        "-C",
        str(work),
        "check sandbox",
        script="codex-basic",
    )
    assert ask_out.returncode == 0 and ask_out.stderr == b"", ask_out
    ask_res = json.loads(ask_out.stdout)
    assert ask_res["status"] == "completed", ask_res
    assert "mode=default" in ask_res["message"], ask_res
    assert "sandbox=readOnly" in ask_res["message"], ask_res
    assert "approval=never" in ask_res["message"], ask_res

    plan_out = _craze(
        craze_bin,
        "new",
        "--wait",
        "--exit-after-turn",
        "--json",
        "--provider",
        "codex",
        "--plan",
        "-C",
        str(work),
        "check plan",
        script="codex-basic",
    )
    assert plan_out.returncode == 0 and plan_out.stderr == b"", plan_out
    plan_res = json.loads(plan_out.stdout)
    assert plan_res["status"] == "completed", plan_res
    assert plan_res["message"] == "Plan for: check plan", plan_res
