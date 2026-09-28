"""The persisted spend ledger: reservations before forwarding, reconciliation after
(plan 029 §3.1.7).

The ledger file is append-only JSONL, fsync'd after every line, guarded by an
exclusive ``fcntl`` lock so two crazeeval processes never both think they have
headroom. The file is the source of truth: every admission first reads whatever
other processes appended since the last look.

Two line kinds share an ``id``:

- ``reserve`` -- written before a request is forwarded, holding ``reservation``;
- ``settle`` -- written when the request ends, holding the actual ``cost`` from
  the reported usage, or ``usage: null`` when none arrived (a disconnect, an error),
  in which case the reservation stands as the charge.

A request's charge is its settled cost, else its reservation (settled without
usage, or not settled at all -- in flight, or orphaned by a crash). Admission
refuses when (charges so far) + (this reservation) would pass the cap.
"""

from __future__ import annotations

import fcntl
import json
import os
import statistics
import threading
import time
import uuid
from contextlib import contextmanager
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from crazeeval.pricing import Usage

DEFAULT_CAP = 95.0
DEFAULT_RUN_CAP = 3.0
# A run's bound on concurrent exposure, independent of the per-run cap above: at most this
# many of its reservations may be open (unsettled) at once.
MAX_OPEN_RESERVATIONS_PER_RUN = 4

BUDGET_STOP = "budget-stop"
BUDGET_CAPPED = "budget-capped"


@dataclass(frozen=True)
class Reservation:
    id: str
    run_id: str
    harness: str
    model: str
    provider: str
    amount: float
    list_amount: float


@dataclass(frozen=True)
class Admission:
    ok: bool
    reason: str | None
    reservation: Reservation | None
    total_before: float
    run_total_before: float


