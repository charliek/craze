"""T-W1's hidden tests: a fake Quayside API v3 on a loopback port, answering only the
requests its documentation (the task's corpus) describes."""

from __future__ import annotations

import json
import threading
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from quayside import Client, QuaysideError, Slot

KEY = "qs_test_7c1d4e0b9a"
HARBOR = "rotterdam-maasvlakte"
DAY = "2026-03-14"


def slot(berth, vessel, start, end, pilot="none"):
    return {"berth": berth, "vessel": vessel, "start": start, "end": end, "pilot": pilot}


# Two pages for HARBOR on DAY, one page for "gdansk-north".
PAGES = {
    (HARBOR, None): {"slots": [slot("B7", "Nordic Star", "2026-03-14T06:00:00Z", "2026-03-14T14:00:00Z", "required"),
                               slot("B9", "Alma Rios", "2026-03-14T07:30:00Z", "2026-03-14T19:00:00Z")],
                     "next_page_mark": "pm_8f2c41"},
    (HARBOR, "pm_8f2c41"): {"slots": [slot("B7", "Kestrel Bay", "2026-03-14T15:00:00Z", "2026-03-14T23:00:00Z")],
                            "next_page_mark": None},
    ("gdansk-north", None): {"slots": [slot("N2", "Baltic Wren", "2026-03-14T05:00:00Z", "2026-03-14T09:00:00Z")],
                             "next_page_mark": None},
}


class FakeQuayside(BaseHTTPRequestHandler):
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

    def do_GET(self):
        u = urllib.parse.urlsplit(self.path)
        self.server.seen.append((u.path, u.query, self.headers.get("X-Quayside-Key")))
        if len(self.server.seen) > 20:
            return self._error(429, "too_many")
        parts = u.path.split("/")
        if len(parts) != 5 or parts[:3] != ["", "v3", "harbors"] or parts[4] != "berth-slots":
            return self._error(404, "not_found")
        if self.headers.get("X-Quayside-Key") != KEY:
            return self._error(401, "auth.missing_key")
        q = urllib.parse.parse_qs(u.query, keep_blank_values=True)
        if q.get("service_date") != [DAY]:
            return self._error(400, "params.service_date")
        if set(q) - {"service_date", "page_mark", "berth"}:
            return self._error(400, "params.unknown")
        mark = q.get("page_mark", [None])[0]
        page = PAGES.get((urllib.parse.unquote(parts[3]), mark))
        if page is None:
            return self._error(400 if mark else 404, "paging.bad_mark" if mark else "harbors.unknown")
        self._send(200, {"harbor": parts[3], "service_date": DAY, **page})


@pytest.fixture
def api():
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeQuayside)
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
    assert not t.is_alive(), "berth_schedule did not return"
    if "error" in out:
        raise out["error"]
    return out["value"]


def test_every_slot_across_pages(api):
    got = call(lambda: Client(base(api), KEY).berth_schedule(HARBOR, DAY))
    assert got == [
        Slot("B7", "Nordic Star", "2026-03-14T06:00:00Z", "2026-03-14T14:00:00Z"),
        Slot("B9", "Alma Rios", "2026-03-14T07:30:00Z", "2026-03-14T19:00:00Z"),
        Slot("B7", "Kestrel Bay", "2026-03-14T15:00:00Z", "2026-03-14T23:00:00Z"),
    ]
    assert len(api.seen) == 2


def test_a_single_page(api):
    got = call(lambda: Client(base(api) + "/", KEY).berth_schedule("gdansk-north", DAY))
    assert got == [Slot("N2", "Baltic Wren", "2026-03-14T05:00:00Z", "2026-03-14T09:00:00Z")]
    assert len(api.seen) == 1


def test_every_request_carries_the_key(api):
    call(lambda: Client(base(api), KEY).berth_schedule(HARBOR, DAY))
    assert api.seen and all(key == KEY for _, _, key in api.seen)


def test_a_rejected_key_is_an_error(api):
    with pytest.raises(QuaysideError):
        call(lambda: Client(base(api), "qs_test_wrong").berth_schedule(HARBOR, DAY))


def test_an_unknown_harbor_is_an_error(api):
    with pytest.raises(QuaysideError):
        call(lambda: Client(base(api), KEY).berth_schedule("atlantis", DAY))
