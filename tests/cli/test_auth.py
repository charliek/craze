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
providers.toml.

The ChatGPT plan (plan 033 §3.13, at the end of this file) takes no key: its
login is Sign in with ChatGPT, driven here on a real terminal against
siwc_fixture's fake issuer -- the paste flow, a re-login through the loopback
listener, plan usage turned off, Ctrl-C, the list's three rows, logout -- with
every token value the fake issued scanned for in everything craze printed and
every file it wrote (A17). Each sign-in is also in the native directory's
sign-in log (plan 034 §3.3, A11, A12), which is scanned byte for byte for
every value of the fake's sign-ins and their percent-encodings, and whose
records are checked against what happened. A failing check never prints it either (review r2): it fails
through no_canary -- which command, which stream, where, and the text with the
key masked -- without a traceback, whose frames would show the arguments that
hold it; and the parametrized cases name the key by the placeholder KEY, so no
case's parameters hold it.
"""

from __future__ import annotations

import json
import os
import pty
import re
import select
import socket
import subprocess
import sys
import threading
import time
import urllib.request
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from urllib.parse import parse_qs, quote, quote_plus, urlsplit

import pytest

from siwc_fixture import FULL_SCOPE, IDENTITY_SCOPE, RESOURCE, FakeIssuer
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


# ---------------------------------------------------------------------------
# The ChatGPT plan (plan 033 §3.13): Sign in with ChatGPT against a fake issuer.
#
# A failure here never prints a token value either: every check of what craze
# printed goes through shows / lacks, which fail with the text masked
# (FakeIssuer.masked) and no traceback, rather than an assert whose rewriting
# would print the text as it is.

SIGNED_IN_ROW = ["ChatGPT plan", "chatgpt", "signed in as person@example.test · ChatGPT plan · renews automatically"]
PLAN_OFF_ROW = ["ChatGPT plan", "chatgpt", "plan usage disabled — run craze auth login chatgpt"]
CALLBACK = "http://127.0.0.1:1455/auth/callback"
NOTICE = (
    "You're using your ChatGPT plan.\n"
    "Eligible usage in this app uses your ChatGPT plan. Manage usage in your ChatGPT settings: "
    "https://chatgpt.com/settings/usage\n"
)
PLAN_MODELS = (
    "ChatGPT plan models: chatgpt/gpt-6.1-sol, chatgpt/gpt-6-astra, chatgpt/gpt-6-sol, chatgpt/gpt-6-luna, "
    "chatgpt/gpt-5.6-sol, chatgpt/gpt-5.6-terra, chatgpt/gpt-5.6-luna, chatgpt/gpt-5.5\n"
)
# The client_version craze pins for the model list (plan 034 D-82): read from
# the shipped catalog, so a bump there is not a second edit here.
PIN = re.search(
    r'^models_client_version = "([0-9.]+)"$',
    (Path(__file__).resolve().parents[2] / "internal/harness/modeltable/catalog.toml").read_text(),
    re.M,
).group(1)
HOUR = "An access token already issued may keep working for up to an hour, until it expires.\n"
AUTHORIZE_URL = re.compile(r"http://127\.0\.0\.1:\d+/api/accounts/authorize\?\S+")
WAITING = re.compile(r"craze is waiting for the browser to come back to (http://127\.0\.0\.1:(\d+)/auth/callback)\.")


@pytest.fixture
def issuer() -> Iterator[FakeIssuer]:
    fake = FakeIssuer().start()
    try:
        yield fake
    finally:
        fake.close()
        assert not fake.stray, f"craze asked the fake for paths it does not serve: {fake.stray}"
        assert fake.models_without_version == 0, "craze asked for the model list without a client_version"


def plan_env(fake: FakeIssuer) -> dict[str, str]:
    """The fake's overrides, and an SSH session, so craze never opens a
    browser on this machine (a desktop's DISPLAY is dropped by AuthTerminal
    too): every case here pastes, or plays the browser itself."""
    return {**fake.env(), "SSH_CONNECTION": "127.0.0.1 50000 127.0.0.1 22"}


def native_dir() -> Path:
    return craze_home() / "native"


def auth_dir() -> Path:
    return native_dir() / "auth"


def shows(fake: FakeIssuer, where: str, text: str, *needles: str) -> None:
    """Fail unless text -- what craze printed to where -- holds every
    needle, and fail if it holds a token the fake issued (once it has issued
    any: the cases that must scan call assert_no_token, which refuses to scan
    for nothing); the text is shown masked."""
    if fake.issued:
        fake.assert_no_token(where, text)
    for needle in needles:
        if needle not in text:
            pytest.fail(f"{where} lacks {fake.masked(needle)!r}; it shows (tokens masked):\n{fake.masked(text)}", pytrace=False)


def lacks(fake: FakeIssuer, where: str, text: str, *needles: str) -> None:
    for needle in needles:
        if needle in text:
            pytest.fail(f"{where} holds {needle!r}; it shows (tokens masked):\n{fake.masked(text)}", pytrace=False)


def plan_row(craze_bin: Path, fake: FakeIssuer) -> list[str]:
    """The ChatGPT plan's `craze auth list` row, the run checked for tokens."""
    proc = run_craze(craze_bin, "auth", "list", env=plan_env(fake))
    shows(fake, "craze auth list", proc.stdout + proc.stderr)
    assert proc.returncode == 0, fake.masked(proc.stderr)
    return list_rows(proc.stdout)[0]


