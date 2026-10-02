"""Plan 031 C4: `craze auth login|logout|list` from the outside (§3.7).

The native provider's keys are stored as the inline api_key of their
providers.toml entry, in the craze directory's native/ (0600), by the real
binary against a temp CRAZE_HOME (isolate_run_env: every variable the shipped
catalog names is unset, so its four key providers start unconnected, and
nobody is signed in to the fifth, the ChatGPT plan). stdin is a
pipe here, never a terminal, so the key is always stdin's first line; the
no-echo prompt on a terminal is the Go pty test's
(internal/cli/auth_test.go).

Canary checks (A7): the key never appears in stdout or stderr, in any error,
in the journal of a native session it funds, or in any file but
providers.toml. A failing check never prints it either (review r2): it fails
through no_canary -- which command, which stream, where, and the text with the
key masked -- without a traceback, whose frames would show the arguments that
hold it; and the parametrized cases name the key by the placeholder KEY, so no
case's parameters hold it.
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

# The ChatGPT plan's row, first by name: funded by its sign-in, never a key
# (plan 033 §3.11), and nobody is signed in here.
PLAN_ROW = ["ChatGPT plan", "chatgpt", "not signed in"]


@pytest.fixture
def fixture_server():
    server = SSEFixture().start()
    try:
        yield server
    finally:
        server.close()


# KEY stands for the key in a case's parameters (with_key puts it in).
KEY = "<CANARY>"


def with_key(args: list[str]) -> list[str]:
    return [a.replace(KEY, CANARY) for a in args]


def masked(text: str) -> str:
    """text with the key replaced by KEY: what a failure may print."""
    return text.replace(CANARY, KEY)


def no_canary(what: str, where: str, text: str) -> None:
    """Fail when text holds the key, saying which command (what) and which
    stream or file (where) and where in it, the text shown masked -- and with
    no traceback, whose frames would print the arguments holding the key."""
    at = text.find(CANARY)
    if at >= 0:
        pytest.fail(
            f"{masked(what)}: the key leaked into {where} at index {at}; with it masked it reads:\n{masked(text)}",
            pytrace=False,
        )


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
    what = " ".join(["craze", *args])
    no_canary(what, "stdout", proc.stdout)
    no_canary(what, "stderr", proc.stderr)
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
    assert list_rows(proc.stdout) == [PLAN_ROW] + [row + ["not connected"] for row in SHIPPED_ROWS]

    proc = run_craze(craze_bin, "auth", "login", "fireworks", stdin=CANARY + "\n")
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout == f"Saved the Fireworks key in {providers_toml()}.\n"
    assert proc.stderr == ""
    assert providers_toml().stat().st_mode & 0o777 == 0o600
    assert (craze_home() / "native").stat().st_mode & 0o777 == 0o700
    if CANARY not in providers_toml().read_text(encoding="utf-8"):
        pytest.fail("craze auth login fireworks: the key is not in providers.toml", pytrace=False)
    assert not (craze_home() / "native" / "models.toml").exists()

    proc = run_craze(craze_bin, "auth", "list")
    assert list_rows(proc.stdout)[1] == ["Fireworks", "fireworks", "stored key"]

    # An exported variable wins over the stored key, and both commands say so.
    exported = {"FIREWORKS_API_KEY": "sk-exported-not-a-secret-0002"}
    proc = run_craze(craze_bin, "auth", "list", env=exported)
    assert list_rows(proc.stdout)[1] == ["Fireworks", "fireworks", "env FIREWORKS_API_KEY"]
    proc = run_craze(craze_bin, "auth", "login", "Fireworks", stdin=CANARY + "\n", env=exported)
    assert proc.stdout.endswith(
        "FIREWORKS_API_KEY is set in this environment; craze uses it before the stored key.\n"
    ), proc.stdout

    proc = run_craze(craze_bin, "auth", "logout", "fireworks", env=exported)
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout == "Removed the stored Fireworks key.\nFireworks is still connected through FIREWORKS_API_KEY.\n"
    no_canary("craze auth logout fireworks", str(providers_toml()), providers_toml().read_text(encoding="utf-8"))
    proc = run_craze(craze_bin, "auth", "logout", "fireworks")
    assert (proc.returncode, proc.stdout) == (0, "No stored Fireworks key.\n")
    proc = run_craze(craze_bin, "auth", "list")
    assert list_rows(proc.stdout)[1] == ["Fireworks", "fireworks", "not connected"]


@pytest.mark.parametrize(
    ("args", "stdin", "code", "says"),
    [
        (["login", "nosuch"], KEY + "\n", 2, "no such provider; craze has chatgpt, fireworks, meta, openrouter, zai-coding-plan"),
        (["login", KEY], KEY + "\n", 2, "no such provider"),
        (["login", "fireworks"], "", 1, "no key given; nothing was saved"),
        (["login", "fireworks"], "abc\n", 1, "was not saved: shorter than 8 bytes"),
        (["login"], KEY + "\n", 2, "name a provider"),
        (["logout"], "", 2, "name the provider whose stored key to remove"),
    ],
    ids=["unknown provider", "key as the provider", "empty stdin", "3-byte key", "no provider", "logout of nothing"],
)
def test_failures_save_nothing(craze_bin: Path, args: list[str], stdin: str, code: int, says: str) -> None:
    proc = run_craze(craze_bin, "auth", *with_key(args), stdin=stdin.replace(KEY, CANARY))
    assert proc.returncode == code, proc.stdout + proc.stderr
    assert says in proc.stderr, proc.stderr
    assert proc.stdout == ""
    assert "abc" not in proc.stderr
    assert not (craze_home() / "native").exists()


@pytest.mark.parametrize(
    ("args", "says"),
    [
        (["list", KEY], "craze auth list: takes no arguments"),
        (["login", "fireworks", KEY], "craze auth login: takes one argument at most, the provider"),
        (["logout", "fireworks", KEY], "craze auth logout: takes one argument, the provider"),
        (["login", f"--{KEY}", "fireworks"], "craze auth login: unknown or malformed flag; see craze auth login --help"),
        (["list", f"-{KEY}"], "craze auth list: unknown or malformed flag; see craze auth list --help"),
        (["logout", f"--help={KEY}", "fireworks"], "craze auth logout: unknown or malformed flag; see craze auth logout --help"),
    ],
    ids=["list with a key", "login with a key too", "logout with a key too", "key as a flag", "key as short flags", "key as a flag value"],
)
def test_surplus_arguments_and_bad_flags_are_not_quoted(craze_bin: Path, args: list[str], says: str) -> None:
    """X29, review r2: an argument too many, or a flag the command does not
    take, is exit 2 with a line that says what the command takes -- never
    what it was given, which cobra's own errors quote. Nothing is saved."""
    proc = run_craze(craze_bin, "auth", *with_key(args), stdin=CANARY + "\n")
    assert (proc.returncode, proc.stderr) == (2, says + "\n")
    assert proc.stdout == ""
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
    no_canary("craze auth login meta", "the symlink's target", target.read_text(encoding="utf-8"))
    # Listing still works through the link.
    proc = run_craze(craze_bin, "auth", "list")
    assert list_rows(proc.stdout)[2] == ["Meta (mine)", "meta", "not connected"]


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
    if not any(CANARY in r.authorization for r in fixture_server.requests):
        pytest.fail(f"none of the {len(fixture_server.requests)} requests carried the stored key", pytrace=False)

    journals = sorted((craze_home() / "journal").glob("*/*.jsonl"))
    assert journals, "the native session journaled nothing, so the check below would prove nothing"
    # Compared outside an assert: pytest's rewriting would print the key the
    # call was given.
    holding = files_holding(craze_home(), CANARY.encode())
    if holding != [providers_toml()]:
        pytest.fail(f"the key is in {holding}; want it in {providers_toml()} alone", pytrace=False)
    holding = files_holding(Path(os.environ["HOME"]), CANARY.encode())
    if holding:
        pytest.fail(f"the key is in {holding} under HOME", pytrace=False)
