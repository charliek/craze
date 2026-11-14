"""T-W4's hidden tests: a fake Tallowmere Locker API v3 on a loopback port, answering only
the requests its documentation (the task's corpus) describes. Version 2 is retired: every
/v2/ request gets 410, as the live API does."""

from __future__ import annotations

import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from tallowmere import Client, Reservation, TallowmereError

KEY = "tmk_test_3a9e51c0d7"
LOCKER = "lk-utrecht-cs-04"
FULL = "lk-amersfoort-02"
HOLD = "hd_5Qm2Tz"
FIELDS = {"size", "consignment_id", "hold_minutes"}


class FakeTallowmere(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, status, obj=None):
        self.send_response(status)
        if obj is None:
            self.end_headers()
            return
        body = json.dumps(obj).encode()
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status, code):
        self._send(status, {"error": {"code": code, "message": code}})

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def _common(self, method):
        """Records the request and answers what every endpoint answers alike; returns the
        path's parts, or None when it has answered."""
        u = urllib.parse.urlsplit(self.path)
        raw = self._body()
        self.server.seen.append((method, u.path, self.headers.get("TM-Access-Key"), raw))
        if len(self.server.seen) > 20:
            self._error(429, "too_many")
            return None
        parts = u.path.split("/")
        if len(parts) > 1 and parts[1] == "v2":
            self._error(410, "version.retired")
            return None
        if len(parts) < 2 or parts[1] != "v3":
            self._error(404, "not_found")
            return None
        if self.headers.get("TM-Access-Key") != KEY:
            self._error(401, "auth.missing_key")
            return None
        if u.query:
            self._error(400, "params.unknown")
            return None
        return parts, raw

    def do_POST(self):
        got = self._common("POST")
        if got is None:
            return
        parts, raw = got
        if len(parts) != 5 or parts[2] != "lockers" or parts[4] != "holds":
            return self._error(404, "not_found")
        locker = urllib.parse.unquote(parts[3])
        try:
            body = json.loads(raw)
        except ValueError:
            return self._error(400, "params.body")
        if not isinstance(body, dict):
            return self._error(400, "params.body")
        if set(body) - FIELDS:
            return self._error(400, "params.unknown")
        if body.get("size") not in ("S", "M", "L"):
            return self._error(400, "params.size")
        if not isinstance(body.get("consignment_id"), str) or not body["consignment_id"]:
            return self._error(400, "params.consignment_id")
        minutes = body.get("hold_minutes")
        if type(minutes) is not int or not 30 <= minutes <= 4320:
            return self._error(400, "params.hold_minutes")
        if locker == FULL:
            return self._error(409, "lockers.full")
        if locker != LOCKER:
            return self._error(404, "lockers.unknown")
        self.server.holds.add(HOLD)
        self._send(201, {"hold_id": HOLD, "locker_id": LOCKER, "compartment": "B07", "compartment_door": 7,
                         "size": body["size"], "release_pin": "482913", "expires_at": "2026-10-09T10:30:00Z"})

    def do_DELETE(self):
        got = self._common("DELETE")
        if got is None:
            return
        parts, _ = got
        if len(parts) != 4 or parts[2] != "holds":
            return self._error(404, "not_found")
        hold = urllib.parse.unquote(parts[3])
        if hold not in self.server.holds:
            return self._error(404, "holds.unknown")
        self.server.holds.discard(hold)
        self._send(204)

    def do_GET(self):
        if self._common("GET") is not None:
            self._error(404, "not_found")


@pytest.fixture
def api():
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeTallowmere)
    server.seen = []
    server.holds = set()
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        yield server
    finally:
        server.shutdown()
        server.server_close()


def base(server):
    return f"http://127.0.0.1:{server.server_address[1]}"


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


def posted(server):
    return [json.loads(raw) for method, _, _, raw in server.seen if method == "POST"]


def test_reserve_returns_the_hold(api):
    got = call(lambda: Client(base(api), KEY).reserve(LOCKER, "M", "CN-88213"))
    assert got == Reservation(HOLD, LOCKER, "B07", "482913", "2026-10-09T10:30:00Z")
    assert posted(api) == [{"size": "M", "consignment_id": "CN-88213", "hold_minutes": 4320}]


def test_the_hold_length_is_sent_in_minutes(api):
    got = call(lambda: Client(base(api) + "/", KEY).reserve(LOCKER, "S", "CN-90017", ttl_hours=2))
    assert got.id == HOLD and got.code == "482913"
    assert posted(api) == [{"size": "S", "consignment_id": "CN-90017", "hold_minutes": 120}]


def test_cancel_gives_the_compartment_back(api):
    c = Client(base(api), KEY)
    r = call(lambda: c.reserve(LOCKER, "L", "CN-88214"))
    assert call(lambda: c.cancel(r.id)) is None
    assert api.holds == set()
    assert [(m, p) for m, p, _, _ in api.seen] == [("POST", f"/v3/lockers/{LOCKER}/holds"),
                                                   ("DELETE", f"/v3/holds/{HOLD}")]


def test_cancelling_an_unknown_hold_is_an_error(api):
    with pytest.raises(TallowmereError):
        call(lambda: Client(base(api), KEY).cancel("hd_nope"))


def test_every_request_carries_the_key(api):
    c = Client(base(api), KEY)
    call(lambda: c.cancel(c.reserve(LOCKER, "M", "CN-88215").id))
    assert len(api.seen) == 2 and all(key == KEY for _, _, key, _ in api.seen)


def test_a_rejected_key_is_an_error(api):
    with pytest.raises(TallowmereError):
        call(lambda: Client(base(api), "tmk_test_wrong").reserve(LOCKER, "M", "CN-88216"))


def test_a_full_locker_is_an_error(api):
    with pytest.raises(TallowmereError):
        call(lambda: Client(base(api), KEY).reserve(FULL, "M", "CN-88217"))