class AuthTerminal:
    """`craze <args>` on a terminal of its own -- its controlling terminal, so
    a Ctrl-C typed here is the SIGINT a person's would be -- with stdin,
    stdout and stderr all on it, as a person running craze auth login sees
    it. The screen is read in a background thread; text() is it, its line
    endings made "\n". A failure shows it with fake's tokens masked."""

    def __init__(self, craze_bin: Path, fake: FakeIssuer, *args: str) -> None:
        self.fake = fake
        child = os.environ.copy()
        for name in ("DISPLAY", "WAYLAND_DISPLAY"):
            child.pop(name, None)
        child.update(plan_env(fake))
        child["TERM"] = "xterm-256color"
        master, slave = pty.openpty()
        # setsid and TIOCSCTTY in a fresh interpreter (never a preexec_fn in a
        # threaded process), then exec craze, keeping the pid: the terminal
        # becomes craze's controlling terminal and craze its foreground group.
        argv = [
            sys.executable,
            "-c",
            "import os,fcntl,termios,sys; os.setsid(); "
            "fcntl.ioctl(0, termios.TIOCSCTTY, 0); os.execv(sys.argv[1], sys.argv[1:])",
            str(craze_bin),
            *args,
        ]
        try:
            self.proc = subprocess.Popen(argv, stdin=slave, stdout=slave, stderr=slave, env=child, close_fds=True)
        finally:
            os.close(slave)
        self.master = master
        self._buf = bytearray()
        self._lock = threading.Lock()
        self._reader = threading.Thread(target=self._read, daemon=True)
        self._reader.start()

    def _read(self) -> None:
        while True:
            try:
                ready, _, _ = select.select([self.master], [], [], 0.05)
            except OSError:
                return
            if not ready:
                if self.proc.poll() is None:
                    continue
                # craze is gone and nothing is pending: what it wrote is all
                # here (its writes reached the master before it exited).
                return
            try:
                chunk = os.read(self.master, 65536)
            except OSError:
                return
            if not chunk:
                return
            with self._lock:
                self._buf.extend(chunk)

    def text(self) -> str:
        with self._lock:
            return self._buf.decode("utf-8", "replace").replace("\r\n", "\n")

    def wait_for(self, pattern: str | re.Pattern[str], timeout: float = 15) -> str:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            text = self.text()
            if pattern.search(text) if isinstance(pattern, re.Pattern) else pattern in text:
                return text
            if self.proc.poll() is not None:
                break
            time.sleep(0.02)
        self.kill()
        pytest.fail(f"the terminal never showed {pattern!r}; it shows (tokens masked):\n{self.fake.masked(self.text())}", pytrace=False)

    def type(self, text: str) -> None:
        os.write(self.master, text.encode())

    def finish(self, want: int, timeout: float = 20) -> str:
        """Wait for craze to exit with want; the screen, checked for tokens."""
        try:
            code = self.proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            self.kill()
            pytest.fail(f"craze never exited; the terminal shows (tokens masked):\n{self.fake.masked(self.text())}", pytrace=False)
        self._reader.join(timeout=5)
        os.close(self.master)
        screen = self.text()
        if code != want:
            pytest.fail(f"exit {code}, want {want}; the terminal shows (tokens masked):\n{self.fake.masked(screen)}", pytrace=False)
        shows(self.fake, "the terminal", screen)
        return screen

    def kill(self) -> None:
        if self.proc.poll() is None:
            self.proc.kill()
            self.proc.wait(timeout=5)


