"""A loopback fake of OpenAI's Sign in with ChatGPT for `craze auth` (plan 033 C15).

craze's sign-in (internal/chatgptauth) talks to fixed HTTPS origins in
production, auth.openai.com and api.openai.com; its test overrides,
CRAZE_TEST_CHATGPT_ISSUER and CRAZE_TEST_CHATGPT_API, are honoured only for a
loopback http URL (plan 033 §3.10), which is what FakeIssuer serves: the
OpenID configuration, a key set, the token endpoint (authorization code and
refresh grants), the revocation endpoint, and the plan's model list -- on
127.0.0.1 at an OS-assigned port, in a background thread, stdlib only.

It does the real work an issuer does, so the binary's own checks run against
it rather than being skipped: an authorization code is bound to the PKCE
challenge, the redirect address and the client id it was issued for, and is
exchanged only for the S256 verifier that hashes to its challenge; the
id_token is a JWT signed RS256 with a 2048-bit key published in the key set,
carrying the issuer, the issued client id as audience, the attempt's nonce,
and the account's subject and email. The RSA arithmetic is Python's own
(Miller-Rabin primes, PKCS#1 v1.5), one key per test session: the suite has no
crypto dependency, and craze refuses a key under 2048 bits.

FakeIssuer.authorize stands in for the browser and the person: it reads the
authorization URL craze printed as the server would, issues a code, and
answers the address the browser would be sent back to -- which the test then
pastes into craze, or GETs from craze's loopback listener.

Every token value it issues is a dummy, recorded in FakeIssuer.issued so a
test can prove none of them leaked; a failure never prints one
(assert_no_token).
"""

from __future__ import annotations

import base64
import functools
import hashlib
import json
import math
import secrets
import threading
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlencode, urlsplit

import pytest

ISSUER_ENV = "CRAZE_TEST_CHATGPT_ISSUER"
API_ENV = "CRAZE_TEST_CHATGPT_API"

# The authorization request's constants, as craze sends them (the sign-in
# docs' table): the resource is an identifier the server checks, the
# production API's even against a fake.
RESOURCE = "https://api.openai.com/v1"
FULL_SCOPE = "openid profile email offline_access resource.invoke chatgpt.tokens.use.direct"
IDENTITY_SCOPE = "openid profile email offline_access"
DYNAMIC_CLIENT = "dynamic_agent_client"
# The fake's error_description: text from the server, which craze never
# repeats -- the sign-in log's scan looks for it (plan 034 A11), so it is a
# value no record could hold by chance.
ERROR_DESCRIPTION = "fixture-error-description-3b9e7c"

AUTHORIZE_PATH = "/api/accounts/authorize"
TOKEN_PATH = "/api/accounts/oauth/token"
REVOKE_PATH = "/api/accounts/oauth/revoke"
DISCOVERY_PATH = "/.well-known/openid-configuration"
JWKS_PATH = "/jwks"
MODELS_PATH = "/v1/models"


def b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode("ascii")


