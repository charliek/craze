"""A small client for the Quayside harbour-operations API (version 3).

Standard library only. Every API error -- a status outside 2xx, or a body that is not
JSON -- is raised as QuaysideError.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass


class QuaysideError(Exception):
    """The API answered with an error, or with something that is not JSON."""


@dataclass(frozen=True)
class Slot:
    """One booked berth slot: the berth, the vessel's name, and when it starts and
    ends (ISO 8601 timestamps, as the API gives them)."""

    berth: str
    vessel: str
    start: str
    end: str

    @classmethod
    def from_json(cls, obj: dict) -> "Slot":
        return cls(berth=obj["berth"], vessel=obj["vessel"], start=obj["start"], end=obj["end"])


class Client:
    def __init__(self, base_url: str, api_key: str, timeout: float = 10.0):
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def berth_schedule(self, harbor: str, day: str) -> list[Slot]:
        """Every berth slot booked at ``harbor`` on ``day`` (YYYY-MM-DD), in the order
        the API lists them."""
        raise NotImplementedError("berth_schedule is not written yet")