def authorization(screen: str) -> tuple[str, dict[str, str]]:
    """The authorization URL on the screen, and its query (each parameter
    once). The screen is the sign-in's start, before any token exists."""
    found = AUTHORIZE_URL.findall(screen)
    assert len(found) == 1, f"want one authorization URL on the screen:\n{screen}"
    query = parse_qs(urlsplit(found[0]).query, keep_blank_values=True)
    assert all(len(v) == 1 for v in query.values()), query
    return found[0], {k: v[0] for k, v in query.items()}


def stale(redirect: str) -> str:
    """redirect with another attempt's state: what the browser of an earlier
    sign-in would have been sent to."""
    return re.sub(r"state=[^&]+", "state=an-earlier-attempt", redirect)


def paste_sign_in(craze_bin: Path, fake: FakeIssuer) -> str:
    """A whole paste-only sign-in on a terminal; the screen."""
    term = AuthTerminal(craze_bin, fake, "auth", "login", "chatgpt", "--no-browser")
    url, _ = authorization(term.wait_for("Redirect address: "))
    term.type(fake.authorize(url) + "\n")
    return term.finish(0)


def mode(path: Path) -> int:
    return path.stat().st_mode & 0o777


def no_token_anywhere(fake: FakeIssuer, *, allowed: tuple[Path, ...] = ()) -> None:
    """No issued token value is in any file under CRAZE_HOME but allowed,
    nor anywhere under HOME."""
    for root in (craze_home(), Path(os.environ["HOME"])):
        holding = [p for p in fake.files_holding_a_token(root) if p not in allowed]
        if holding:
            pytest.fail(f"an issued token value is in {holding}", pytrace=False)


def signin_log() -> list[dict]:
    """The sign-in log's records (plan 034 §3.3), oldest first, from both its
    files: the logs directory 0700 and each file 0600. Records are value-free,
    so a failure may print them."""
    logs = native_dir() / "logs"
    assert mode(logs) == 0o700
    records = []
    for name in ("signin.log.1", "signin.log"):
        path = logs / name
        if path.exists():
            assert mode(path) == 0o600, name
            records += [json.loads(line) for line in path.read_text().splitlines()]
    return records


def assert_signin_log_value_free(fake: FakeIssuer, *extra: str) -> list[dict]:
    """Plan 034 A11, over the binary: neither sign-in log file holds any value
    of the fake's sign-ins (FakeIssuer.secret_values) or of extra -- the
    addresses and lines the test handled -- nor any of their percent-encodings,
    scanned byte for byte. A failure names the value's length, never the
    value, and has no traceback. Answers the records."""
    values = [v for v in dict.fromkeys([*fake.secret_values(), *extra]) if len(v) >= 8]
    if len(values) < 5:
        pytest.fail("the sign-in log's scan has almost nothing to look for; it would pass vacuously", pytrace=False)
    files = [p for p in (native_dir() / "logs" / "signin.log", native_dir() / "logs" / "signin.log.1") if p.exists()]
    if not files:
        pytest.fail("there is no sign-in log to scan", pytrace=False)
    for path in files:
        data = path.read_bytes()
        for value in values:
            for form in {value, quote(value, safe=""), quote(value), quote_plus(value)}:
                if form.encode() in data:
                    pytest.fail(f"{path.name} holds a fixture value ({len(value)} bytes) or one of its percent-encodings",
                                pytrace=False)
    return signin_log()