class Ledger:
    def __init__(
        self,
        path: Path | str,
        cap: float = DEFAULT_CAP,
        run_cap: float = DEFAULT_RUN_CAP,
        scrub: Callable[[str], str] | None = None,
    ):
        self.path = Path(path)
        self.cap = float(cap)
        self.run_cap = float(run_cap)
        self._scrub = scrub or (lambda s: s)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = open(self.path, "a+b")
        self._lock = threading.Lock()
        self._offset = 0
        self._entries: dict[str, dict] = {}
        self.torn_lines = 0
        with self._locked():
            self._sync()

    # -- file handling -------------------------------------------------------

    def close(self) -> None:
        try:
            self._fh.close()
        except OSError:
            pass

    @contextmanager
    def _locked(self):
        with self._lock:
            fcntl.flock(self._fh.fileno(), fcntl.LOCK_EX)
            try:
                yield
            finally:
                fcntl.flock(self._fh.fileno(), fcntl.LOCK_UN)

    def _sync(self) -> None:
        self._fh.seek(self._offset)
        data = self._fh.read()
        end = data.rfind(b"\n")
        if end < 0:
            return
        chunk = data[: end + 1]
        self._offset += len(chunk)
        for line in chunk.splitlines():
            if not line.strip():
                continue
            try:
                rec = json.loads(line)
            except ValueError:
                self.torn_lines += 1
                continue
            self._apply(rec)

    def _apply(self, rec: dict) -> None:
        rid = rec.get("id")
        if not rid:
            return
        e = self._entries.setdefault(rid, {"reserve": None, "settle": None})
        if rec.get("kind") == "reserve":
            e["reserve"] = rec
        elif rec.get("kind") == "settle":
            e["settle"] = rec

    def _append(self, rec: dict) -> None:
        line = self._scrub(json.dumps(rec, sort_keys=True)) + "\n"
        end = self._fh.seek(0, os.SEEK_END)
        if end > 0:
            # A writer that died mid-line must not swallow this line into its own.
            self._fh.seek(end - 1)
            if self._fh.read(1) != b"\n":
                line = "\n" + line
        self._fh.write(line.encode())
        self._fh.flush()
        os.fsync(self._fh.fileno())

    # -- accounting ------------------------------------------------------------

    @staticmethod
    def _charge(e: dict) -> float:
        s, r = e["settle"], e["reserve"]
        if s is not None:
            return float(s.get("charged", 0.0))
        if r is not None:
            return float(r.get("reservation", 0.0))
        return 0.0

    @staticmethod
    def _field(e: dict, name: str):
        for part in ("reserve", "settle"):
            if e[part] is not None and name in e[part]:
                return e[part][name]
        return None

    def _totals(self, run_id: str) -> tuple[float, float, float, int]:
        """Charges so far: everyone's, ``run_id``'s, ``run_id``'s settled (actually spent)
        part, and ``run_id``'s count of open (unsettled) reservations."""
        total = run_total = run_settled = 0.0
        run_open = 0
        for e in self._entries.values():
            c = self._charge(e)
            total += c
            if self._field(e, "run_id") == run_id:
                run_total += c
                if e["settle"] is not None:
                    run_settled += c
                else:
                    run_open += 1
        return total, run_total, run_settled, run_open

    def totals(self) -> dict:
        with self._locked():
            self._sync()
            committed = reserved = list_eq = 0.0
            open_n = 0
            for e in self._entries.values():
                if e["settle"] is not None:
                    committed += self._charge(e)
                    list_eq += float(e["settle"].get("list_cost") or 0.0)
                else:
                    reserved += self._charge(e)
                    open_n += 1
            return {
                "committed": round(committed, 6),
                "reserved": round(reserved, 6),
                "total": round(committed + reserved, 6),
                "list_equivalent": round(list_eq, 6),
                "open_reservations": open_n,
                "requests": len(self._entries),
                "cap": self.cap,
            }

    def per_run_costs(self, model: str) -> dict[str, float]:
        """Charged dollars per run id for runs on ``model``."""
        with self._locked():
            self._sync()
            out: dict[str, float] = {}
            for e in self._entries.values():
                if self._field(e, "model") != model:
                    continue
                rid = self._field(e, "run_id") or "?"
                out[rid] = out.get(rid, 0.0) + self._charge(e)
            return out

    def run_estimate(self, model: str, prior: float) -> float:
        """Median cost per run of ``model`` so far, else the pinned prior."""
        costs = list(self.per_run_costs(model).values())
        return statistics.median(costs) if costs else prior

    def admit(
        self,
        *,
        run_id: str,
        harness: str,
        model: str,
        provider: str,
        amount: float,
        list_amount: float = 0.0,
    ) -> Admission:
        """Reserve ``amount`` under the lock, or refuse (budget-stop / budget-capped)."""
        with self._locked():
            self._sync()
            total, run_total, run_settled, run_open = self._totals(run_id)
            if total + amount > self.cap:
                return Admission(False, BUDGET_STOP, None, total, run_total)
            # The per-run cap is a runaway guard on what a run has actually spent. It is not checked against this
            # request's reservation: a request with no output limit reserves the model's whole output ceiling
            # (about $6 on kimi-k3), which would cap a run that has spent almost nothing. The global cap above still
            # counts every reservation, at an upper bound on its actual cost under the providers' documented
            # billing (input tokens <= request bytes, output tokens <= the reserved limit) -- not an estimate --
            # so the global cap stays a hard bound on settled spend. A run's concurrent exposure while it has
            # settled little is instead bounded by MAX_OPEN_RESERVATIONS_PER_RUN below: a single worst-case
            # reservation is still admitted, but a run cannot stack many of them before any of them settle.
            if run_settled >= self.run_cap:
                return Admission(False, BUDGET_CAPPED, None, total, run_total)
            if run_open >= MAX_OPEN_RESERVATIONS_PER_RUN:
                return Admission(False, BUDGET_CAPPED, None, total, run_total)
            res = Reservation(
                id=uuid.uuid4().hex,
                run_id=run_id,
                harness=harness,
                model=model,
                provider=provider,
                amount=float(amount),
                list_amount=float(list_amount),
            )
            self._append(
                {
                    "kind": "reserve",
                    "id": res.id,
                    "ts": time.time(),
                    "run_id": run_id,
                    "harness": harness,
                    "model": model,
                    "provider": provider,
                    "reservation": res.amount,
                    "list_reservation": res.list_amount,
                }
            )
            self._sync()
            return Admission(True, None, res, total, run_total)

    def settle(
        self,
        res: Reservation,
        usage: Usage | None,
        cost: float | None,
        list_cost: float | None,
        status: int | None = None,
        note: str | None = None,
    ) -> float:
        """Reconcile a reservation. Without usage the reservation stays the charge."""
        charged = float(cost) if (usage is not None and cost is not None) else res.amount
        with self._locked():
            self._append(
                {
                    "kind": "settle",
                    "id": res.id,
                    "ts": time.time(),
                    "run_id": res.run_id,
                    "harness": res.harness,
                    "model": res.model,
                    "provider": res.provider,
                    "reservation": res.amount,
                    "usage": usage.to_dict() if usage is not None else None,
                    "cost": float(cost) if cost is not None else None,
                    "list_cost": float(list_cost) if list_cost is not None else None,
                    "charged": charged,
                    "status": status,
                    "note": note,
                }
            )
            self._sync()
        return charged
