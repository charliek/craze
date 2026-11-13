"""Plan 050 C2 through the built binary: craze models, and craze new --wait's
pre-check -- refused before any host is spawned, so no request ever reaches a
model provider -- beside the entry points that stay lenient."""

from __future__ import annotations

import json
import os
import re
import socket
import subprocess
import threading
from pathlib import Path

import pytest

from sse_fixture import SSEFixture, write_native_config
from test_detach import _entries
from test_waiting import WAIT, _config, _craze, _home, _one_object, _stop


def _native_dir() -> Path:
    return Path(os.environ["CRAZE_HOME"]) / "native"


def _with_efforts(base_url: str) -> None:
    """The fixture's one model, with two effort levels."""
    write_native_config(_native_dir(), base_url)
    with (_native_dir() / "models.toml").open("a", encoding="utf-8") as f:
        f.write('efforts = ["low", "high"]\ndefault_effort = "high"\n')


def test_models_shipped_answers_under_env_i(craze_bin: Path) -> None:
    """--shipped reads no file and no environment: under `env -i` (no HOME) it
    prints the catalog's document, one object, this craze's version, every row
    sorted and stateless, the OpenAI seeds among them."""
    out = subprocess.run(["env", "-i", str(craze_bin), "models", "--shipped", "--json"], capture_output=True, timeout=WAIT, check=False)
    assert out.returncode == 0 and out.stderr == b"", out
    doc = _one_object(out)
    version = subprocess.run([str(craze_bin), "version"], capture_output=True, text=True, timeout=WAIT, check=True).stdout.strip()
    assert doc["schema"] == 1 and doc["crazeVersion"] == version, doc
    aliases = [m["alias"] for m in doc["models"]]
    assert aliases == sorted(aliases) and "openai/gpt-6.1-sol" in aliases, aliases
    assert all("state" not in m and m["source"] == "catalog" for m in doc["models"]), doc["models"]
    assert {p["id"]: p["accountModels"] for p in doc["providers"]}["chatgpt"] == "only", doc["providers"]

    # An environment every other command refuses -- the removed config-file
    # variable, a reparenting spawn's mark that names no pipe, a craze directory
    # that is no directory, no HOME -- changes not a byte.
    hostile = {"CRAZE_" + "CONFIG": "ignored", "CRAZE_REPARENT_FD": "not-a-pipe", "CRAZE_HOME": "/dev/null/nowhere", "PATH": os.environ["PATH"]}
    again = subprocess.run([str(craze_bin), "models", "--shipped", "--json"], capture_output=True, env=hostile, timeout=WAIT, check=False)
    assert again.returncode == 0 and again.stderr == b"" and again.stdout == out.stdout, again


def test_models_reads_this_craze_directory(craze_bin: Path, tmp_path: Path) -> None:
    """The local form reads the craze directory's files -- a model of the user's own
    is `user`, funded by its inline key -- and a miss is exit 1, the error object
    with its reason, one stderr line."""
    with SSEFixture() as fixture:
        _with_efforts(fixture.base_url)
        out = _craze(craze_bin, "models", "--json")
        assert out.returncode == 0, out
        (row,) = _one_object(out)["models"]
        assert row["alias"] == "fixture-model" and row["source"] == "user" and row["efforts"] == ["low", "high"], row
        assert row["state"] == {"funded": True, "offered": True}, row
        out = _craze(craze_bin, "models", "fixture-model", "--json")
        assert out.returncode == 0 and _one_object(out)["alias"] == "fixture-model", out
        out = _craze(craze_bin, "models", "no-such-model", "--json")
        err = _one_object(out)["error"]
        assert out.returncode == 1 and err["kind"] == "config", out
        assert err["detail"] == {"reason": "model_unknown", "model": "no-such-model"}, err
        assert err["message"] == 'native: unknown model "no-such-model"; craze models lists the models', err
        assert out.stderr.decode() == f"craze models: {err['message']}\n", out
        assert fixture.requests == []