def test_chatgpt_sign_in_by_paste_list_and_logout(craze_bin: Path, issuer: FakeIssuer) -> None:
    """A17, A19: the paste-only sign-in a person on another machine uses
    (`--no-browser`, P25) from first registration to logout. The URL is a
    first registration's, with PKCE, state and nonce, and no hint; a stale
    redirect is refused, never shown -- the prompt does not echo (review r14
    1) -- and the right one is confirmed by its origin and path alone, never
    its query, and signs in -- the exchange proving the PKCE verifier,
    the redirect address and the issued client id, the id_token validated
    over the key set -- and craze prints the account, the one-time notice and
    the plan's models in priority order, fetched there and then (the hidden
    one not among them). The files are 0600 in a 0700 directory, the list
    row says who is signed in, and logout revokes the refresh token and
    deletes the tokens, keeping the registration. No token value is printed,
    or written anywhere but the token file, which logout removes. The sign-in
    log has the attempt, value-free (plan 034 A11): its begin, the stale
    redirect refused as an earlier attempt's, the redirect, the sign-in and
    the model fetch."""
    env = plan_env(issuer)
    proc = run_craze(craze_bin, "auth", "list", env=env)
    assert list_rows(proc.stdout)[0] == PLAN_ROW

    term = AuthTerminal(craze_bin, issuer, "auth", "login", "chatgpt", "--no-browser")
    screen = term.wait_for("Redirect address: ")
    url, q = authorization(screen)
    assert q.pop("state") and q.pop("nonce")
    challenge = q.pop("code_challenge")
    host_id = q.pop("ext_agent_host_id")
    assert re.fullmatch(r"[A-Za-z0-9_-]{43}", challenge), challenge
    assert re.fullmatch(r"urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}", host_id), host_id
    assert q == {
        "client_id": "dynamic_agent_client",
        "agent_name_hint": "craze",
        "response_type": "code",
        "redirect_uri": CALLBACK,
        "scope": FULL_SCOPE,
        "resource": RESOURCE,
        "code_challenge_method": "S256",
    }
    assert "After you approve, the browser goes to an address starting with " + CALLBACK in screen, screen
    assert "craze opened it in your browser" not in screen, screen

    redirect = issuer.authorize(url)
    term.type(stale(redirect) + "\n")
    screen = term.wait_for("That is the redirect of an earlier sign-in attempt.")
    assert "Redirect address: \nThat is the redirect of an earlier sign-in attempt. Use the address shown above." in screen, screen
    lacks(issuer, "the terminal", screen, "an-earlier-attempt")
    term.type(redirect + "\n")
    screen = term.finish(0)
    issuer.assert_no_token("the terminal", screen)
    shows(issuer, "the terminal", screen,
          "Redirect address: \nReceived the redirect to " + CALLBACK + "; signing in.\n"
          "Signed in to ChatGPT as person@example.test.\n" + NOTICE + PLAN_MODELS)
    lacks(issuer, "the terminal", screen, "code=" + parse_qs(urlsplit(redirect).query)["code"][0])

    assert len(issuer.exchanges) == 1
    exchange = issuer.exchanges[0]
    assert (exchange.client_id, exchange.redirect_uri, exchange.pkce_ok, exchange.granted) == (
        issuer.issue_client, CALLBACK, True, True)
    assert issuer.models_gets == 1
    assert issuer.model_versions == [PIN]
    records = assert_signin_log_value_free(issuer, url, stale(redirect), redirect)
    assert [r["event"] for r in records] == ["begin", "paste_refused", "redirect_received", "signed_in", "models_fetched"], records
    assert {r["surface"] for r in records} == {"cli"} and len({r["attempt"] for r in records}) == 1, records
    assert (records[0]["mode"], records[0]["reason"], records[0]["port"], records[0]["registration"]) == (
        "paste_only", "requested", 1455, "new"), records
    assert (records[1]["refusal"], records[1]["part"]) == ("mismatch", "state"), records
    assert (records[2]["via"], records[3]["usage"], records[3]["registration"]) == ("paste", "plan", "new"), records
    assert (records[4]["models"], records[4]["client_version"]) == (8, PIN), records
    cache = (native_dir() / "chatgpt-models.json").read_text()
    assert f'"client_version": "{PIN}"' in cache and '"minimal_client_version": "0.153.0"' in cache, cache

    assert mode(auth_dir()) == 0o700
    for name in ("host-id", "chatgpt-client.json", "chatgpt.json"):
        assert mode(auth_dir() / name) == 0o600, name
    assert mode(native_dir() / "chatgpt-models.json") == 0o600
    assert (auth_dir() / "host-id").read_text().strip() == host_id
    client = (auth_dir() / "chatgpt-client.json").read_text()
    assert '"client_id": "' + issuer.issue_client + '"' in client and '"notice_shown": true' in client, client
    no_token_anywhere(issuer, allowed=(auth_dir() / "chatgpt.json",))
    assert plan_row(craze_bin, issuer) == SIGNED_IN_ROW

    proc = run_craze(craze_bin, "auth", "logout", "chatgpt", env=env)
    shows(issuer, "craze auth logout chatgpt", proc.stdout + proc.stderr)
    assert (proc.returncode, proc.stderr) == (0, "")
    assert proc.stdout == "Signed out of ChatGPT: the sign-in is revoked, and its tokens are deleted from this machine.\n" + HOUR
    hints = [(r.get("token_type_hint"), r.get("client_id")) for r in issuer.revokes]
    assert hints == [("refresh_token", issuer.issue_client)]
    if issuer.revoked_issued_refresh_tokens() != 1:
        pytest.fail("logout did not revoke the refresh token the sign-in was issued", pytrace=False)
    assert not (auth_dir() / "chatgpt.json").exists()
    assert (auth_dir() / "chatgpt-client.json").exists() and (auth_dir() / "host-id").exists()
    no_token_anywhere(issuer)

    assert plan_row(craze_bin, issuer) == PLAN_ROW
    proc = run_craze(craze_bin, "auth", "logout", "chatgpt", env=env)
    assert (proc.returncode, proc.stdout) == (0, "Not signed in to ChatGPT.\n")


