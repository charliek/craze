"""Plan 050 C2 through the built binary: craze models, and craze new --wait's
pre-check -- refused before any host is spawned, so no request ever reaches a
model provider -- beside the entry points that stay lenient."""

from __future__ import annotations

import json
import os
import re
import subprocess
from pathlib import Path

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
        _refused(out, {"reason": "model_unknown", "model": "nope-model"}, 'native: unknown model "nope-model"')
        assert fixture.requests == [] and _entries(_home()) == []

        # The control: the same model at a level it lists, in another case, runs.
        out = _craze(craze_bin, *native, "--model", "fixture-model", "--effort", "LOW", "--no-fast", "--exit-after-turn", "go")
        res = json.loads(out.stdout)
        assert out.returncode == 0 and res["status"] == "completed" and "warnings" not in res, out
        assert len(fixture.requests) == 1


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
