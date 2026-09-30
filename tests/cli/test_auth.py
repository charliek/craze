"""Plan 031 C4: `craze auth login|logout|list` from the outside (§3.7).

The native provider's keys are stored as the inline api_key of their
providers.toml entry, in the craze directory's native/ (0600), by the real
binary against a temp CRAZE_HOME (isolate_run_env: every variable the shipped
catalog names is unset, so its four providers start unconnected). stdin is a
pipe here, never a terminal, so the key is always stdin's first line; the
no-echo prompt on a terminal is the Go pty test's
(internal/cli/auth_test.go).

Canary checks (A7): the key never appears in stdout or stderr, in any error,
in the journal of a native session it funds, or in any file but
providers.toml.
"""

from __future__ import annotations

import os
import subprocess
from pathlib import Path

import pytest

from sse_fixture import CANARY, UNUSED_ENV_KEY, SSEFixture

SHIPPED_ROWS = [
    ["Fireworks", "fireworks"],
    ["Meta", "meta"],
    ["OpenRouter", "openrouter"],
    ["Z.AI Coding Plan", "zai-coding-plan"],
]


@pytest.fixture
def fixture_server():
    server = SSEFixture().start()
    try:
        yield server
    finally:
        server.close()


def craze_home() -> Path:
    return Path(os.environ["CRAZE_HOME"])


def providers_toml() -> Path:
    return craze_home() / "native" / "providers.toml"


def run_craze(
    craze_bin: Path, *args: str, stdin: str = "", env: dict[str, str] | None = None, timeout: float = 10
) -> subprocess.CompletedProcess[str]:
    child = os.environ.copy()
    child.update(env or {})
    proc = subprocess.run(
        [str(craze_bin), *args],
        input=stdin,
        capture_output=True,
        text=True,
        env=child,
        timeout=timeout,
        check=False,
    )
    assert CANARY not in proc.stdout, proc.stdout
    assert CANARY not in proc.stderr, proc.stderr
    return proc


def list_rows(stdout: str) -> list[list[str]]:
    """`craze auth list`'s rows, each split on its column gaps (2+ spaces)."""
    rows = []
    for line in stdout.splitlines():
        cells = [c for c in line.split("  ") if c.strip()]
        rows.append([c.strip() for c in cells])
    return rows


def files_holding(root: Path, needle: bytes) -> list[Path]:
    return sorted(p for p in root.rglob("*") if p.is_file() and needle in p.read_bytes())


def test_login_list_logout(craze_bin: Path) -> None:
    proc = run_craze(craze_bin, "auth", "list")
    assert proc.returncode == 0, proc.stderr
    assert list_rows(proc.stdout) == [row + ["not connected"] for row in SHIPPED_ROWS]

    proc = run_craze(craze_bin, "auth", "login", "fireworks", stdin=CANARY + "\n")
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout == f"Saved the Fireworks key in {providers_toml()}.\n"
    assert proc.stderr == ""
    assert providers_toml().stat().st_mode & 0o777 == 0o600
    assert (craze_home() / "native").stat().st_mode & 0o777 == 0o700
    assert CANARY in providers_toml().read_text(encoding="utf-8")
    assert not (craze_home() / "native" / "models.toml").exists()

    proc = run_craze(craze_bin, "auth", "list")
    assert list_rows(proc.stdout)[0] == ["Fireworks", "fireworks", "stored key"]

    # An exported variable wins over the stored key, and both commands say so.
    exported = {"FIREWORKS_API_KEY": "sk-exported-not-a-secret-0002"}
    proc = run_craze(craze_bin, "auth", "list", env=exported)
    assert list_rows(proc.stdout)[0] == ["Fireworks", "fireworks", "env FIREWORKS_API_KEY"]
    proc = run_craze(craze_bin, "auth", "login", "Fireworks", stdin=CANARY + "\n", env=exported)
    assert proc.stdout.endswith(
        "FIREWORKS_API_KEY is set in this environment; craze uses it before the stored key.\n"
    ), proc.stdout

    proc = run_craze(craze_bin, "auth", "logout", "fireworks", env=exported)
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout == "Removed the stored Fireworks key.\nFireworks is still connected through FIREWORKS_API_KEY.\n"
    assert CANARY not in providers_toml().read_text(encoding="utf-8")
    proc = run_craze(craze_bin, "auth", "logout", "fireworks")
    assert (proc.returncode, proc.stdout) == (0, "No stored Fireworks key.\n")
    proc = run_craze(craze_bin, "auth", "list")
    assert list_rows(proc.stdout)[0] == ["Fireworks", "fireworks", "not connected"]


