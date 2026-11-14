"""A small client for the Orvelline Passes API (version 2).

Standard library only. Every API error -- a status outside 2xx, or a body that is not
JSON -- is raised as OrvellineError, carrying the HTTP status and the API's error code.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from dataclasses import dataclass, field


class OrvellineError(Exception):
    """The API answered with an error, or with something that is not JSON."""

    def __init__(self, status: int, code: str, message: str = "", details: dict | None = None):
        super().__init__(f"{status} {code}" + (f": {message}" if message else ""))
        self.status = status
        self.code = code
        self.message = message
        self.details = details or {}


@dataclass(frozen=True)
class Booking:
    """One booked pass: the booking's reference, the traveller, the pass product, its
    first day of validity (YYYY-MM-DD) and the price charged, in cents."""

    reference: str
    traveller_id: str
    pass_code: str
    starts_on: str
    price_cents: int

    @classmethod
    def from_json(cls, obj: dict) -> "Booking":
        return cls(reference=obj["reference"], traveller_id=obj["traveller_id"], pass_code=obj["pass_code"],
                   starts_on=obj["starts_on"], price_cents=obj["price"]["amount_cents"])


@dataclass
class Client:
    base_url: str
    api_key: str
    timeout: float = 10.0
    user_agent: str = field(default="orvelline-py/0.4")

    def __post_init__(self):
        self.base_url = self.base_url.rstrip("/")

    def _request(self, method: str, path: str, body: dict | None = None,
                 headers: dict[str, str] | None = None) -> dict:
        """One request; the decoded JSON body of a 2xx answer, else OrvellineError."""
        data = json.dumps(body).encode() if body is not None else None
        req = urllib.request.Request(f"{self.base_url}{path}", data=data, method=method, headers={
            "Authorization": f"Bearer {self.api_key}",
            "Accept": "application/json",
            "Content-Type": "application/json",
            "User-Agent": self.user_agent,
            **(headers or {}),
        })
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            raise _api_error(e.code, e.read()) from None
        except urllib.error.URLError as e:
            raise OrvellineError(0, "transport", str(e.reason)) from None
        try:
            return json.loads(raw)
        except ValueError:
            raise OrvellineError(200, "bad_json", f"{path} did not answer JSON") from None

    def book_pass(self, traveller_id: str, pass_code: str, starts_on: str) -> Booking:
        """Book pass ``pass_code`` for ``traveller_id``, valid from ``starts_on``
        (YYYY-MM-DD), and return the booking."""
        body = {"traveller_id": traveller_id, "pass_code": pass_code, "starts_on": starts_on}
        out = self._request("POST", "/v2/pass-bookings", body)
        return Booking.from_json(out["booking"])


def _api_error(status: int, raw: bytes) -> OrvellineError:
    """An error response as OrvellineError: the code, message and details of the API's
    {"error": {...}} body, or a generic code when the body is not that shape."""
    try:
        err = json.loads(raw)["error"]
        return OrvellineError(status, err["code"], err.get("message", ""), err.get("details"))
    except (ValueError, KeyError, TypeError):
        return OrvellineError(status, "http_error", raw[:200].decode("utf-8", "replace"))
