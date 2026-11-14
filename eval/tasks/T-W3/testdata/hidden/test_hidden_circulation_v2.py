"""T-W3's hidden tests: a fake Bindlewick Circulation API on a loopback port that answers
only version 2 as its changelog (the task's corpus) describes it; version 1 is gone."""

from __future__ import annotations

import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from bindlewick import BindlewickError, Checkout, Client, RenewalRefused

KEY = "bw_test_3f9a61c2"
SITE = "ELM"
PATRON = "p-1042"


def loan(id, copy, title, due_at, renewals=0, status="active"):
    return {"id": id, "patron_id": PATRON, "copy_ref": copy, "title": title,
            "lent_at": "2026-11-10T14:02:11-05:00", "due_at": due_at, "renewals": renewals, "status": status}


# Due at 20:00 local time, UTC-5: in UTC it is already the next day.
ON_SHELF = {"31205000418823": ("ln_4KD8", "The Lantern Orchard", "2026-12-01T20:00:00-05:00")}
LOANS = {
    PATRON: [loan("ln_9XW1", "31205000377102", "A Field Guide to Moss", "2026-11-24T20:00:00-05:00"),
             loan("ln_2MB5", "31205000512940", "Night Ferry to Vallin", "2026-11-28T09:30:00-05:00", renewals=3),
             loan("ln_7HQ3", "31205000600417", "Small Engines, Kept", "2026-11-30T21:15:00-05:00")],
    "p-2001": [],
}
RENEWED = {"ln_9XW1": "2026-12-15T20:00:00-05:00"}
REFUSALS = {"ln_2MB5": "loans.renewal_cap", "ln_0RT6": "loans.not_active", "ln_7HQ3": "holds.waiting"}


class FakeCirculation(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status, code):
        self._send(status, {"error": {"code": code, "message": code}})

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        if not raw:
            return None
        try:
            return json.loads(raw)
        except ValueError:
            return "malformed"

    def _handle(self, method):
        u = urllib.parse.urlsplit(self.path)
        body = self._body() if method == "POST" else None
        self.server.seen.append({"method": method, "path": u.path, "query": u.query, "body": body,
                                 "auth": self.headers.get("Authorization"), "site": self.headers.get("Bindlewick-Site")})
        if len(self.server.seen) > 20:
            return self._error(429, "too_many")
        parts = u.path.split("/")
        if len(parts) < 3 or parts[1] != "v2":
            return self._error(404, "not_found")
        if self.headers.get("Authorization") != f"Bearer {KEY}":
            return self._error(401, "auth.invalid_key")
        if u.query:
            return self._error(400, "params.unknown")
        if self.headers.get("Bindlewick-Site") is None:
            return self._error(400, "site.missing")
        if self.headers.get("Bindlewick-Site") != SITE:
            return self._error(403, "site.forbidden")
        rest = [urllib.parse.unquote(p) for p in parts[2:]]
        if method == "POST" and rest == ["loans"]:
            return self._lend(body)
        if method == "GET" and len(rest) == 3 and rest[0] == "patrons" and rest[2] == "loans":
            if rest[1] not in LOANS:
                return self._error(404, "patrons.unknown")
            return self._send(200, {"loans": LOANS[rest[1]]})
        if method == "POST" and len(rest) == 3 and rest[0] == "loans" and rest[2] == "renewals":
            return self._renew(rest[1], body)
        return self._error(404, "not_found")

    def _lend(self, body):
        if not isinstance(body, dict) or set(body) != {"patron_id", "copy_ref"}:
            return self._error(400, "params.unknown")
        if body["patron_id"] not in LOANS:
            return self._error(404, "patrons.unknown")
        if body["copy_ref"] not in ON_SHELF:
            return self._error(404, "copies.unknown")
        id, title, due_at = ON_SHELF[body["copy_ref"]]
        return self._send(201, {"loan": loan(id, body["copy_ref"], title, due_at)})

    def _renew(self, loan_id, body):
        # Version 2's renewal body is an empty JSON object (the changelog); no body at all,
        # or any other, is a missing or unknown body field (400, the errors page).
        if body != {}:
            return self._error(400, "params.unknown")
        if loan_id in REFUSALS:
            return self._error(409, REFUSALS[loan_id])
        current = next((x for x in LOANS[PATRON] if x["id"] == loan_id), None)
        if current is None or loan_id not in RENEWED:
            return self._error(404, "loans.unknown")
        return self._send(201, {"loan": {**current, "due_at": RENEWED[loan_id], "renewals": current["renewals"] + 1}})

    def do_GET(self):
        self._handle("GET")

    def do_POST(self):
        self._handle("POST")

    def do_PUT(self):
        self._handle("PUT")


