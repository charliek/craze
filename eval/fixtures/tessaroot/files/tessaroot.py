"""A small client for the Tessaroot sensor API (version 2).

Standard library only. Every API error -- a status outside 2xx, or a body that is not
JSON -- is raised as TessarootError, with the HTTP status and Tessaroot's error code
when the body names one.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass


class TessarootError(Exception):
    """The API answered with an error, or with something that is not JSON."""

    def __init__(self, message: str, status: int | None = None, code: str | None = None):
        super().__init__(message)
        self.status = status
        self.code = code


@dataclass(frozen=True)
class Reading:
    """One sensor reading: when it was taken (an ISO 8601 timestamp, as the API gives
    it), its value and unit, and whether it is provisional (may still be revised)."""

    at: str
    value: float
    unit: str
    provisional: bool = False

    @classmethod
    def from_json(cls, obj: dict) -> "Reading":
        return cls(at=obj["at"], value=float(obj["value"]), unit=obj["unit"])


def error_from_body(status: int, body: bytes) -> TessarootError:
    """The TessarootError for an error response: its code and message from the JSON
    body (``{"error": {"code": ..., "message": ...}}``) when there is one."""
    try:
        err = json.loads(body).get("error") or {}
    except (ValueError, AttributeError):
        err = {}
    if not isinstance(err, dict):
        err = {}
    code = err.get("code")
    message = err.get("message") or "request failed"
    return TessarootError(f"{code or 'HTTP ' + str(status)}: {message}", status=status, code=code)


class Client:
    def __init__(self, base_url: str, token: str, timeout: float = 10.0):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def _get(self, path: str, params: dict[str, str]) -> dict:
        """GET ``path`` with ``params``, authenticated with the bearer token."""
        url = f"{self.base_url}{path}?{urllib.parse.urlencode(params)}"
        req = urllib.request.Request(url, headers={"Authorization": f"Bearer {self.token}",
                                                   "Accept": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                body = resp.read()
        except urllib.error.HTTPError as e:
            raise error_from_body(e.code, e.read()) from None
        except urllib.error.URLError as e:
            raise TessarootError(f"cannot reach the API: {e.reason}") from None
        try:
            return json.loads(body)
        except ValueError:
            raise TessarootError(f"{path} did not answer JSON") from None

    def readings(self, house: str, sensor: str, start: str, end: str) -> list[Reading]:
        """Every reading ``sensor`` in greenhouse ``house`` took from ``start`` up to
        ``end`` (ISO 8601 timestamps), oldest first."""
        path = (f"/v2/houses/{urllib.parse.quote(house, safe='')}"
                f"/sensors/{urllib.parse.quote(sensor, safe='')}/readings")
        page = self._get(path, {"from": start, "to": end})
        return [Reading.from_json(obj) for obj in page.get("readings") or []]
