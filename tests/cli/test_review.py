from __future__ import annotations

import json
import os
import shlex
import subprocess
import sys
from pathlib import Path

import pytest


def configure(work: Path, fake: Path, codex_script: str) -> None:
    subprocess.run(["git", "init", "--quiet", work], check=True)
    home = Path(os.environ["CRAZE_HOME"])
    home.mkdir(parents=True, exist_ok=True)
    wrapper = home / "fake-codex"
    wrapper.write_text(f"#!/bin/sh\nexec {shlex.quote(str(fake))} -script {codex_script} \"$@\"\n")
    wrapper.chmod(0o700)
    (home / "config.toml").write_text(
        f'[agents]\ncodex = "{wrapper}"\ncursor = "{fake}"\n'
        '[targets.test]\nmaker = "test"\neffort = "low"\n'
        '[[targets.test.routes]]\nprovider = "codex"\nmodel = "gpt-6.2-sol"\nbilling = "subscription"\n'
        '[[targets.test.routes]]\nprovider = "cursor"\nmodel = "m-1"\nbilling = "subscription"\n'
    )


def review(binary: Path, work: Path, *args: str, env: dict[str, str] | None = None):
    process = subprocess.run(
        [str(binary), "review", "-C", str(work), "--json", *args],
        env={**os.environ, "CRAZE_FAKE_SCRIPT": "echo", "CODEX_API_KEY": "", **(env or {})},
        stdin=subprocess.DEVNULL, capture_output=True, timeout=60,
    )
    return process, json.loads(process.stdout)


@pytest.mark.parametrize("script,code,kind,count", [
    ("codex-quota", 0, "quota", 2),
    ("codex-subscription-refused", 0, "auth", 2),
    ("codex-dirty-quota", 3, "quota", 1),
    ("codex-ask-freetext", 4, None, 1),
])
def test_real_host_fallback(craze_bin, fake_agent_bin, tmp_path, script, code, kind, count):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, script)
    proc, doc = review(craze_bin, work, "--target", "test", "--allow-same-provider", "review")
    assert proc.returncode == code, (proc.stderr, doc)
    assert len(doc["attempts"]) == count, doc
    if kind:
        assert doc["attempts"][0]["kind"] == kind, doc
    if code == 0:
        assert doc["route"]["provider"] == "cursor" and doc["message"], doc
    if script == "codex-dirty-quota":
        assert doc["error"]["kind"] == "dirty_tree" and doc["dirtyFiles"], doc


def test_direct_paid_and_self_guard(craze_bin, fake_agent_bin, tmp_path):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-basic")
    args = ["--provider", "codex", "--allow-same-provider", "inspect"]
    env = {"CODEX_API_KEY": "test-key"}
    proc, doc = review(craze_bin, work, *args, env=env)
    assert proc.returncode == 1 and doc["error"]["kind"] == "config", doc
    proc, doc = review(craze_bin, work, *args, "--allow-paid-fallback", env=env)
    assert proc.returncode == 0 and doc["route"]["billing"] == "api_key", (doc, proc.stderr)
    proc, doc = review(craze_bin, work, "--provider", "codex", "inspect", env={"CRAZE_SESSION_PROVIDER": "codex"})
    assert proc.returncode == 2 and doc["error"]["kind"] == "usage", doc


def test_bundle_only_from_real_cli(craze_bin, fake_agent_bin, tmp_path):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-basic")
    (work / "changed").write_text("line\n" * 950)
    proc, doc = review(craze_bin, work, "--bundle-only", "--changes-since", "HEAD")
    assert proc.returncode == 0 and not doc["inlined"], (proc.stderr, doc)
    spill = Path(doc["bundleFile"])
    try:
        assert doc["lines"] >= 950 and doc["bytes"] == spill.stat().st_size
        assert doc["files"][0]["first"] <= doc["files"][0]["last"]
    finally:
        spill.unlink()
        spill.parent.rmdir()


def test_signal_during_second_host_start(craze_bin, fake_agent_bin, tmp_path):
    import signal
    import time

    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-quota")
    home = Path(os.environ["CRAZE_HOME"])
    marker = tmp_path / "second-started"
    delayed = tmp_path / "slow-cursor"
    # The marker proves the first attempt was already cleaned up and the
    # second provider is in its startup handshake when the signal arrives.
    delayed.write_text(f"#!/bin/sh\nprintf '%s\\n' \"$$\" > {shlex.quote(str(marker))}\nexec sleep 90\n")
    delayed.chmod(0o700)
    cfg = home / "config.toml"
    cfg.write_text(cfg.read_text().replace(f'cursor = "{fake_agent_bin}"', f'cursor = "{delayed}"'))
    proc = subprocess.Popen(
        [str(craze_bin), "review", "-C", str(work), "--target", "test", "--allow-same-provider",
         "--timeout", "2m", "--json", "check"],
        stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        env={**os.environ, "CODEX_API_KEY": ""},
    )
    try:
        end = time.monotonic() + 30
        while not marker.exists() and proc.poll() is None and time.monotonic() < end:
            time.sleep(0.025)
        assert marker.exists(), "fallback provider did not start"
        proc.send_signal(signal.SIGTERM)
        out, err = proc.communicate(timeout=30)
        doc = json.loads(out)
        assert proc.returncode == 3 and doc["status"] == "cancelled", (doc, err)
        assert [a["provider"] for a in doc["attempts"]] == ["codex", "cursor"], doc
        with pytest.raises(ProcessLookupError):
            os.kill(int(marker.read_text()), 0)
    finally:
        if proc.poll() is None:
            proc.terminate()
            proc.wait(timeout=30)


