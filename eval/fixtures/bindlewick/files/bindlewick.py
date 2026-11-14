"""A small client for the Bindlewick Circulation API.

Standard library only. Every API error -- a status outside 2xx, or a body that is not
JSON -- is raised as BindlewickError; a renewal the library refuses because the patron
has renewed the item as often as allowed is raised as RenewalRefused.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass

API_VERSION = "v1"


class BindlewickError(Exception):
    """The API answered with an error, or with something that is not JSON."""


class RenewalRefused(BindlewickError):
    """The patron has renewed this item as many times as the library allows."""


@dataclass(frozen=True)
class Checkout:
    """One item a patron has borrowed: the checkout's id, the barcode on the copy, the
    title, and the day it is due back (YYYY-MM-DD, the branch's calendar day)."""

    id: str
    barcode: str
    title: str
    due: str

    @classmethod
    def from_json(cls, obj: dict) -> "Checkout":
        return cls(id=obj["checkout_id"], barcode=obj["barcode"], title=obj["title"], due=obj["due_date"])


class Client:
    def __init__(self, base_url: str, api_key: str, branch: str, timeout: float = 10.0):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.branch = branch
        self.timeout = timeout

    def _request(self, method: str, path: str, body: dict | None = None) -> dict:
        """``method`` ``path`` (under the API version) with an optional JSON ``body``,
        for this client's branch; the decoded JSON answer."""
        query = urllib.parse.urlencode({"branch": self.branch})
        url = f"{self.base_url}/{API_VERSION}{path}?{query}"
        headers = {"Authorization": f"Bearer {self.api_key}", "Accept": "application/json"}
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                raw = resp.read()
        except urllib.error.HTTPError as e:
            code = _error_code(e.read())
            if e.code == 403 and code == "renew_limit":
                raise RenewalRefused(f"{path}: renewal limit reached") from None
            raise BindlewickError(f"HTTP {e.code} for {path} ({code or 'no code'})") from None
        except urllib.error.URLError as e:
            raise BindlewickError(f"cannot reach the API: {e.reason}") from None
        try:
            return json.loads(raw)
        except ValueError:
            raise BindlewickError(f"{path} did not answer JSON") from None

    def checkout(self, patron_id: str, barcode: str) -> Checkout:
        """Lend the copy with ``barcode`` to the patron ``patron_id``."""
        obj = self._request("POST", "/checkouts", {"patron_id": patron_id, "barcode": barcode})
        return Checkout.from_json(obj)

    def checkouts(self, patron_id: str) -> list[Checkout]:
        """Everything the patron ``patron_id`` has out, in the order the API lists it."""
        obj = self._request("GET", f"/patrons/{urllib.parse.quote(patron_id, safe='')}/checkouts")
        return [Checkout.from_json(o) for o in obj["checkouts"]]

    def renew(self, checkout_id: str) -> Checkout:
        """Renew the checkout ``checkout_id``; the checkout with its new due day."""
        obj = self._request("POST", f"/checkouts/{urllib.parse.quote(checkout_id, safe='')}/renew")
        return Checkout.from_json(obj)


def _error_code(raw: bytes) -> str | None:
    """The ``error.code`` of an error body, or None when it has none."""
    try:
        return json.loads(raw)["error"]["code"]
    except (ValueError, KeyError, TypeError):
        return None