def _int_bytes(n: int) -> bytes:
    return n.to_bytes((n.bit_length() + 7) // 8, "big")


# ---------------------------------------------------------------------------
# RSA, enough to sign an id_token RS256.

_SMALL_PRIMES = [p for p in range(3, 2000, 2) if all(p % d for d in range(3, math.isqrt(p) + 1, 2))]


def _probable_prime(n: int, rounds: int = 40) -> bool:
    """Miller-Rabin, after trial division by the small odd primes."""
    if n < 2 or n % 2 == 0:
        return n == 2
    for p in _SMALL_PRIMES:
        if n % p == 0:
            return n == p
    d, s = n - 1, 0
    while d % 2 == 0:
        d //= 2
        s += 1
    for _ in range(rounds):
        x = pow(secrets.randbelow(n - 3) + 2, d, n)
        if x in (1, n - 1):
            continue
        for _ in range(s - 1):
            x = pow(x, 2, n)
            if x == n - 1:
                break
        else:
            return False
    return True


def _prime(bits: int) -> int:
    """A random prime of exactly bits bits, its top two bits set so that a
    product of two is exactly twice as long."""
    while True:
        candidate = secrets.randbits(bits) | (0b11 << (bits - 2)) | 1
        if _probable_prime(candidate):
            return candidate


@dataclass(frozen=True)
class RSAKey:
    n: int
    e: int
    d: int

    @property
    def size(self) -> int:
        return (self.n.bit_length() + 7) // 8


@functools.cache
def rsa_key() -> RSAKey:
    """The session's signing key: 2048 bits, e = 65537."""
    e = 65537
    while True:
        p, q = _prime(1024), _prime(1024)
        lam = math.lcm(p - 1, q - 1)
        if p == q or math.gcd(e, lam) != 1 or (p * q).bit_length() != 2048:
            continue
        return RSAKey(n=p * q, e=e, d=pow(e, -1, lam))


# DER of the SHA-256 DigestInfo prefix (RFC 8017 §9.2, note 1).
_SHA256_DIGEST_INFO = bytes.fromhex("3031300d060960864801650304020105000420")


def rs256(key: RSAKey, signing_input: bytes) -> bytes:
    """An RSASSA-PKCS1-v1_5 signature with SHA-256 (JWS RS256)."""
    t = _SHA256_DIGEST_INFO + hashlib.sha256(signing_input).digest()
    em = b"\x00\x01" + b"\xff" * (key.size - len(t) - 3) + b"\x00" + t
    return pow(int.from_bytes(em, "big"), key.d, key.n).to_bytes(key.size, "big")


def sign_jwt(key: RSAKey, kid: str, claims: dict) -> str:
    header = b64url(json.dumps({"alg": "RS256", "kid": kid, "typ": "JWT"}).encode())
    payload = b64url(json.dumps(claims).encode())
    signing_input = f"{header}.{payload}".encode()
    return f"{header}.{payload}.{b64url(rs256(key, signing_input))}"


# ---------------------------------------------------------------------------
# The issuer.


def default_models() -> list[dict]:
    """The plan's list as the live server answers a client at or above its
    newest minimum (plan 034 §3.1): eight shown models in priority order
    gpt-6.1-sol, gpt-6-astra, gpt-6-sol, gpt-6-luna, gpt-5.6-sol,
    gpt-5.6-terra, gpt-5.6-luna, gpt-5.5, each with its
    minimal_client_version, and two hidden, which craze must not offer. The
    fake gates the reply on the client_version of the request (gated_models)."""

    def levels(*efforts: str) -> list[dict]:
        return [{"effort": e, "description": e} for e in efforts]

    def shown(slug: str, name: str, priority: int, minimum: str, modalities: list[str], efforts: list[str],
              default: str, **extra: object) -> dict:
        return {"slug": slug, "display_name": name, "visibility": "list", "priority": priority,
                "context_window": 272000, "minimal_client_version": minimum, "input_modalities": modalities,
                "supported_reasoning_levels": levels(*efforts), "default_reasoning_level": default, **extra}

    text, image = ["text"], ["text", "image"]
    return [
        shown("gpt-5.6-luna", "GPT-5.6-Luna", 9, "0.144.0", text, ["low", "medium"], "medium",
              supports_parallel_tool_calls=False),
        shown("gpt-6-astra", "GPT-6-Astra", 2, "0.153.0", image, ["low", "medium", "high", "xhigh", "max", "ultra"],
              "medium", supports_parallel_tool_calls=True, base_instructions="a codex prompt craze does not keep"),
        {"slug": "gpt-reserve", "display_name": "GPT-Reserve", "visibility": "hide", "priority": 1,
         "context_window": 272000, "minimal_client_version": "0.144.0"},
        shown("gpt-5.6-sol", "GPT-5.6-Sol", 5, "0.144.0", image, ["low", "medium", "high"], "low"),
        shown("gpt-6.1-sol", "GPT-6.1-Sol", 1, "0.153.0", image, ["low", "medium", "high", "xhigh"], "medium",
              supports_parallel_tool_calls=True),
        shown("gpt-6-sol", "GPT-6-Sol", 3, "0.155.0", image, ["low", "medium", "high"], "medium"),
        shown("gpt-6-luna", "GPT-6-Luna", 4, "0.155.0", text, ["low", "medium"], "medium"),
        shown("gpt-5.6-terra", "GPT-5.6-Terra", 6, "0.144.0", text, ["low", "medium", "high"], "medium"),
        shown("gpt-5.5", "GPT-5.5", 10, "0.124.0", text, ["low", "medium", "high"], "medium"),
        {"slug": "codex-auto-review", "display_name": "Codex Auto Review", "visibility": "hide", "priority": 43,
         "context_window": 272000, "minimal_client_version": "0.98.0"},
    ]


def _version(v: str) -> tuple[int, ...]:
    parts = [int(p) for p in v.split(".")]
    while parts and parts[-1] == 0:
        parts.pop()  # 0.160 == 0.160.0
    return tuple(parts)


def gated_models(models: list[dict], client_version: str) -> list[dict]:
    """The reply for a client at client_version: the entries whose
    minimal_client_version it meets (0.0.1 meets none of the live list's)."""
    return [m for m in models if _version(client_version) >= _version(m.get("minimal_client_version", "0"))]


@dataclass
class Exchange:
    """One authorization-code exchange as the fake saw it."""

    client_id: str
    redirect_uri: str
    pkce_ok: bool
    granted: bool


# repr=False: pytest's assertion introspection prints the repr of an object
# an assertion reads an attribute of, and the generated one would print every
# issued token (__repr__ below names the fake alone).
@dataclass(repr=False)
class FakeIssuer:
    host: str = "127.0.0.1"
    kid: str = "fake-kid-1"

    # Knobs: set before the step they shape.
    issue_client: str = "oaiapp_fakeclient000000000001"
    subject: str = "user-fake-subject-0001"
    email: str = "person@example.test"
    scope: str = FULL_SCOPE  # what a sign-in grants
    tamper_id_token: bool = False  # break the id_token's signature
    wrong_challenge: bool = False  # refuse every verifier, as a code bound to another challenge
    revoke_status: int = 200
    models: list[dict] = field(default_factory=default_models)

    # Records.
    authorizations: list[dict[str, str]] = field(default_factory=list)
    exchanges: list[Exchange] = field(default_factory=list)
    revokes: list[dict[str, str]] = field(default_factory=list)
    models_gets: int = 0
    model_versions: list[str] = field(default_factory=list)  # each list request's client_version
    models_without_version: int = 0  # list requests with no (or several) client_version: answered 400
    issued: list[str] = field(default_factory=list)
    stray: list[str] = field(default_factory=list)  # requests to paths the fake does not serve
    codes: list[str] = field(default_factory=list)  # every authorization code issued
    verifiers: list[str] = field(default_factory=list)  # every code_verifier an exchange sent
    redirects: list[str] = field(default_factory=list)  # every address the browser was sent back to

    def __repr__(self) -> str:
        return f"FakeIssuer({self.url if self._server else 'not started'})"

    def __post_init__(self) -> None:
        self.key = rsa_key()
        self._lock = threading.Lock()
        self._pending: dict[str, dict[str, str]] = {}
        self._access: dict[str, str] = {}  # live access token -> client id
        self._refresh: dict[str, str] = {}  # refresh token -> "live" | "rotated" | "revoked"
        self._server: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None

    # -- lifecycle --------------------------------------------------------

    def start(self) -> "FakeIssuer":
        issuer = self

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args: object) -> None:  # quiet
                pass

            def do_GET(self) -> None:  # noqa: N802
                issuer._get(self)

            def do_POST(self) -> None:  # noqa: N802
                issuer._post(self)

        self._server = ThreadingHTTPServer((self.host, 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return self

    def close(self) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()

    @property
    def url(self) -> str:
        assert self._server is not None
        return f"http://{self.host}:{self._server.server_address[1]}"

    def env(self) -> dict[str, str]:
        """The test overrides that point craze's sign-in at this fake."""
        return {ISSUER_ENV: self.url, API_ENV: self.url + "/v1"}

    # -- the browser ------------------------------------------------------

    def authorize(self, auth_url: str, decline: bool = False) -> str:
        """Stand in for the browser and the person approving craze: read the
        authorization URL as the server would, issue a code bound to its PKCE
        challenge, redirect address and client id, and answer the address
        the browser is sent back to. A first registration's redirect carries
        the issued client id; a re-login's does not. decline answers
        error=access_denied instead."""
        u = urlsplit(auth_url)
        if f"{u.scheme}://{u.netloc}{u.path}" != self.url + AUTHORIZE_PATH:
            pytest.fail(f"the authorization URL is not the fake's authorize endpoint: {auth_url}", pytrace=False)
        multi = parse_qs(u.query, keep_blank_values=True)
        if any(len(v) != 1 for v in multi.values()):
            pytest.fail(f"a parameter is repeated in the authorization URL: {sorted(multi)}", pytrace=False)
        q = {k: v[0] for k, v in multi.items()}
        with self._lock:
            self.authorizations.append(q)
        redirect = q["redirect_uri"]
        if decline:
            back = redirect + "?" + urlencode({"error": "access_denied", "state": q["state"], "error_description": ERROR_DESCRIPTION})
            with self._lock:
                self.redirects.append(back)
            return back
        registering = q["client_id"] == DYNAMIC_CLIENT
        client = self.issue_client if registering else q["client_id"]
        code = "fake-code-" + secrets.token_hex(8)
        with self._lock:
            self._pending[code] = {
                "challenge": q.get("code_challenge", ""),
                "method": q.get("code_challenge_method", ""),
                "redirect": redirect,
                "client": client,
                "nonce": q.get("nonce", ""),
            }
        out = {"code": code, "state": q["state"], "scope": self.scope}
        if registering:
            out["client_id"] = client
        back = redirect + "?" + urlencode(out)
        with self._lock:
            self.codes.append(code)
            self.redirects.append(back)
        return back

    # -- the leak scan ----------------------------------------------------

    def assert_no_token(self, where: str, text: str | bytes) -> None:
        """Fail when text holds any token value this fake issued, naming
        where and the value's length -- never the value -- and with no
        traceback, whose frames would show it."""
        data = text.encode() if isinstance(text, str) else text
        with self._lock:
            issued = list(self.issued)
        if not issued:
            pytest.fail("the leak scan has no issued token to look for; it would pass vacuously", pytrace=False)
        for value in issued:
            if value.encode() in data:
                pytest.fail(f"an issued token value ({len(value)} bytes) is in {where}", pytrace=False)

    def masked(self, text: str) -> str:
        """text with every token value this fake issued replaced by <TOKEN>:
        what a failure may print."""
        with self._lock:
            issued = sorted(self.issued, key=len, reverse=True)
        for value in issued:
            text = text.replace(value, "<TOKEN>")
        return text

    def files_holding_a_token(self, root: Path) -> list[Path]:
        with self._lock:
            needles = [v.encode() for v in self.issued]
        return sorted(p for p in root.rglob("*") if p.is_file() and any(n in p.read_bytes() for n in needles))

    def secret_values(self) -> list[str]:
        """Every value of the sign-ins this fake served that craze must never
        log (plan 034 A11): the tokens it issued, the codes and the verifiers
        it saw, each authorization's state, nonce, PKCE challenge, host id
        and login_hint, the account's email and subject, the issued client
        id, the error_description, and every address the browser was sent
        back to. Short generic values are left out (they could be any text)."""
        with self._lock:
            values = [*self.issued, *self.codes, *self.verifiers, *self.redirects]
            for q in self.authorizations:
                values += [q.get(k, "") for k in ("state", "nonce", "code_challenge", "ext_agent_host_id", "login_hint")]
        values += [self.email, self.subject, self.issue_client, ERROR_DESCRIPTION]
        return [v for v in dict.fromkeys(values) if len(v) >= 8]

    def revoked_issued_refresh_tokens(self) -> int:
        """How many refresh tokens this fake issued were revoked through its
        revocation endpoint (by the craze that held them)."""
        with self._lock:
            return sum(1 for state in self._refresh.values() if state == "revoked")

    # -- the endpoints ----------------------------------------------------

    def _json(self, h: BaseHTTPRequestHandler, status: int, body: object, headers: dict[str, str] | None = None) -> None:
        data = json.dumps(body).encode()
        h.send_response(status)
        h.send_header("Content-Type", "application/json")
        h.send_header("Content-Length", str(len(data)))
        for k, v in (headers or {}).items():
            h.send_header(k, v)
        h.end_headers()
        h.wfile.write(data)

    def _get(self, h: BaseHTTPRequestHandler) -> None:
        path = urlsplit(h.path).path
        if path == DISCOVERY_PATH:
            self._json(h, 200, {
                "issuer": self.url,
                "authorization_endpoint": self.url + AUTHORIZE_PATH,
                "token_endpoint": self.url + TOKEN_PATH,
                "revocation_endpoint": self.url + REVOKE_PATH,
                "jwks_uri": self.url + JWKS_PATH,
            })
        elif path == JWKS_PATH:
            self._json(h, 200, {"keys": [{
                "kty": "RSA", "kid": self.kid, "use": "sig", "alg": "RS256",
                "n": b64url(_int_bytes(self.key.n)), "e": b64url(_int_bytes(self.key.e)),
            }]})
        elif path == MODELS_PATH:
            token = h.headers.get("Authorization", "").removeprefix("Bearer ")
            # keep_blank_values: `client_version=&client_version=0.160.0` is two
            # values, one blank, and a 400 (plan 034 r1 #8).
            versions = parse_qs(urlsplit(h.path).query, keep_blank_values=True).get("client_version", [])
            with self._lock:
                self.models_gets += 1
                live = token in self._access
                if len(versions) != 1 or not versions[0]:
                    self.models_without_version += 1
                else:
                    self.model_versions.append(versions[0])
            if len(versions) != 1 or not versions[0]:
                self._json(h, 400, {"error": {"code": "client_version_required"}})
                return
            if not live:
                self._json(h, 401, {"error": {"code": "invalid_api_key"}})
                return
            self._json(h, 200, {"models": gated_models(self.models, versions[0])},
                       {"X-Models-Etag": "fake-models-etag-1"})
        else:
            with self._lock:
                self.stray.append(f"GET {path}")
            self._json(h, 404, {"error": "not_found"})

    def _form(self, h: BaseHTTPRequestHandler) -> dict[str, str]:
        n = int(h.headers.get("Content-Length") or 0)
        return {k: v[0] for k, v in parse_qs(h.rfile.read(n).decode(), keep_blank_values=True).items()}

    def _post(self, h: BaseHTTPRequestHandler) -> None:
        path = urlsplit(h.path).path
        form = self._form(h)
        if path == TOKEN_PATH:
            self._token(h, form)
        elif path == REVOKE_PATH:
            with self._lock:
                self.revokes.append(form)
                if self.revoke_status == 200 and form.get("token") in self._refresh:
                    self._refresh[form["token"]] = "revoked"
            h.send_response(self.revoke_status)
            h.send_header("Content-Length", "0")
            h.end_headers()
        else:
            with self._lock:
                self.stray.append(f"POST {path}")
            self._json(h, 404, {"error": "not_found"})

    def _mint(self, client: str) -> tuple[str, str]:
        """A live access and refresh token for client. The access token is
        JWT-shaped (the docs call it opaque) with a client_id claim, as the
        real one has; craze checks that claim. Called with the lock held."""
        claims = {"client_id": client, "aud": RESOURCE, "r": secrets.token_hex(8)}
        access = ".".join([b64url(b'{"alg":"none"}'), b64url(json.dumps(claims).encode()), "fakesig" + secrets.token_hex(4)])
        refresh = "fake-refresh-" + secrets.token_hex(12)
        self._access[access] = client
        self._refresh[refresh] = "live"
        self.issued += [access, refresh]
        return access, refresh

    def _id_token(self, client: str, nonce: str) -> str:
        now = int(time.time())
        claims = {
            "iss": self.url, "aud": client, "sub": self.subject, "email": self.email,
            "iat": now, "exp": now + 3600, "auth_time": now, "jti": secrets.token_hex(8),
        }
        if nonce:
            claims["nonce"] = nonce
        token = sign_jwt(self.key, self.kid, claims)
        if self.tamper_id_token:
            token = token[:-4] + ("AAAA" if not token.endswith("AAAA") else "BBBB")
        self.issued.append(token)
        return token

    def _token(self, h: BaseHTTPRequestHandler, form: dict[str, str]) -> None:
        if form.get("resource") != RESOURCE:
            self._json(h, 400, {"error": "invalid_target"})
            return
        grant = form.get("grant_type")
        if grant == "authorization_code":
            with self._lock:
                pending = self._pending.pop(form.get("code", ""), None)
                verifier = form.get("code_verifier", "")
                self.verifiers.append(verifier)
                pkce_ok = (
                    pending is not None
                    and not self.wrong_challenge
                    and pending["method"] == "S256"
                    and b64url(hashlib.sha256(verifier.encode()).digest()) == pending["challenge"]
                )
                granted = (
                    pkce_ok
                    and pending["client"] == form.get("client_id")
                    and pending["redirect"] == form.get("redirect_uri")
                )
                self.exchanges.append(Exchange(form.get("client_id", ""), form.get("redirect_uri", ""), pkce_ok, granted))
                if not granted:
                    self._json(h, 400, {"error": "invalid_grant", "error_description": ERROR_DESCRIPTION})
                    return
                access, refresh = self._mint(pending["client"])
                id_token = self._id_token(pending["client"], pending["nonce"])
            self._json(h, 200, {
                "access_token": access, "refresh_token": refresh, "id_token": id_token,
                "token_type": "Bearer", "expires_in": 3600, "scope": self.scope,
                "earliest_refresh_at": int(time.time()) + 3240,
            })
        elif grant == "refresh_token":
            with self._lock:
                state = self._refresh.get(form.get("refresh_token", ""))
                if state != "live":
                    self._json(h, 400, {"error": "refresh_token_reused" if state == "rotated" else "invalid_grant"})
                    return
                self._refresh[form["refresh_token"]] = "rotated"
                access, refresh = self._mint(form.get("client_id", ""))
                id_token = self._id_token(form.get("client_id", ""), "")
            self._json(h, 200, {
                "access_token": access, "refresh_token": refresh, "id_token": id_token,
                "token_type": "Bearer", "expires_in": 3600, "scope": self.scope,
            })
        else:
            self._json(h, 400, {"error": "unsupported_grant_type"})