def test_codex_subscription_refusal_falls_back_to_native_plan(
    craze_bin, fake_agent_bin, tmp_path
):
    from datetime import UTC, datetime, timedelta
    import tomllib

    from sse_fixture import SSEFixture, answer, call_step, write_native_config

    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-subscription-refused")
    home = Path(os.environ["CRAZE_HOME"])
    cfg = home / "config.toml"
    cfg.write_text(
        cfg.read_text().replace(
            'provider = "cursor"\nmodel = "m-1"',
            'provider = "native"\nmodel = "chatgpt/gpt-6.2-sol"',
        )
    )
    native = home / "native"
    auth = native / "auth"
    auth.mkdir(parents=True)

    def write(path, obj):
        path.write_text(json.dumps(obj))
        path.chmod(0o600)

    client, subject = "app_review_fixture", "review-subject"
    now = datetime.now(UTC)
    catalog = (
        Path(__file__).resolve().parents[2] / "internal/harness/modeltable/catalog.toml"
    )
    version = tomllib.loads(catalog.read_text())["chatgpt_defaults"][
        "models_client_version"
    ]
    write(
        auth / "chatgpt-client.json",
        dict(client_id=client, subject=subject, plan_usage=True, notice_shown=True),
    )
    write(
        auth / "chatgpt.json",
        dict(
            version=1,
            client_id=client,
            subject=subject,
            issuer="https://auth.openai.com",
            scopes=["openid", "offline_access", "chatgpt.tokens.use.direct"],
            access_token="fixture-access",
            refresh_token="fixture-refresh",
            id_token="fixture-id",
            access_expires_at=(now + timedelta(hours=1)).isoformat(),
            incarnation="review-1",
            generation=1,
        ),
    )
    write(
        native / "chatgpt-models.json",
        dict(
            version=1,
            client_id=client,
            subject=subject,
            client_version=version,
            fetched_at=now.isoformat(),
            models=[
                dict(
                    slug="gpt-6.2-sol",
                    display_name="Fixture Sol",
                    context_window=200000,
                    efforts=["low"],
                    default_effort="low",
                )
            ],
        ),
    )
    server = SSEFixture().start()
    paid = SSEFixture().start()
    try:
        write_native_config(native, paid.base_url, alias="paid-child")
        models = native / "models.toml"
        models.write_text(
            models.read_text().replace("catalog = false", "catalog = true")
        )
        server.set_script(
            [
                call_step(
                    "agent",
                    {
                        "description": "delegate",
                        "prompt": "review",
                        "model": "paid-child",
                    },
                ),
                answer("native review complete"),
            ]
        )
        proc, doc = review(
            craze_bin,
            work,
            "--target",
            "test",
            "--allow-same-provider",
            "inspect",
            env={
                "CRAZE_TEST_CHATGPT_API": server.base_url,
                "CRAZE_TEST_CHATGPT_ISSUER": server.base_url.removesuffix("/v1"),
            },
        )
        assert proc.returncode == 0, (doc, proc.stderr)
        assert [a["provider"] for a in doc["attempts"]] == ["codex", "native"], doc
        assert doc["attempts"][0]["kind"] == "auth" and doc["dirtyFiles"] == [], doc
        assert "native review complete" in doc["message"], doc
        assert len(server.requests) == 2
        for req in server.requests:
            (namespace,) = req.body["tools"]
            assert namespace["name"] == "craze"
            assert {t["name"] for t in namespace["tools"]} == {"read", "grep", "glob"}
        assert "agent" in json.dumps(server.requests[-1].input)
        assert not paid.requests
    finally:
        server.close()
        paid.close()


def test_explicit_cap_covers_routing_lookup(craze_bin, tmp_path):
    home = tmp_path / "routed-home"
    home.mkdir()
    os.mkfifo(home / "config.toml", 0o600)
    proc = subprocess.run(
        [
            str(craze_bin),
            "review",
            "--target",
            "sol",
            "--timeout",
            "150ms",
            "--json",
            "inspect",
        ],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        timeout=10,
        env={**os.environ, "CRAZE_HOME": str(home)},
    )
    doc = json.loads(proc.stdout)
    assert proc.returncode == 5 and doc["status"] == "timed_out" and doc["stopping"], (
        doc,
        proc.stderr,
    )
    assert doc["attempts"] == [], doc