def test_chatgpt_sign_in_hides_a_pasted_key(craze_bin: Path, issuer: FakeIssuer) -> None:
    """Review r14 1: a key pasted at the sign-in's prompt on a real terminal --
    where a key prompt would be, out of habit -- is on no part of the
    terminal: the prompt does not echo, and the refusal, which says the plan
    takes no key, never quotes it. It is stored nowhere, and the redirect
    pasted after it signs in. The whole screen is scanned. The control is the
    authorization URL's state on that screen: the scan reads what craze
    printed. (The control with the echo left on, where the key shows, is the
    Go pty test's: TestAuthLoginChatGPTHidesAPastedKey.)"""
    term = AuthTerminal(craze_bin, issuer, "auth", "login", "chatgpt", "--no-browser")
    url, q = authorization(term.wait_for("Redirect address: "))
    term.type(CANARY + "\n")
    term.wait_for("never by an API key.")
    term.type(issuer.authorize(url) + "\n")
    screen = term.finish(0)
    assert q["state"] in screen, "control: the authorization URL's state is not on the screen"
    no_canary("craze auth login chatgpt", "the terminal", screen)
    shows(issuer, "the terminal", screen,
          "Redirect address: \nThat is not an address: the ChatGPT plan is funded by signing in, never by an API key. "
          "Paste the whole address the browser was sent to; it starts with " + CALLBACK + ".\n"
          "Redirect address: \nReceived the redirect to " + CALLBACK + "; signing in.\n"
          "Signed in to ChatGPT as person@example.test.\n")
    assert not providers_toml().exists()
    holding = files_holding(craze_home(), CANARY.encode())
    if holding:
        pytest.fail(f"the pasted key is in {holding}", pytrace=False)
    assert plan_row(craze_bin, issuer) == SIGNED_IN_ROW


