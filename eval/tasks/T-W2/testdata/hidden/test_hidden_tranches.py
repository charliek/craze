"""T-W2's hidden tests: a fake Skerrywind telemetry API v4 ingest host on a loopback port,
storing only the tranches its documentation (the task's corpus) describes. Every test,
the error cases included, needs the documented path, seal and body: an error case passes
only when the request reached the documented endpoint and failed for the fault under test."""

from __future__ import annotations

import hashlib
import hmac
import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from skerrywind import Client, Reading, SkerrywindError

STATION = "glenmore-ridge-03"
SECRET = "sws_test_4b9e17c2d05a8f63e1"
SEAL = "Skw-Tranche-Seal"
MEASURES = {"air_temp_dk", "station_pres_dapa", "wind_speed_cms"}
FIELDS = {"taken_utc"} | MEASURES

READINGS = [
    Reading("2026-04-02T10:00:00Z", 12.34, 1013.27, 4.2),
    Reading("2026-04-02T10:05:00Z", -3.06, 998.71, 0.35),
    Reading("2026-04-02T10:10:00Z", 0.04, 1001.0, 12.06),
]
# READINGS in version 4's members and units: decikelvin, decapascals, centimetres per second.
STORED = [
    {"taken_utc": "2026-04-02T10:00:00Z", "air_temp_dk": 2855, "station_pres_dapa": 10133, "wind_speed_cms": 420},
    {"taken_utc": "2026-04-02T10:05:00Z", "air_temp_dk": 2701, "station_pres_dapa": 9987, "wind_speed_cms": 35},
    {"taken_utc": "2026-04-02T10:10:00Z", "air_temp_dk": 2732, "station_pres_dapa": 10010, "wind_speed_cms": 1206},
]


def seal(secret, station, raw):
    return "s1=" + hmac.new(secret.encode(), station.encode() + b"\n" + raw, hashlib.sha256).hexdigest()


def path_for(station):
    return f"/v4/stations/{station}/tranches"


def body_problem(raw):
    """The error code for a body that is not a documented tranche, or None; and the body."""
    try:
        doc = json.loads(raw)
    except ValueError:
        return "body.not_json", None
    if not isinstance(doc, dict) or set(doc) != {"tranche_ref", "samples"}:
        return "tranche.malformed", None
    ref, samples = doc["tranche_ref"], doc["samples"]
    if not isinstance(ref, str) or not 1 <= len(ref) <= 64:
        return "tranche.malformed", None
    if not isinstance(samples, list) or not 1 <= len(samples) <= 500:
        return "tranche.malformed", None
    for s in samples:
        if not isinstance(s, dict) or FIELDS - set(s):
            return "samples.missing_field", None
        if set(s) - FIELDS:
            return "samples.unknown_field", None
        if not isinstance(s["taken_utc"], str):
            return "samples.malformed", None
        if any(type(s[k]) is not int for k in MEASURES):
            return "units.not_integer", None
    return None, doc


class FakeIngest(BaseHTTPRequestHandler):
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
        self._send(status, {"error": {"code": code, "message": code}, **extra})

    def do_GET(self):
        self._error(404, "not_found")

    def do_PUT(self):
        self._error(404, "not_found")

    def do_POST(self):
        u = urllib.parse.urlsplit(self.path)
        raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        got = self.headers.get(SEAL)
        self.server.seen.append((u.path, got, raw))
        if len(self.server.seen) > 20:
            return self._error(429, "too_many")
        parts = u.path.split("/")
        if len(parts) != 5 or parts[:3] != ["", "v4", "stations"] or parts[4] != "tranches":
            return self._error(404, "not_found")
        station = urllib.parse.unquote(parts[3])
        if station != STATION:
            return self._error(404, "stations.unknown")
        if not got:
            return self._error(401, "seal.missing")
        if not hmac.compare_digest(got, seal(SECRET, station, raw)):
            return self._error(401, "seal.mismatch")
        problem, doc = body_problem(raw)
        if problem:
            return self._error(400 if problem == "body.not_json" else 422, problem)
        ref, samples = doc["tranche_ref"], doc["samples"]
        if ref in self.server.tranches:
            first, receipt = self.server.tranches[ref]
            ack = {"id": receipt, "queued_at": "2026-04-02T10:00:04Z"}
            if first == samples:
                return self._error(409, "tranche.replayed", acknowledgement=ack)
            # The same kind of body as a replay, so only the error code tells them apart.
            return self._error(409, "tranche.ref_reused", acknowledgement=ack)
        receipt = f"ack_{len(self.server.tranches) + 1:02d}{ref[-4:]}"
        self.server.tranches[ref] = (samples, receipt)
        self._send(202, {"accepted": len(samples),
                         "acknowledgement": {"id": receipt, "queued_at": "2026-04-02T10:00:04Z"}})


