"""T-W6's hidden tests: a fake Orvelline Passes API v2 on a loopback port. At "busy
times" (some travellers) a booking is held: 409 booking.hold_pending, finished only by
the resend its troubleshooting guide (the task's corpus) describes, sent once the answer's
settle_cs has passed, and repeated for as long as the answer is hold_pending (see HELD for
how the timing is judged)."""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from orvelline import Booking, Client, OrvellineError

KEY = "ok_test_3d81c5a0f2"
DEDUP = "Orvelline-Dedup-Key"
RESUME = "Orvelline-Resume-Hold"
PASSES = {"ZONE-AB-7D": (4150, 6), "ZONE-ABC-30D": (13900, 29)}

# A held traveller's waits (one per hold_pending answer, in centiseconds) and how the claim
# ends once they are over: "book" or "release" (the quota sold out).
#
# Timing is judged so that load cannot fail a client that does as documented. A resend
# earlier than its answer's wait (less a little slack for clock granularity) is refused,
# and a client that slept the wait after reading the answer can never be early: the wait
# is timed from before the answer is sent. The late bound is LAPSE_S, far wider than the
# guide's 3 s, so no scheduling delay reaches it. What the late bound no longer catches,
# the long hold does: thirty answers whose waits differ and sum to under a second, inside
# CALL_TIMEOUT_S. So no fixed pause passes (tr_7e22d1 needs at least 3.6 s; thirty of
# those exceed CALL_TIMEOUT_S), nor a wait read in milliseconds (early) or in seconds
# (too slow), nor reusing an earlier answer's wait (early), nor giving up after a fixed
# number of resends.
LONG_WAITS = [2, 5, 1, 4, 3] * 6
HELD = {
    "tr_7e22d1": ([400], "book"),
    "tr_b40f93": ([15, 40], "book"),
    "tr_d8813c": ([20], "release"),
    "tr_6f30b8": (LONG_WAITS, "book"),
}
LAPSE_S = 15.0
CALL_TIMEOUT_S = 45
OVERLAP = {"tr_0a5e77"}


def ends_on(starts_on, days):
    y, m, d = map(int, starts_on.split("-"))
    return f"{y:04d}-{m:02d}-{d + days:02d}"