def _refused(out: subprocess.CompletedProcess[bytes], detail: dict, message: str) -> None:
    """A pre-check refusal: exit 1, one object of kind config with detail, its
    message starting with message, the stderr line its words."""
    assert out.returncode == 1, out
    err = _one_object(out)["error"]
    assert err["kind"] == "config" and err["retryable"] is False and err["detail"] == detail, err
    assert err["message"].startswith(message), err
    assert out.stderr.decode() == f"craze new: {err['message']}\n", out


def test_new_wait_refuses_before_any_host(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """Each pre-check refusal of craze new --wait on native: no host is spawned (the
    registry stays empty) and the provider's server sees no request. The shipped
    catalog's retired alias (with the replacement the catalog names) is model_retired; an unknown one model_unknown."""
    _config(fake_agent_bin)
    work = str(tmp_path)
    out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "--model", "openrouter/gpt-6.1-sol", "-C", work, "go")
    _refused(
        out,
        {"reason": "model_retired", "model": "openrouter/gpt-6.1-sol", "replaced_by": "openrouter/gpt-sol-latest"},
        "native: openrouter/gpt-6.1-sol was retired; use openrouter/gpt-sol-latest",
    )
    with SSEFixture() as fixture:
        _with_efforts(fixture.base_url)
        native = ("new", "--wait", "--json", "--provider", "native", "-C", work)
        out = _craze(craze_bin, *native, "--model", "fixture-model", "--effort", "minimal", "go")
        _refused(out, {"reason": "effort_not_offered", "model": "fixture-model"}, "--effort minimal: fixture-model offers low, high")
        out = _craze(craze_bin, *native, "--model", "fixture-model", "--fast", "go")
        _refused(out, {"reason": "fast_not_offered", "model": "fixture-model"}, "--fast: fixture-model has no fast variant")
        out = _craze(craze_bin, *native, "--model", "nope-model", "go")
        _refused(out, {"reason": "model_unknown", "model": "nope-model"}, 'native: unknown model "nope-model"; craze models lists the models')
        assert _one_object(out)["error"]["message"] == 'native: unknown model "nope-model"; craze models lists the models', out
        assert fixture.requests == [] and _entries(_home()) == []

        # The control: the same model at a level it lists, in another case, runs.
        out = _craze(craze_bin, *native, "--model", "fixture-model", "--effort", "LOW", "--no-fast", "--exit-after-turn", "go")
        res = json.loads(out.stdout)
        assert out.returncode == 0 and res["status"] == "completed" and "warnings" not in res, out
        assert len(fixture.requests) == 1


class _ProxyRecorder:
    """A proxy that answers nothing: it records the first line of every
    connection made to it (an HTTPS request's CONNECT names its host) and
    closes it. With HTTPS_PROXY and HTTP_PROXY aimed here, a request any craze
    process makes to a provider's endpoint arrives here first. Read lines only
    after the with block: leaving it stops the thread, then accepts whatever
    is still queued on the listening socket, so no connection is missed."""

    def __init__(self) -> None:
        self._sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self._sock.bind(("127.0.0.1", 0))
        self._sock.listen(16)
        self._sock.settimeout(0.2)
        self.lines: list[bytes] = []
        self._done = threading.Event()
        self._thread = threading.Thread(target=self._serve, daemon=True)

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self._sock.getsockname()[1]}"

    def _serve(self) -> None:
        while not self._done.is_set():
            try:
                conn, _ = self._sock.accept()
            except OSError:
                continue
            self._record(conn)

    def _record(self, conn: socket.socket) -> None:
        with conn:
            conn.settimeout(1.0)
            try:
                self.lines.append(conn.recv(4096).split(b"\r\n", 1)[0])
            except OSError:
                self.lines.append(b"")

    def __enter__(self) -> _ProxyRecorder:
        self._thread.start()
        return self

    def __exit__(self, *exc: object) -> None:
        self._done.set()
        self._thread.join(timeout=WAIT)
        assert not self._thread.is_alive(), "the proxy recorder's thread did not stop"
        # Drain: a connection the kernel queued after the thread's last accept.
        self._sock.setblocking(False)
        while True:
            try:
                conn, _ = self._sock.accept()
            except (BlockingIOError, OSError):
                break
            self._record(conn)
        self._sock.close()


