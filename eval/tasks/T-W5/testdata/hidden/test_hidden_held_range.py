"""T-W5's hidden tests: a fake Tessaroot API v2 on a loopback port. A held range (error
TSR-7719) answers only the follow-up its troubleshooting page (the task's corpus)
describes; retrying unchanged, or any other option, keeps failing."""

from __future__ import annotations

import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from tessaroot import Client, Reading, TessarootError

TOKEN = "tk_test_51b0e8d2"
REF = "bf_3a91c7"
START, END = "2026-03-14T00:00:00Z", "2026-03-15T00:00:00Z"
PATH = "/v2/houses/{house}/sensors/{sensor}/readings"


def reading(at, value, unit, state=None):
    obj = {"at": at, "value": value, "unit": unit}
    if state is not None:
        obj["settle_state"] = state
    return obj


SETTLED = {("north-bay-3", "t-01"): [reading("2026-03-14T06:00:00Z", 21.5, "degC"),
                                     reading("2026-03-14T06:05:00Z", 21.75, "degC")]}
# Held ranges: what the follow-up returns.
HELD = {("propagation-1", "rh-04"): [reading("2026-03-14T06:00:00Z", 81.5, "pct_rh", "final"),
                                     reading("2026-03-14T06:05:00Z", 82.0, "pct_rh", "pending"),
                                     reading("2026-03-14T06:10:00Z", 82.25, "pct_rh", "pending")]}
RETIRED = {("old-wing", "sm-02")}


class FakeTessaroot(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status, code, **extra):
        self._send(status, {"error": {"code": code, "message": code, **extra}})

    def do_GET(self):
        u = urllib.parse.urlsplit(self.path)
        ref = self.headers.get("Tessaroot-Backfill-Ref")
        q = urllib.parse.parse_qs(u.query, keep_blank_values=True)
        self.server.seen.append((u.path, q, ref))
        if len(self.server.seen) > 12:
            return self._error(429, "TSR-4290")
        parts = u.path.split("/")
        if len(parts) != 7 or parts[:3] != ["", "v2", "houses"] or parts[4] != "sensors" or parts[6] != "readings":
            return self._error(404, "TSR-4040")
        if self.headers.get("Authorization") != f"Bearer {TOKEN}":
            return self._error(401, "TSR-4013")
        if q.get("from") != [START] or q.get("to") != [END]:
            return self._error(400, "TSR-4001")
        if set(q) - {"from", "to", "resolution", "unsettled"}:
            return self._error(400, "TSR-4019")
        key = (urllib.parse.unquote(parts[3]), urllib.parse.unquote(parts[5]))
        out = {"house": key[0], "sensor": key[1]}
        if key in RETIRED:
            return self._error(409, "TSR-7702", retired_at="2026-02-01T00:00:00Z")
        if key in SETTLED:
            return self._send(200, {**out, "readings": SETTLED[key]})
        if key not in HELD:
            return self._error(404, "TSR-4040")
        if "unsettled" not in q:
            return self._error(409, "TSR-7719", backfill_ref=REF)
        if q["unsettled"] != ["include"] or ref != REF:
            return self._error(400, "TSR-4019")
        self._send(200, {**out, "readings": HELD[key]})


@pytest.fixture
def api():
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeTessaroot)
    server.seen = []
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
    """fn() in a daemon thread, so a client that retries forever fails instead of hanging."""
    out = {}

    def run():
        try:
            out["value"] = fn()
        except BaseException as e:  # noqa: BLE001 -- handed back to the test
            out["error"] = e

    t = threading.Thread(target=run, daemon=True)
    t.start()
    t.join(timeout)
    assert not t.is_alive(), "readings did not return"
    if "error" in out:
        raise out["error"]
    return out["value"]


def test_a_settled_range_is_one_request(api):
    got = call(lambda: Client(base(api), TOKEN).readings("north-bay-3", "t-01", START, END))
    assert got == [Reading("2026-03-14T06:00:00Z", 21.5, "degC"), Reading("2026-03-14T06:05:00Z", 21.75, "degC")]
    assert len(api.seen) == 1


def test_a_held_range_returns_its_readings(api):
    got = call(lambda: Client(base(api) + "/", TOKEN).readings("propagation-1", "rh-04", START, END))
    assert got == [
        Reading("2026-03-14T06:00:00Z", 81.5, "pct_rh", provisional=False),
        Reading("2026-03-14T06:05:00Z", 82.0, "pct_rh", provisional=True),
        Reading("2026-03-14T06:10:00Z", 82.25, "pct_rh", provisional=True),
    ]


def test_a_held_range_takes_one_follow_up(api):
    call(lambda: Client(base(api), TOKEN).readings("propagation-1", "rh-04", START, END))
    assert len(api.seen) == 2
    (p1, q1, _), (p2, q2, ref2) = api.seen
    assert p1 == p2 == PATH.format(house="propagation-1", sensor="rh-04")
    assert q2.get("from") == [START] and q2.get("to") == [END]
    assert ref2 == REF


def test_another_conflict_is_still_an_error(api):
    with pytest.raises(TessarootError) as e:
        call(lambda: Client(base(api), TOKEN).readings("old-wing", "sm-02", START, END))
    assert (e.value.status, e.value.code) == (409, "TSR-7702")
    assert len(api.seen) == 1


def test_a_rejected_token_is_an_error(api):
    with pytest.raises(TessarootError) as e:
        call(lambda: Client(base(api), "tk_test_wrong").readings("propagation-1", "rh-04", START, END))
    assert e.value.status == 401


def test_an_unknown_house_is_an_error(api):
    with pytest.raises(TessarootError) as e:
        call(lambda: Client(base(api), TOKEN).readings("atlantis", "t-01", START, END))
    assert (e.value.status, e.value.code) == (404, "TSR-4040")