@pytest.mark.parametrize(
    ("args", "stdin", "code", "says"),
    [
        (["login", "nosuch"], CANARY + "\n", 2, "no such provider; craze has fireworks, meta, openrouter, zai-coding-plan"),
        (["login", CANARY], CANARY + "\n", 2, "no such provider"),
        (["login", "fireworks"], "", 1, "no key given; nothing was saved"),
        (["login", "fireworks"], "abc\n", 1, "was not saved: shorter than 8 bytes"),
        (["login"], CANARY + "\n", 2, "name a provider"),
        (["logout"], "", 2, "name the provider whose stored key to remove"),
    ],
    ids=["unknown provider", "key as the provider", "empty stdin", "3-byte key", "no provider", "logout of nothing"],
)
def test_failures_save_nothing(craze_bin: Path, args: list[str], stdin: str, code: int, says: str) -> None:
    proc = run_craze(craze_bin, "auth", *args, stdin=stdin)
    assert proc.returncode == code, proc.stdout + proc.stderr
    assert says in proc.stderr, proc.stderr
    assert proc.stdout == ""
    assert "abc" not in proc.stderr
    assert not (craze_home() / "native").exists()


def test_symlinked_providers_toml_is_refused(craze_bin: Path, tmp_path: Path) -> None:
    target = tmp_path / "elsewhere.toml"
    target.write_text('version = 1\n\n[providers.meta]\nname = "Meta (mine)"\n', encoding="utf-8")
    native = craze_home() / "native"
    native.mkdir(parents=True)
    providers_toml().symlink_to(target)

    proc = run_craze(craze_bin, "auth", "login", "meta", stdin=CANARY + "\n")
    assert proc.returncode == 1, proc.stdout
    assert "edit its target by hand" in proc.stderr, proc.stderr
    assert providers_toml().is_symlink()
    assert CANARY not in target.read_text(encoding="utf-8")
    # Listing still works through the link.
    proc = run_craze(craze_bin, "auth", "list")
    assert list_rows(proc.stdout)[1] == ["Meta (mine)", "meta", "not connected"]


def test_stored_key_funds_a_native_session(craze_bin: Path, tmp_path: Path, fixture_server: SSEFixture) -> None:
    """A3 end to end: a provider with no key, given one by `craze auth login`,
    funds a native session; the key reaches the provider's wire and nothing
    else -- not stdout, stderr or the session's journal, and no file but
    providers.toml. The directory says `catalog = false`, so key management
    lists its own provider alone."""
    fixture_server.set_ok(text_parts=["funded by the stored key"])
    native = craze_home() / "native"
    native.mkdir(parents=True)
    providers_toml().write_text(
        "version = 1\n\n[providers.fixture]\n"
        'driver = "openai-compat"\n'
        f'base_url = "{fixture_server.base_url}"\n'
        f'env_keys = ["{UNUSED_ENV_KEY}"]\n',
        encoding="utf-8",
    )
    (native / "models.toml").write_text(
        'version = 1\ncatalog = false\ndefault_model = "fixture-model"\n\n'
        '[models.fixture-model]\nprovider = "fixture"\nwire_model = "fixture-wire-model"\n',
        encoding="utf-8",
    )
    env = {UNUSED_ENV_KEY: ""}

    proc = run_craze(craze_bin, "auth", "list", env=env)
    assert list_rows(proc.stdout) == [["fixture", "fixture", "not connected"]], proc.stdout

    proc = run_craze(craze_bin, "auth", "login", "fixture", stdin=CANARY + "\n", env=env)
    assert proc.returncode == 0, proc.stderr
    proc = run_craze(craze_bin, "auth", "list", env=env)
    assert list_rows(proc.stdout) == [["fixture", "fixture", "stored key"]], proc.stdout

    proc = run_craze(
        craze_bin,
        "prompt",
        "--json",
        "--provider",
        "native",
        "--workspace",
        str(tmp_path),
        "hi",
        env=env,
        timeout=15,
    )
    assert proc.returncode == 0, proc.stderr
    assert "funded by the stored key" in proc.stdout
    assert any(CANARY in r.authorization for r in fixture_server.requests), fixture_server.requests

    journals = sorted((craze_home() / "journal").glob("*/*.jsonl"))
    assert journals, "the native session journaled nothing, so the check below would prove nothing"
    assert files_holding(craze_home(), CANARY.encode()) == [providers_toml()]
    assert files_holding(Path(os.environ["HOME"]), CANARY.encode()) == []