def test_an_openai_key_in_the_environment_alone_is_not_chosen(
    craze_bin: Path, fake_agent_bin: Path, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Guard 2 (G1) through the built binary, in a scratch craze directory whose
    only key is a dummy OPENAI_API_KEY: craze providers --json reports native
    needs_setup, naming the variable (never its value) and the fix; craze new
    --wait --json with no --model is the session start's kind config refusal,
    with the same reason and fix, and leaves no session; no request leaves for
    any provider. With the key stored for openai, native is ready."""
    _config(fake_agent_bin)
    dummy = "sk-guard-openai-dummy-0001"
    monkeypatch.setenv("OPENAI_API_KEY", dummy)
    reason = "OPENAI_API_KEY is set in the environment, but no key is stored for OpenAI"
    fix = "craze auth login openai, or start with --model openai/gpt-6.1-sol"
    with _ProxyRecorder() as proxy:
        for name in ("HTTPS_PROXY", "HTTP_PROXY"):
            monkeypatch.setenv(name, proxy.url)
        for name in ("https_proxy", "http_proxy", "NO_PROXY", "no_proxy", "ALL_PROXY", "all_proxy"):
            monkeypatch.delenv(name, raising=False)
        out = _craze(craze_bin, "providers", "--json")
        assert out.returncode == 0, out
        native = {p["id"]: p for p in _one_object(out)["providers"]}["native"]
        assert native == {"id": "native", "label": "native", "state": "needs_setup", "reason": reason, "fix": fix}, native

        out = _craze(craze_bin, "new", "--wait", "--json", "--provider", "native", "-C", str(tmp_path), "go")
        assert out.returncode == 1, out
        err = _one_object(out)["error"]
        assert set(err) == {"message", "kind", "retryable"} and err["kind"] == "config" and err["retryable"] is False, err
        assert f"native: {reason}, and craze does not start a new session on openai/gpt-6.1-sol (OpenAI) on its own; run {fix}" in err["message"], err
        assert dummy.encode() not in out.stdout + out.stderr, out
        assert _entries(_home()) == []
    assert proxy.lines == [], proxy.lines

    native_dir = Path(os.environ["CRAZE_HOME"]) / "native"
    native_dir.mkdir(parents=True, exist_ok=True)
    (native_dir / "providers.toml").write_text(f'version = 1\n\n[providers.openai]\napi_key = "{dummy}"\n', encoding="utf-8")
    (native_dir / "providers.toml").chmod(0o600)
    out = _craze(craze_bin, "providers", "--json")
    assert out.returncode == 0, out
    assert {p["id"]: p for p in _one_object(out)["providers"]}["native"] == {"id": "native", "label": "native", "state": "ready"}, out


def test_the_lenient_entry_points(craze_bin: Path, fake_agent_bin: Path, tmp_path: Path) -> None:
    """craze prompt and plain craze new take the flags craze new --wait refuses --
    an --effort the model does not list, and --fast -- and run; craze send --wait
    to the session plain craze new started checks nothing again."""
    _config(fake_agent_bin)
    work = tmp_path / "proj"
    work.mkdir()
    with SSEFixture() as fixture:
        _with_efforts(fixture.base_url)
        out = _craze(craze_bin, "prompt", "--provider", "native", "--workspace", str(work), "--model", "fixture-model",
                     "--effort", "minimal", "--fast", "say ok")
        assert out.returncode == 0, out
        seen = len(fixture.requests)
        assert seen >= 1

        out = _craze(craze_bin, "new", "--json", "--provider", "native", "--model", "fixture-model", "--effort", "minimal",
                     "--fast", "-C", str(work), "start it")
        assert out.returncode == 0, out
        (entry,) = _entries(_home())
        short = entry["crazeSessionId"][-8:]
        out = _craze(craze_bin, "send", short, "--wait", "--json", "and again")
        res = json.loads(out.stdout)
        assert out.returncode == 0 and res["status"] == "completed" and re.fullmatch(r"turn-\d+", res["turn"]), out
        assert len(fixture.requests) >= seen + 2
        _stop(craze_bin, short)