@pytest.fixture
def api():
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeIngest)
    server.seen = []
    server.tranches = {}
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
    """fn() in a daemon thread, so a client that hangs fails instead."""
    out = {}

    def run():
        try:
            out["value"] = fn()
        except BaseException as e:  # noqa: BLE001 -- handed back to the test
            out["error"] = e

    t = threading.Thread(target=run, daemon=True)
    t.start()
    t.join(timeout)
    assert not t.is_alive(), "upload did not return"
    if "error" in out:
        raise out["error"]
    return out["value"]


def assert_documented(seen, station, secret, ref, samples):
    """The one request in ``seen`` went to the documented endpoint for ``station``, sealed
    with ``secret`` as documented, carrying ``samples`` under ``ref``."""
    assert len(seen) == 1, seen
    path, got, raw = seen[0]
    assert path == path_for(station)
    assert got == seal(secret, station, raw)
    problem, doc = body_problem(raw)
    assert problem is None, problem
    assert doc == {"tranche_ref": ref, "samples": samples}


def test_a_tranche_is_stored_in_the_documented_fields_and_units(api):
    receipt = call(lambda: Client(base(api), STATION, SECRET).upload(READINGS, "gr03-20260402-1000"))
    assert api.tranches == {"gr03-20260402-1000": (STORED, receipt)}
    assert receipt == "ack_011000"
    assert len(api.seen) == 1


def test_a_trailing_slash_and_a_single_reading(api):
    receipt = call(lambda: Client(base(api) + "/", STATION, SECRET).upload(READINGS[1:2], "gr03-single"))
    assert api.tranches == {"gr03-single": (STORED[1:2], receipt)}
    assert receipt == "ack_01ngle"


def test_a_retried_tranche_returns_the_first_receipt(api):
    client = Client(base(api), STATION, SECRET)
    first = call(lambda: client.upload(READINGS, "gr03-20260402-1000"))
    again = call(lambda: client.upload(READINGS, "gr03-20260402-1000"))
    assert first == again == "ack_011000"
    assert api.tranches == {"gr03-20260402-1000": (STORED, "ack_011000")}
    assert len(api.seen) == 2


def test_a_reused_reference_is_an_error(api):
    client = Client(base(api), STATION, SECRET)
    assert call(lambda: client.upload(READINGS, "gr03-20260402-1000")) == "ack_011000"
    with pytest.raises(SkerrywindError):
        call(lambda: client.upload(READINGS[:2], "gr03-20260402-1000"))
    assert api.tranches == {"gr03-20260402-1000": (STORED, "ack_011000")}
    assert len(api.seen) == 2


def test_a_wrong_secret_is_an_error(api):
    with pytest.raises(SkerrywindError):
        call(lambda: Client(base(api), STATION, "sws_test_wrong").upload(READINGS, "gr03-x"))
    assert api.tranches == {}
    assert_documented(api.seen, STATION, "sws_test_wrong", "gr03-x", STORED)


def test_an_unknown_station_is_an_error(api):
    with pytest.raises(SkerrywindError):
        call(lambda: Client(base(api), "glenmore-ridge-99", SECRET).upload(READINGS, "gr99-x"))
    assert api.tranches == {}
    assert_documented(api.seen, "glenmore-ridge-99", SECRET, "gr99-x", STORED)