@contextmanager
def port_1455_taken(deadline: float = 60.0) -> Iterator[None]:
    """Port 1455 on 127.0.0.1 held by this test for the whole block, so
    craze's listener must take another: a re-login may, once registered (P21).
    Something else may hold it when this starts (several suites run at once on
    one box): the bind is retried until it succeeds, and the test fails, naming
    the port, if it never does -- never carried on without holding it, since
    craze would then take 1455 itself."""
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    end = time.monotonic() + deadline
    try:
        while True:
            try:
                s.bind(("127.0.0.1", 1455))
                s.listen(1)
                break
            except OSError as e:
                if time.monotonic() >= end:
                    pytest.fail(f"127.0.0.1:1455 stayed held by another process for {deadline:.0f}s: {e}", pytrace=False)
                time.sleep(0.2)
        yield
    finally:
        s.close()


def test_chatgpt_relogin_through_the_listener(craze_bin: Path, issuer: FakeIssuer) -> None:
    """A17: a re-login reuses the issued client id with the account's email as
    login_hint -- never an id_token_hint, and no registration's parameters --
    and, with 1455 taken, listens on another port; the browser's redirect to
    that listener signs in, its page sent no-store and no-referrer. The
    one-time notice is not shown again, and the listener is closed after."""
    first = paste_sign_in(craze_bin, issuer)
    shows(issuer, "the first sign-in", first, NOTICE)
    _, q1 = authorization(first)
    assert run_craze(craze_bin, "auth", "logout", "chatgpt", env=plan_env(issuer)).returncode == 0

    with port_1455_taken():
        term = AuthTerminal(craze_bin, issuer, "auth", "login", "chatgpt")
        screen = term.wait_for(WAITING)
    callback, port = WAITING.search(screen).group(1, 2)
    assert port != "1455", screen
    url, q = authorization(screen)
    assert q["client_id"] == issuer.issue_client and q["login_hint"] == "person@example.test", q
    assert q["redirect_uri"] == callback and q["ext_agent_host_id"] == q1["ext_agent_host_id"], q
    for absent in ("agent_name_hint", "id_token_hint", "prompt"):
        assert absent not in q, absent

    with urllib.request.urlopen(issuer.authorize(url), timeout=10) as page:  # the browser
        assert page.status == 200
        assert page.read().decode().startswith("craze received the sign-in.")
        assert page.headers["Cache-Control"] == "no-store" and page.headers["Referrer-Policy"] == "no-referrer"
        assert page.headers["Content-Security-Policy"].startswith("default-src 'none'")
    screen = term.finish(0)
    issuer.assert_no_token("the terminal", screen)
    shows(issuer, "the terminal", screen, "Signed in to ChatGPT as person@example.test.\n" + PLAN_MODELS)
    lacks(issuer, "the terminal", screen, "You're using your ChatGPT plan.")
    with pytest.raises(OSError):
        socket.create_connection(("127.0.0.1", int(port)), timeout=2).close()
    assert plan_row(craze_bin, issuer) == SIGNED_IN_ROW
    # The re-login in the sign-in log (plan 034 Q8): listening on another
    # port because 1455 was busy, the redirect through the listener.
    records = assert_signin_log_value_free(issuer, url)
    relogin = [r for r in records if r["attempt"] == records[-1]["attempt"]]
    assert [r["event"] for r in relogin] == ["begin", "redirect_received", "signed_in", "models_fetched"], relogin
    assert (relogin[0]["mode"], relogin[0]["reason"], relogin[0]["port"], relogin[0]["registration"]) == (
        "listening", "port_busy", int(port), "reused"), relogin
    assert (relogin[1]["via"], relogin[2]["registration"]) == ("listener", "reused"), relogin


