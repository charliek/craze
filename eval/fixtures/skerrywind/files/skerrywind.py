"""A small uploader for the Skerrywind station telemetry API (version 4).

Standard library only. Every API error -- a status outside 2xx that the API does not
document as a success, or a body that is not JSON -- is raised as SkerrywindError.
"""

from __future__ import annotations

import json
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass


class SkerrywindError(Exception):
    """The API answered with an error, or with something that is not JSON."""


@dataclass(frozen=True)
class Reading:
    """One line of the gateway's log: when the sensors were read (an RFC 3339 UTC
    timestamp), the air temperature in degrees Celsius, the station-level pressure in
    hectopascals and the mean wind speed in metres per second."""

    when: str
    temp_c: float
    pressure_hpa: float
    wind_ms: float

    @classmethod
    def from_log_line(cls, line: str) -> "Reading":
        """Parse one line of the gateway's log: ``<when> <temp_c> <pressure_hpa> <wind_ms>``."""
        when, temp, pressure, wind = line.split()
        return cls(when, float(temp), float(pressure), float(wind))


class Client:
    def __init__(self, base_url: str, station_id: str, signing_secret: str, timeout: float = 10.0):
        self.base_url = base_url.rstrip("/")
        self.station_id = station_id
        self.signing_secret = signing_secret
        self.timeout = timeout

    def upload(self, readings: list[Reading], batch_id: str) -> str:
        """Upload ``readings`` for this client's station and return the receipt the API
        issues for them. ``batch_id`` is the gateway's own id for this upload; it sends
        the same id again when it retries."""
        raise NotImplementedError("upload is not written yet")
