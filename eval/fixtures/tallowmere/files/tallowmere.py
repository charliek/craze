"""A small client for the Tallowmere parcel-locker API (version 2).

Standard library only. Every API error -- a status outside 2xx, or a body that is not
JSON -- is raised as TallowmereError.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass

SIZES = ("S", "M", "L")


class TallowmereError(Exception):
    """The API answered with an error, or with something that is not JSON."""


@dataclass(frozen=True)
class Reservation:
    """A compartment kept for one parcel: the reservation's id, the locker and the
    compartment in it, the code the recipient types at the locker, and when the
    reservation lapses (an ISO 8601 timestamp, as the API gives it)."""

    id: str
    locker: str
    compartment: str
    code: str
    expires_at: str

    @classmethod
    def from_json(cls, obj: dict) -> "Reservation":
        return cls(id=obj["reservation_id"], locker=obj["locker_id"], compartment=obj["compartment"],
                   code=obj["pickup_code"], expires_at=obj["expires_at"])


class Client:
    def __init__(self, base_url: str, api_key: str, timeout: float = 10.0):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def _request(self, method: str, path: str, body: dict | None = None) -> dict:
        headers = {"Authorization": f"Token {self.api_key}", "Accept": "application/json"}
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(self.base_url + path, data=data, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            raise TallowmereError(f"HTTP {e.code} for {method} {path}") from None
        except urllib.error.URLError as e:
            raise TallowmereError(f"cannot reach the API: {e.reason}") from None
        try:
            return json.loads(raw)
        except ValueError:
            raise TallowmereError(f"{method} {path} did not answer JSON") from None

    def reserve(self, locker: str, size: str, parcel_ref: str, ttl_hours: int = 72) -> Reservation:
        """Reserve a compartment of ``size`` (S, M or L) at ``locker`` for the parcel
        ``parcel_ref``, kept for ``ttl_hours`` hours."""
        if size not in SIZES:
            raise ValueError(f"size must be one of {', '.join(SIZES)}, not {size!r}")
        body = {"size": size, "parcel_ref": parcel_ref, "ttl_hours": ttl_hours}
        obj = self._request("POST", f"/v2/lockers/{urllib.parse.quote(locker, safe='')}/reservations", body)
        return Reservation.from_json(obj)

    def cancel(self, reservation_id: str) -> None:
        """Give a reserved compartment back before its parcel is dropped off."""
        self._request("DELETE", f"/v2/reservations/{urllib.parse.quote(reservation_id, safe='')}")