def test_chatgpt_plan_usage_off_then_ctrl_c(craze_bin: Path, issuer: FakeIssuer) -> None:
    """A17b: an account that signs in without granting plan usage is told
    so, and how to turn it on; no tokens are kept, no model list is fetched,
    and the list row says so. The next sign-in asks ChatGPT for consent again
    (prompt=consent); a Ctrl-C there cancels it -- exit 130 -- and changes
    nothing."""
    issuer.scope = IDENTITY_SCOPE
    screen = paste_sign_in(craze_bin, issuer)
    shows(issuer, "the terminal", screen,
          "Signed in to ChatGPT as person@example.test, but ChatGPT plan usage is off: the account did not allow craze "
          "to use its ChatGPT plan, so the plan's models cannot be used.\n"
          "To turn it on, run craze auth login chatgpt again and allow ChatGPT plan usage when ChatGPT asks.\n")
    lacks(issuer, "the terminal", screen, "You're using your ChatGPT plan.", "ChatGPT plan models:")
    assert issuer.models_gets == 0
    assert not (auth_dir() / "chatgpt.json").exists() and not (native_dir() / "chatgpt-models.json").exists()
    no_token_anywhere(issuer)
    assert plan_row(craze_bin, issuer) == PLAN_OFF_ROW

    term = AuthTerminal(craze_bin, issuer, "auth", "login", "chatgpt", "--no-browser")
    _, q = authorization(term.wait_for("Redirect address: "))
    assert (q["client_id"], q["login_hint"], q["prompt"]) == (issuer.issue_client, "person@example.test", "consent")
    term.type("\x03")
    screen = term.finish(130)
    shows(issuer, "the terminal", screen, "craze auth login: the sign-in was cancelled; nothing was changed\n")
    assert plan_row(craze_bin, issuer) == PLAN_OFF_ROW
    assert len(issuer.exchanges) == 1
    # Both attempts in the sign-in log: the first signed in with plan usage
    # off, the second cancelled by the signal.
    records = assert_signin_log_value_free(issuer)
    first = [r for r in records if r["attempt"] == records[0]["attempt"]]
    assert (first[-1]["event"], first[-1]["usage"]) == ("signed_in", "off"), records
    assert (records[-1]["event"], records[-1]["reason"]) == ("cancelled", "signal"), records


def test_chatgpt_takes_no_key(craze_bin: Path, issuer: FakeIssuer) -> None:
    """X134: a key piped into `craze auth login chatgpt` is never asked for,
    stored or repeated -- the line is refused as not an address, saying the
    plan takes no key -- and the paste-only sign-in ends when stdin does,
    exit 1, nothing exchanged. (run_craze fails on the key in any output.)"""
    proc = run_craze(craze_bin, "auth", "login", "chatgpt", "--no-browser", stdin=CANARY + "\n", env=plan_env(issuer))
    assert proc.returncode == 1, proc.stdout + proc.stderr
    assert proc.stdout == ""
    assert "That is not an address: the ChatGPT plan is funded by signing in, never by an API key." in proc.stderr
    assert proc.stderr.endswith("craze auth login: stdin ended before the redirect address was pasted; nothing was changed\n")
    assert "API key: " not in proc.stderr
    assert not providers_toml().exists() and not (auth_dir() / "chatgpt.json").exists()
    assert issuer.exchanges == []
    # The sign-in log has the key refused as no address, and the attempt
    # cancelled at stdin's end; the key itself is nowhere in it.
    no_canary("craze auth login chatgpt", "the sign-in log", (native_dir() / "logs" / "signin.log").read_text())
    records = signin_log()
    assert [r["event"] for r in records] == ["begin", "paste_refused", "cancelled"], records
    assert (records[1]["refusal"], records[2]["reason"]) == ("not_address", "no_input"), records