class FakeOrvelline(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _send(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _error(self, status, code, details=None):
        err = {"code": code, "message": code}
        if details is not None:
            err["details"] = details
        self._send(status, {"error": err})

    def _book(self, body):
        s = self.server
        price, days = PASSES[body["pass_code"]]
        booking = {"reference": f"OB-{3001 + len(s.bookings)}", **body,
                   "ends_on": ends_on(body["starts_on"], days),
                   "price": {"amount_cents": price, "currency": "EUR"}}
        s.bookings.append(booking)
        return 201, {"booking": booking}

    def _pending(self, hold):
        hold["issued"] = time.monotonic()
        return 409, {"error": {"code": "booking.hold_pending", "message": "booking.hold_pending",
                               "details": {"quota_claim": hold["token"], "settle_cs": hold["waits"][0],
                                           "quota_left": 12}}}

    def do_POST(self):
        s = self.server
        with s.lock:
            dedup = self.headers.get(DEDUP) or None
            resume = self.headers.get(RESUME) or None
            raw = self.rfile.read(int(self.headers.get("Content-Length") or 0))
            s.seen.append({"path": self.path, "dedup": dedup, "resume": resume, "at": time.monotonic()})
            if len(s.seen) > 60:
                return self._error(429, "rate.exceeded")
            if self.path != "/v2/pass-bookings":
                return self._error(404, "not_found")
            if self.headers.get("Authorization") != f"Bearer {KEY}":
                return self._error(401, "auth.invalid_token")
            try:
                body = json.loads(raw)
                assert set(body) == {"traveller_id", "pass_code", "starts_on"}
                assert all(isinstance(v, str) for v in body.values())
            except (ValueError, AssertionError, TypeError):
                return self._error(400, "body.invalid")
            if resume:
                status, out = self._resume(resume, dedup, body)
            else:
                status, out = self._fresh(dedup, body)
            if dedup and not resume:
                s.answers.setdefault(dedup, (body, status, out))
            return self._send(status, out)

    def _resume(self, token, dedup, body):
        s = self.server
        hold = s.holds.get(token)
        if hold is None:
            return 404, {"error": {"code": "holds.unknown", "message": "holds.unknown"}}
        if hold["released"]:
            return 410, {"error": {"code": "booking.hold_released", "message": "booking.hold_released"}}
        if dedup is None or dedup != hold["dedup"] or body != hold["body"]:
            return 422, {"error": {"code": "booking.dedup_mismatch", "message": "booking.dedup_mismatch"}}
        waited, wait = time.monotonic() - hold["issued"], hold["waits"][0] / 100
        # A little slack for clock granularity; a client that waited as told is never early.
        if waited < wait * 0.9:
            hold["released"] = True
            s.early += 1
            return 425, {"error": {"code": "booking.resent_too_early", "message": "booking.resent_too_early"}}
        if waited > wait + LAPSE_S:
            hold["released"] = True
            s.late += 1
            return 410, {"error": {"code": "booking.hold_lapsed", "message": "booking.hold_lapsed"}}
        hold["waits"].pop(0)
        if hold["waits"]:
            return self._pending(hold)
        if hold["end"] == "release":
            hold["released"] = True
            return 410, {"error": {"code": "booking.hold_released", "message": "booking.hold_released"}}
        hold["released"] = True
        return self._book(body)

    def _fresh(self, dedup, body):
        s = self.server
        if dedup and dedup in s.answers:
            first, status, out = s.answers[dedup]
            if first != body:
                return 422, {"error": {"code": "booking.dedup_mismatch", "message": "booking.dedup_mismatch"}}
            return status, out
        if body["pass_code"] not in PASSES:
            return 422, {"error": {"code": "pass.unknown", "message": "pass.unknown"}}
        if body["traveller_id"] in OVERLAP:
            return 409, {"error": {"code": "traveller.pass_overlap", "message": "traveller.pass_overlap",
                                   "details": {"conflicting_booking": "OB-2190"}}}
        if body["traveller_id"] in HELD:
            waits, end = HELD[body["traveller_id"]]
            token = f"qc_{len(s.holds) + 1:04x}e2b7"
            hold = {"token": token, "dedup": dedup, "body": body, "waits": list(waits), "end": end,
                    "released": False}
            s.holds[token] = hold
            return self._pending(hold)
        return self._book(body)


@pytest.fixture
def api():
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeOrvelline)
    server.lock = threading.Lock()
    server.seen, server.bookings, server.holds, server.answers = [], [], {}, {}
    server.early = server.late = 0
    t = threading.Thread(target=server.serve_forever, daemon=True)
    t.start()
    try:
        yield server
    finally:
        server.shutdown()
        server.server_close()


def client(server):
    return Client(f"http://127.0.0.1:{server.server_address[1]}", KEY)


def call(fn, timeout=CALL_TIMEOUT_S):
    """fn() in a daemon thread, so a client that loops forever fails instead of hanging."""
    out = {}

    def run():
        try:
            out["value"] = fn()
        except BaseException as e:  # noqa: BLE001 -- handed back to the test
            out["error"] = e

    t = threading.Thread(target=run, daemon=True)
    t.start()
    t.join(timeout)
    assert not t.is_alive(), "book_pass did not return"
    if "error" in out:
        raise out["error"]
    return out["value"]


def test_an_off_peak_booking(api):
    got = call(lambda: client(api).book_pass("tr_51c0aa", "ZONE-AB-7D", "2026-10-03"))
    assert got == Booking("OB-3001", "tr_51c0aa", "ZONE-AB-7D", "2026-10-03", 4150)
    assert len(api.seen) == 1 and len(api.bookings) == 1


def test_a_held_booking_is_finished(api):
    got = call(lambda: client(api).book_pass("tr_7e22d1", "ZONE-ABC-30D", "2026-10-01"))
    assert got == Booking("OB-3001", "tr_7e22d1", "ZONE-ABC-30D", "2026-10-01", 13900)
    assert len(api.bookings) == 1 and len(api.holds) == 1
    first, resend = api.seen
    assert first["dedup"] and resend["dedup"] == first["dedup"]
    assert first["resume"] is None and resend["resume"] == next(iter(api.holds))
    assert api.early == 0 and api.late == 0


def test_a_hold_still_pending_on_the_first_resend(api):
    got = call(lambda: client(api).book_pass("tr_b40f93", "ZONE-AB-7D", "2026-10-05"))
    assert got == Booking("OB-3001", "tr_b40f93", "ZONE-AB-7D", "2026-10-05", 4150)
    assert len(api.seen) == 3 and len(api.bookings) == 1 and len(api.holds) == 1
    assert len({r["dedup"] for r in api.seen}) == 1 and all(r["resume"] for r in api.seen[1:])
    assert api.early == 0 and api.late == 0


def test_a_long_hold_is_resent_until_it_is_decided(api):
    got = call(lambda: client(api).book_pass("tr_6f30b8", "ZONE-AB-7D", "2026-10-06"))
    assert got == Booking("OB-3001", "tr_6f30b8", "ZONE-AB-7D", "2026-10-06", 4150)
    assert len(api.seen) == len(LONG_WAITS) + 1 and len(api.bookings) == 1 and len(api.holds) == 1
    assert len({r["dedup"] for r in api.seen}) == 1 and all(r["resume"] for r in api.seen[1:])
    assert api.early == 0 and api.late == 0


def test_a_released_hold_is_an_error(api):
    with pytest.raises(OrvellineError) as e:
        call(lambda: client(api).book_pass("tr_d8813c", "ZONE-AB-7D", "2026-10-02"))
    assert e.value.code == "booking.hold_released"
    assert len(api.seen) == 2 and not api.bookings and len(api.holds) == 1
    assert api.early == 0 and api.late == 0


def test_each_booking_has_its_own_key(api):
    c = client(api)
    a = call(lambda: c.book_pass("tr_51c0aa", "ZONE-AB-7D", "2026-10-03"))
    b = call(lambda: c.book_pass("tr_51c0aa", "ZONE-AB-7D", "2026-10-17"))
    assert (a.reference, b.reference) == ("OB-3001", "OB-3002")
    assert all(r["dedup"] for r in api.seen) and api.seen[0]["dedup"] != api.seen[1]["dedup"]


def raised_at_once(api, traveller, pass_code, code):
    with pytest.raises(OrvellineError) as e:
        call(lambda: client(api).book_pass(traveller, pass_code, "2026-10-03"))
    assert e.value.code == code
    assert len(api.seen) == 1 and not api.bookings


def test_an_unknown_pass_is_an_error_at_once(api):
    raised_at_once(api, "tr_51c0aa", "ZONE-QX-1D", "pass.unknown")


def test_an_overlapping_pass_is_an_error_at_once(api):
    raised_at_once(api, "tr_0a5e77", "ZONE-AB-7D", "traveller.pass_overlap")