def test_paid_second_candidate_requires_opt_in(craze_bin, fake_agent_bin, tmp_path):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-quota")
    args = ("--target", "test", "--allow-same-provider", "inspect")
    env = {"CURSOR_API_KEY": "fixture-paid-credential"}
    proc, doc = review(craze_bin, work, *args, env=env)
    assert proc.returncode == 3 and [a["provider"] for a in doc["attempts"]] == [
        "codex"
    ], doc
    proc, doc = review(craze_bin, work, *args, "--allow-paid-fallback", env=env)
    assert proc.returncode == 0 and doc["attempts"][1]["billing"] == "api_key", doc


def test_closed_stdout_preserves_full_file(craze_bin, fake_agent_bin, tmp_path):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-basic")
    out = tmp_path / "review.md"
    readfd, writefd = os.pipe()
    os.close(readfd)
    with os.fdopen(writefd, "wb") as writer:
        p = subprocess.run(
            [
                str(craze_bin),
                "review",
                "-C",
                str(work),
                "--provider",
                "cursor",
                "--allow-same-provider",
                "--out",
                str(out),
                "check all the bytes",
            ],
            stdin=subprocess.DEVNULL,
            stdout=writer,
            stderr=subprocess.PIPE,
            env={**os.environ, "CRAZE_FAKE_SCRIPT": "echo"},
            timeout=30,
        )
    assert p.returncode == 1 and "check all the bytes" in out.read_text(), p.stderr


def test_index_damage_preserves_review(craze_bin, fake_agent_bin, tmp_path):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-basic")
    (work / "new").write_text("original")
    subprocess.run(["git", "-C", str(work), "add", "new"], check=True)
    out = tmp_path / "review.md"
    proc, doc = review(
        craze_bin,
        work,
        "--provider",
        "cursor",
        "--allow-same-provider",
        "--out",
        str(out),
        "inspect",
        env={"CRAZE_FAKE_SCRIPT": "review-damaged-index"},
    )
    assert (
        proc.returncode == 3
        and doc["error"]["kind"] == "dirty_tree"
        and "index" in doc["dirtyFiles"]
    ), (doc, proc.stderr)
    assert doc["message"] == out.read_text() and "review retained" in doc["message"], (
        doc
    )


def test_wire_effort_clamp(craze_bin, fake_agent_bin, tmp_path):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-model-multipage")
    proc, doc = review(
        craze_bin,
        work,
        "--provider",
        "codex",
        "--model",
        "gpt-6.2-sol",
        "--allow-same-provider",
        "--effort",
        "xhigh",
        "inspect",
    )
    assert proc.returncode == 0 and doc["attempts"][0]["effectiveEffort"] == "high", (
        doc,
        proc.stderr,
    )


def test_invalid_ref_before_unavailable_route(craze_bin, fake_agent_bin, tmp_path):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-basic")
    for ref in ("--bad", "unresolved-revision"):
        proc, doc = review(
            craze_bin,
            work,
            "--provider",
            "codex",
            "--allow-same-provider",
            "--changes-since=" + ref,
            env={"CODEX_API_KEY": "fixture"},
        )
        assert proc.returncode == 2 and doc["error"]["kind"] == "usage", (
            doc,
            proc.stderr,
        )


def test_fallback_reaps_adapter_created_session(craze_bin, fake_agent_bin, tmp_path):
    work = tmp_path / "work"
    configure(work, fake_agent_bin, "codex-quota")
    home = Path(os.environ["CRAZE_HOME"])
    marker = tmp_path / "helper.pid"
    wrapper = home / "fake-codex"
    wrapper.write_text(
        f"#!{sys.executable}\nimport os, subprocess\n"
        f"helper=subprocess.Popen(['sleep','45'],start_new_session=True,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)\n"
        f"open({str(marker)!r},'w').write(str(helper.pid))\n"
        f"os.execv({str(fake_agent_bin)!r},[{str(fake_agent_bin)!r},'-script','codex-quota',*__import__('sys').argv[1:]])\n"
    )
    proc, doc = review(
        craze_bin, work, "--target", "test", "--allow-same-provider", "inspect"
    )
    assert proc.returncode == 0 and len(doc["attempts"]) == 2, (doc, proc.stderr)
    ps = subprocess.run(
        ["ps", "-p", marker.read_text(), "-o", "stat="], capture_output=True, text=True
    )
    assert ps.returncode != 0 or ps.stdout.strip().startswith("Z"), ps.stdout