@pytest.mark.parametrize("refusal", ["declined", "tampered id_token", "wrong verifier"])
def test_chatgpt_sign_in_refusals(craze_bin: Path, issuer: FakeIssuer, refusal: str) -> None:
    """Negative controls for the sign-in's checks, end to end: a person who
    declines in the browser, an id_token whose signature the key set does not
    verify, and an exchange the issuer refuses (a code bound to another PKCE
    challenge) each end the sign-in with exit 1 and leave no tokens and a
    "not signed in" row."""
    issuer.tamper_id_token = refusal == "tampered id_token"
    issuer.wrong_challenge = refusal == "wrong verifier"
    term = AuthTerminal(craze_bin, issuer, "auth", "login", "chatgpt", "--no-browser")
    url, _ = authorization(term.wait_for("Redirect address: "))
    term.type(issuer.authorize(url, decline=refusal == "declined") + "\n")
    want = {
        "declined": "craze auth login: the sign-in was declined in the browser; nothing was changed\n",
        "tampered id_token": "craze auth login: the sign-in's id_token is not valid",
        "wrong verifier": "craze auth login: exchange refused (HTTP 400, invalid_grant)\n",
    }[refusal]
    if refusal == "tampered id_token":
        # The exchange issued tokens before craze refused the id_token:
        # none of them may be kept, or shown.
        screen = term.finish(1)
        issuer.assert_no_token("the terminal", screen)
        shows(issuer, "the terminal", screen, want)
        no_token_anywhere(issuer)
    else:
        assert want in term.finish(1)
        if issuer.issued:
            pytest.fail(f"a refused sign-in was issued {len(issuer.issued)} tokens", pytrace=False)
    assert not (auth_dir() / "chatgpt.json").exists()
    proc = run_craze(craze_bin, "auth", "list", env=plan_env(issuer))
    assert list_rows(proc.stdout)[0] == PLAN_ROW
    # The outcome in the sign-in log, value-free (plan 034 A10, A11).
    records = assert_signin_log_value_free(issuer, url)
    outcome = {
        "declined": {"event": "declined", "step": "authorize", "code": "access_denied"},
        "tampered id_token": {"event": "failed", "step": "id_token", "class": "id_token", "check": "signature"},
        "wrong verifier": {"event": "failed", "step": "exchange", "status": 400, "code": "invalid_grant", "class": "refused"},
    }[refusal]
    assert {k: records[-1].get(k) for k in outcome} == outcome, records


def test_chatgpt_refused_sign_in_log_never_fails_the_sign_in(craze_bin: Path, issuer: FakeIssuer) -> None:
    """Plan 034 A12, over the binary: a sign-in log craze must refuse -- its
    directory writable by the group -- is one note on the terminal, naming
    why, and the sign-in finishes as ever; nothing is written there."""
    logs = native_dir() / "logs"
    logs.mkdir(parents=True, mode=0o700)
    logs.chmod(0o770)
    screen = paste_sign_in(craze_bin, issuer)
    shows(issuer, "the terminal", screen, "Signed in to ChatGPT as person@example.test.\n")
    if screen.count("note: the sign-in log is off: ") != 1 or "is writable by other users" not in screen:
        pytest.fail(f"want one note saying the log's directory is writable by others:\n{issuer.masked(screen)}", pytrace=False)
    assert list(logs.iterdir()) == []
    assert plan_row(craze_bin, issuer) == SIGNED_IN_ROW


def test_chatgpt_logout_unconfirmed(craze_bin: Path, issuer: FakeIssuer) -> None:
    """A revocation the issuer does not confirm still signs out: the tokens
    are deleted, and craze says remote revocation was not confirmed."""
    paste_sign_in(craze_bin, issuer)
    issuer.revoke_status = 503
    proc = run_craze(craze_bin, "auth", "logout", "chatgpt", env=plan_env(issuer))
    issuer.assert_no_token("craze auth logout chatgpt", proc.stdout + proc.stderr)
    assert proc.returncode == 0, proc.stderr
    assert proc.stdout == (
        "Signed out of ChatGPT: its tokens are deleted from this machine, but remote revocation not confirmed. "
        "You can disconnect craze in ChatGPT's settings, under Apps.\n" + HOUR
    )
    assert len(issuer.revokes) == 1
    assert not (auth_dir() / "chatgpt.json").exists()
    no_token_anywhere(issuer)
    assert plan_row(craze_bin, issuer) == PLAN_ROW