@pytest.fixture
def api():
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeCirculation)
    server.seen = []
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        yield server
    finally:
        server.shutdown()
        server.server_close()


def client(server, key=KEY):
    return Client(f"http://127.0.0.1:{server.server_address[1]}", key, branch=SITE)


def call(fn, timeout=20):
    """fn() in a daemon thread, so a client that hangs fails instead of hanging the run."""
    out = {}

    def run():
        try:
            out["value"] = fn()
        except BaseException as e:  # noqa: BLE001 -- handed back to the test
            out["error"] = e

    t = threading.Thread(target=run, daemon=True)
    t.start()
    t.join(timeout)
    assert not t.is_alive(), "the client did not return"
    if "error" in out:
        raise out["error"]
    return out["value"]


def test_checkout_lends_the_copy(api):
    got = call(lambda: client(api).checkout(PATRON, "31205000418823"))
    # The due day is the date part as the site wrote it, not UTC's.
    assert got == Checkout("ln_4KD8", "31205000418823", "The Lantern Orchard", "2026-12-01")
    assert len(api.seen) == 1 and api.seen[0]["body"] == {"patron_id": PATRON, "copy_ref": "31205000418823"}


def test_checkouts_lists_every_loan_in_order(api):
    got = call(lambda: client(api).checkouts(PATRON))
    assert got == [
        Checkout("ln_9XW1", "31205000377102", "A Field Guide to Moss", "2026-11-24"),
        Checkout("ln_2MB5", "31205000512940", "Night Ferry to Vallin", "2026-11-28"),
        Checkout("ln_7HQ3", "31205000600417", "Small Engines, Kept", "2026-11-30"),
    ]
    assert call(lambda: client(api).checkouts("p-2001")) == []


def test_renew_moves_the_due_day(api):
    got = call(lambda: client(api).renew("ln_9XW1"))
    assert got == Checkout("ln_9XW1", "31205000377102", "A Field Guide to Moss", "2026-12-15")


def test_a_renewal_over_the_limit_is_refused(api):
    with pytest.raises(RenewalRefused):
        call(lambda: client(api).renew("ln_2MB5"))


def test_other_renewal_conflicts_are_not_refusals(api):
    for loan_id in ("ln_0RT6", "ln_7HQ3"):
        with pytest.raises(BindlewickError) as e:
            call(lambda: client(api).renew(loan_id))
        assert not isinstance(e.value, RenewalRefused), loan_id


def test_every_request_names_the_site_and_the_key(api):
    c = client(api)
    call(lambda: c.checkouts(PATRON))
    call(lambda: c.renew("ln_9XW1"))
    call(lambda: c.checkout(PATRON, "31205000418823"))
    assert len(api.seen) == 3
    assert all(r["site"] == SITE and r["auth"] == f"Bearer {KEY}" and r["query"] == "" for r in api.seen)


def test_errors_are_api_errors(api):
    with pytest.raises(BindlewickError):
        call(lambda: client(api).checkouts("p-9999"))
    with pytest.raises(BindlewickError):
        call(lambda: client(api, key="bw_test_wrong").checkouts(PATRON))
    with pytest.raises(BindlewickError):
        call(lambda: client(api).checkout(PATRON, "31205999999999"))
