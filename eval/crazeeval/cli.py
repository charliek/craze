"""``uv run crazeeval <command>``: run, validate, proxy, snapshot-config, probe, keyscan, ledger,
judge-pair, judge-batch, calibrate, judge-hash, report, rescore, captures."""

from __future__ import annotations

import argparse
import asyncio
import json
import signal
import sys
import tempfile
import time
from pathlib import Path

from crazeeval import paths


def _stamp() -> str:
    return time.strftime("%Y%m%d-%H%M%S")


def _csv(s: str | None) -> list[str]:
    return [x.strip() for x in (s or "").split(",") if x.strip()]


def _opt_path(s: str | None) -> Path | None:
    return Path(s) if s else None


def _ledger_path(a) -> Path:
    return _opt_path(a.ledger) or paths.DEFAULT_LEDGER


# -- run -------------------------------------------------------------------------------


def cmd_run(a) -> int:
    from crazeeval import keys as keymod
    from crazeeval.batch import BatchConfig, run_batch
    from crazeeval.config import HARNESSES, load_models, load_snapshot
    from crazeeval.pricing import load_prices
    from crazeeval.sandbox import Toolchains, bwrap_available
    from crazeeval.tasks import load_tasks, select_tasks

    if a.first_rep < 1:
        print(f"--first-rep must be >= 1, got {a.first_rep}", file=sys.stderr)
        return 2
    harnesses = _csv(a.harness) or list(HARNESSES)
    for h in harnesses:
        if h not in HARNESSES:
            print(f"unknown harness {h!r}", file=sys.stderr)
            return 2
    craze_bin = None
    if "craze" in harnesses:
        if not a.craze_bin:
            print("--craze-bin is required when craze is among the harnesses (never /usr/local/bin/craze)", file=sys.stderr)
            return 2
        craze_bin = Path(a.craze_bin).resolve()
        if str(craze_bin).startswith("/usr/local/bin"):
            print("refusing /usr/local/bin/craze: use a build of the branch", file=sys.stderr)
            return 2
        if not craze_bin.is_file():
            print(f"{craze_bin}: no such file", file=sys.stderr)
            return 2
    if not bwrap_available():
        print("bubblewrap is not usable here; every run must be sandboxed", file=sys.stderr)
        return 2
    all_models = load_models()
    model_keys = _csv(a.model) or list(all_models)
    models = []
    for k in model_keys:
        if k not in all_models:
            print(f"unknown model {k!r} (see eval/models.toml)", file=sys.stderr)
            return 2
        models.append(all_models[k])
    tasks = select_tasks(load_tasks(), a.tasks)
    if not tasks:
        print("no tasks selected", file=sys.stderr)
        return 2
    snap = load_snapshot(_opt_path(a.config))
    prices = load_prices()
    for em in models:
        if em.wire_model not in prices:
            print(f"{em.key}: wire model {em.wire_model!r} has no price in eval/prices.toml", file=sys.stderr)
            return 2
    providers, keyring = keymod.load_providers()
    for em in models:
        if em.provider not in providers or keyring.key_for(em.provider) is None:
            print(f"{em.key}: provider {em.provider!r} has no route or no key", file=sys.stderr)
            return 2
        snap_base = snap.model(em.key)["craze_provider"].get("base_url", "").rstrip("/")
        if snap_base and snap_base != providers[em.provider].base_url:
            print(f"{em.key}: the owner's base URL for {em.provider} differs from the config snapshot's; re-snapshot",
                  file=sys.stderr)
            return 2
    label = a.label or "batch"
    out = Path(a.out) if a.out else paths.DEFAULT_RUNS_DIR / f"{label}-{_stamp()}"
    caps = {}
    if a.cap_zai is not None:
        caps["zai-coding-plan"] = a.cap_zai
    tc = Toolchains.resolve()
    seed = None
    if "opencode" in harnesses:
        from crazeeval.seed import ensure_opencode_seed

        seed = ensure_opencode_seed(tc)
    if a.resume and not a.out:
        print("--resume needs --out (the batch directory to reopen)", file=sys.stderr)
        return 2
    cfg = BatchConfig(
        harnesses=harnesses,
        models=models,
        tasks=tasks,
        reps=a.reps,
        out=out,
        label=label,
        snap=snap,
        first_rep=a.first_rep,
        craze_bin=craze_bin,
        parallel=a.parallel,
        caps=caps,
        cap_other=a.cap_other,
        ledger_path=_ledger_path(a),
        budget_cap=a.budget_cap,
        run_cap=a.run_cap,
        timeout_s=a.timeout,
        resume=a.resume,
        opencode_seed=seed,
    )
    if a.first_rep == 1:
        rep_desc = f"{a.reps} rep(s)"
    elif a.reps == 1:
        rep_desc = f"rep {a.first_rep}"
    else:
        rep_desc = f"reps {a.first_rep}-{a.first_rep + a.reps - 1}"
    print(f"batch {label}: {len(harnesses)} harness(es) x {len(models)} model(s) x {len(tasks)} task(s) x {rep_desc} -> {out}")
    from crazeeval.batch import BatchDirError

    try:
        summary = asyncio.run(run_batch(cfg, providers, keyring, prices, tc))
    except BatchDirError as e:
        print(str(e), file=sys.stderr)
        return 2
    print(json.dumps({k: summary.get(k) for k in ("runs_planned", "statuses", "objective_pass", "estimate", "ledger_after", "refused", "stop_reason")}, indent=2))
    # Non-zero when a requested task failed validation, nothing could run, or the
    # estimate was refused (review r1-c1 finding 19).
    return 1 if summary.get("refused") else 0


# -- validate ----------------------------------------------------------------------------


def cmd_validate(a) -> int:
    from crazeeval.sandbox import Toolchains, bwrap_available
    from crazeeval.tasks import load_tasks, select_tasks
    from crazeeval.validate import validate_task

    tasks = select_tasks(load_tasks(), a.tasks or "split:all")
    tc = Toolchains.resolve() if bwrap_available() else None
    ok_all = True
    report = []
    for t in tasks:
        v = asyncio.run(validate_task(t, tc, keep=_opt_path(a.keep)))
        report.append(v.to_dict())
        ok_all &= v.ok
        print(f"{'PASS' if v.ok else 'FAIL'}  {t.id}  ({t.category}, {t.split})")
        if v.error:
            print(f"      error: {v.error}")
        for c in v.controls:
            mark = "ok " if c.ok else "BAD"
            print(f"      {mark} {c.name}: expected {'pass' if c.expected else 'fail'}, got "
                  f"{'pass' if c.got else ('fail' if c.got is not None else 'n/a')}" + (f"  [{c.detail[:160]}]" if (not c.ok and c.detail) else ""))
    if a.json:
        Path(a.json).write_text(json.dumps(report, indent=2) + "\n")
    print(f"{sum(1 for r in report if r['ok'])}/{len(report)} tasks valid")
    return 0 if ok_all and report else 1


# -- proxy -------------------------------------------------------------------------------


def cmd_proxy(a) -> int:
    from crazeeval import keys as keymod
    from crazeeval.config import load_models, load_snapshot
    from crazeeval.homes import craze_home_multi
    from crazeeval.ledger import Ledger
    from crazeeval.pricing import load_prices
    from crazeeval.proxy import Proxy

    all_models = load_models()
    ems = [all_models[k] for k in _csv(a.model)]
    if not ems:
        print("--model is required (one or more eval model keys)", file=sys.stderr)
        return 2
    providers, keyring = keymod.load_providers()
    prices = load_prices()
    out = Path(a.out) if a.out else paths.DEFAULT_RUNS_DIR / f"proxy-{a.run_id}-{_stamp()}"
    out.mkdir(parents=True, exist_ok=True)
    ledger = Ledger(_ledger_path(a), cap=a.budget_cap, run_cap=a.run_cap,
                    scrub=keyring.scrub)

    snap = load_snapshot(_opt_path(a.config))

    async def main():
        proxy = Proxy(providers, keyring, ledger, prices, port=a.port, refusal_log=out / "proxy-refusals.jsonl",
                      max_output=snap.max_output(ems))
        await proxy.start()
        route = proxy.register_run(a.run_id, a.harness, {em.wire_model for em in ems}, out / "capture.jsonl")
        urls = {em.provider: proxy.base_url(route, em.provider) for em in ems}
        info = {"port": proxy.port, "run_id": a.run_id, "models": [em.wire_model for em in ems], "base_urls": urls,
                "dummy_key": paths.DUMMY_KEY, "capture": str(out / "capture.jsonl")}
        if a.craze_home:
            craze_home_multi(Path(a.craze_home), ems, snap, urls)
            info["craze_home"] = str(Path(a.craze_home))
        (out / "proxy.json").write_text(json.dumps(info, indent=2) + "\n")
        print(json.dumps(info, indent=2), flush=True)
        stop = asyncio.Event()
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, stop.set)
        await stop.wait()
        summary = await proxy.end_run(route)
        await proxy.stop()
        (out / "proxy-summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        print(json.dumps(summary, indent=2))

    asyncio.run(main())
    ledger.close()
    return 0


# -- snapshot-config -------------------------------------------------------------------------


def cmd_snapshot_config(a) -> int:
    from crazeeval import snapshot as snapmod
    from crazeeval.config import load_models, snapshot_hash

    models = load_models()
    snap = snapmod.build_snapshot(models)
    catalog_bytes = Path(a.models_dev).read_bytes() if a.models_dev else snapmod.fetch_models_dev()
    catalog = json.loads(catalog_bytes)
    ids = snapmod.check_opencode_ids(models, catalog)
    for k, v in ids.items():
        snap["models"][k]["opencode_check"] = v
    missing = [k for k, v in ids.items() if not v["present"]]
    if missing:
        print(f"opencode ids missing from the models.dev catalog: {missing}", file=sys.stderr)
        return 1
    if not a.no_variants:
        variants = opencode_variants(models, catalog_bytes)
        for k, v in variants.items():
            snap["models"][k]["opencode_variants"] = v
    d = snapmod.write_snapshot(snap, catalog_bytes, _opt_path(a.root))
    print(f"config snapshot: {d}")
    print(f"hash: {snapshot_hash(d)}")
    for k, m in snap["models"].items():
        print(f"  {k}: craze={m['eval']['craze']} gx={m['eval']['gx']} opencode={m['eval']['opencode']} "
              f"variants={m.get('opencode_variants')} codex={m['eval']['codex']}")
    return 0


def opencode_variants(models, catalog_bytes: bytes) -> dict:
    """Ask opencode itself (sandboxed, isolated config, the saved catalog) which
    variants it offers for each eval model."""
    from crazeeval.homes import opencode_env
    from crazeeval.sandbox import SandboxSpec, Toolchains, run_sandboxed_sync

    tc = Toolchains.resolve()
    catalog_inside = f"{paths.SANDBOX_CONFIG}/catalog.json"
    with tempfile.TemporaryDirectory(prefix="crazeeval-ocvariants-") as td:
        td = Path(td)
        (td / "ws").mkdir()
        home = td / "home"
        (home / ".xdg").mkdir(parents=True)
        (td / "catalog.json").write_bytes(catalog_bytes)
        provs = {}
        for em in models.values():
            pid = em.opencode.split("/", 1)[0]
            provs[pid] = {"options": {"apiKey": paths.DUMMY_KEY, "baseURL": "http://127.0.0.1:1/unused"}}
        (home / "opencode.json").write_text(json.dumps({"$schema": "https://opencode.ai/config.json", "provider": provs}))
        spec = SandboxSpec(workspace=td / "ws", ws_inside=f"{paths.SANDBOX_WORK}/ws", home=home, tools={"opencode"},
                           env=opencode_env(catalog_inside), network=False,
                           extra_ro=[(td / "catalog.json", catalog_inside)])
        r = run_sandboxed_sync(spec, tc, ["opencode", "models", "--verbose"], stdout=td / "out", stderr=td / "err", timeout=300)
        text = (td / "out").read_text(errors="replace")
    if r.exit_code != 0:
        print(f"warning: `opencode models --verbose` exited {r.exit_code}", file=sys.stderr)
    return {k: _variants_for(text, em.opencode) for k, em in models.items()}


def _variants_for(text: str, model_id: str) -> list[str] | None:
    lines = text.splitlines()
    for i, line in enumerate(lines):
        if line.strip() == model_id:
            buf = []
            for l in lines[i + 1 :]:
                buf.append(l)
                if l.startswith("}"):
                    break
            try:
                obj = json.loads("\n".join(buf))
                return sorted((obj.get("variants") or {}).keys())
            except ValueError:
                return None
    return None


# -- probe / keyscan / ledger -------------------------------------------------------------------


def cmd_probe(a) -> int:
    from crazeeval import keys as keymod
    from crazeeval.config import load_models, load_snapshot
    from crazeeval.probe import run_probe
    from crazeeval.sandbox import Toolchains

    snap = load_snapshot(_opt_path(a.config))
    em = load_models()[a.model]
    _, keyring = keymod.load_providers()
    out = Path(a.out) if a.out else paths.DEFAULT_RUNS_DIR / f"probe-{_stamp()}"
    craze_bin = Path(a.craze_bin).resolve() if a.craze_bin else None
    r = asyncio.run(run_probe(out, Toolchains.resolve(), keyring, snap, em, craze_bin))
    inside = r.get("inside") or {}
    print(json.dumps({
        "ok": r["ok"],
        "reachable_from_inside": inside.get("reachable"),
        "credential_like_names": inside.get("credential_like_names"),
        "env_value_is_a_key": r["env_value_is_a_key"],
        "files_with_key": r["key_scan"]["files_with_key"],
        "go_build": inside.get("go_build"),
        "which": inside.get("which"),
        "dns": inside.get("dns_api_meta_ai"),
        "egress": {k: inside.get(k) for k in ("egress_1.1.1.1:443", "egress_8.8.8.8:53")},
        "relay": inside.get("relay"),
        "egress_closed": r.get("egress_closed"),
        "pid1": inside.get("pid1"),
        "out": str(out),
    }, indent=2))
    return 0 if r["ok"] else 1


def cmd_keyscan(a) -> int:
    from crazeeval import keys as keymod

    _, keyring = keymod.load_providers()
    r = keymod.scan_tree(Path(a.dir), keyring)
    print(json.dumps(r, indent=2))
    return 1 if r["files_with_key"] else 0


def cmd_ledger(a) -> int:
    from crazeeval.ledger import Ledger

    led = Ledger(_ledger_path(a))
    print(json.dumps(led.totals(), indent=2))
    led.close()
    return 0


# -- judge / report / captures ------------------------------------------------------------------


def _seed(a) -> int:
    return int(a.seed) if a.seed is not None else int(time.strftime("%Y%m%d"))


def cmd_judge_hash(a) -> int:
    from crazeeval.judge import judge_hash
    from crazeeval.tasks import load_tasks

    print(judge_hash(load_tasks()))
    return 0


def cmd_judge_pair(a) -> int:
    """Judge two runs against each other (the judge smoke)."""
    from crazeeval.judge import Judge, Pair, judge_hash, judge_pair
    from crazeeval.packet import load_side
    from crazeeval.tasks import load_tasks

    tasks = load_tasks()
    x, y = load_side(Path(a.a)), load_side(Path(a.b))
    tid = a.task or x.result.get("task")
    if tid not in tasks:
        print(f"unknown task {tid!r}", file=sys.stderr)
        return 2
    task = tasks[tid]
    if task.split == "heldout" and not a.unseal:
        print(f"{tid} is a held-out task: its verdict is sealed; pass --unseal to judge it here", file=sys.stderr)
        return 2
    pair = Pair(task, x.result.get("model") or "?", x, y)
    t0 = time.monotonic()
    rec = asyncio.run(judge_pair(Judge(parallel=a.parallel), pair, a.judge, _seed(a), a.both_orders))
    rec["judge_hash"] = judge_hash(tasks)
    rec["wall_s_total"] = round(time.monotonic() - t0, 1)
    text = json.dumps(rec, indent=2, default=str)
    if a.out:
        Path(a.out).write_text(text + "\n")
    print(text)
    return 0 if rec["result"] is not None else 1


def cmd_judge_batch(a) -> int:
    from crazeeval.judge import Judge, judge_hash
    from crazeeval.judging import (
        CALIBRATION_FILE,
        CalibrationRefusal,
        enforce_calibration,
        find_runs,
        judge_batch,
        load_calibration,
        make_pairs,
    )
    from crazeeval.tasks import load_tasks

    tasks = load_tasks()
    batch = Path(a.batch)
    runs_x = find_runs(batch)
    runs_y = find_runs(Path(a.batch_y)) if a.batch_y else runs_x
    pairs = []
    for hy in _csv(a.y):
        pairs += make_pairs(runs_x, runs_y, a.x, hy, tasks, set(_csv(a.model)) or None, set(_csv(a.tasks)) or None)
    out = Path(a.out) if a.out else batch / "judging"
    mode = "success-bar" if a.success_bar else ("both" if a.both_orders else "single")
    # The calibration decisions are enforced (review r1-c2 §7): luna needs its agreement
    # gate, single-order judging needs the flip gate; an override is recorded.
    cal_path = Path(a.calibration) if a.calibration else batch / "calibration" / CALIBRATION_FILE
    cal = load_calibration(cal_path, judge_hash(tasks))
    try:
        mode, cal_record = enforce_calibration(a.judge, mode, cal, a.override_calibration)
    except CalibrationRefusal as e:
        print(f"refused: {e}", file=sys.stderr)
        return 2
    if cal_record["forced_both_orders"]:
        print(f"calibration ({cal['status']}, flip gate {cal.get('flip_gate')}): judging both orders")
    if a.override_calibration:
        print(f"calibration overridden: {a.override_calibration}")
    print(f"judge-batch: {len(pairs)} pairs ({mode}, judge {a.judge}) -> {out}")
    counts = asyncio.run(judge_batch(pairs, tasks, out, Judge(parallel=a.parallel), a.judge, mode, _seed(a),
                                     calibration=cal_record))
    print(json.dumps(counts))
    return 0 if not counts["no_verdict"] else 1


def cmd_calibrate(a) -> int:
    from crazeeval.judge import Judge
    from crazeeval.judging import calibrate, find_runs, make_pairs
    from crazeeval.tasks import load_tasks

    tasks = load_tasks()
    dev = {k: t for k, t in tasks.items() if t.split == "dev"}
    runs = find_runs(Path(a.batch), include_heldout=False)
    harnesses = sorted({r.harness for r in runs})
    pairs = []
    for i, hx in enumerate(harnesses):
        for hy in harnesses[i + 1:]:
            pairs += make_pairs(runs, runs, hx, hy, dev)
    out = Path(a.out) if a.out else Path(a.batch) / "calibration"
    summary = asyncio.run(calibrate(pairs, tasks, out, Judge(parallel=a.parallel), _seed(a), a.pairs, a.padding))
    print(json.dumps(summary, indent=2))
    return 0


def cmd_report(a) -> int:
    from crazeeval import report as rp
    from crazeeval.tasks import load_tasks

    tasks = load_tasks()
    batches = [Path(b) for b in a.batch]
    vfiles = rp.verdict_files([Path(v) for v in (a.verdicts or [])] or [b / "judging" for b in batches], a.unseal)
    verdicts = rp.load_verdicts(vfiles, a.unseal)
    # The best open harness is fixed from the baseline batch alone (§3.1.8): the first
    # --batch unless --baseline names it.
    baseline = Path(a.baseline) if a.baseline else batches[0]
    inp = rp.ReportInput(runs=rp.load_runs(batches, a.unseal), verdicts=verdicts, tasks=tasks, unseal=a.unseal,
                         batches=[str(b) for b in batches], verdict_sources=[str(v) for v in vfiles],
                         judge_hashes=[v.get("judge_hash") for v in verdicts if v.get("judge_hash")],
                         baseline_batch=baseline, baseline_runs=rp.load_runs([baseline], a.unseal))
    if a.compare:
        inp.compare_runs = rp.load_runs([Path(a.compare)], a.unseal)
        inp.compare_verdicts = rp.load_verdicts([Path(v) for v in (a.compare_verdicts or [])], a.unseal)
    rep = rp.build(inp)
    out = Path(a.out) if a.out else batches[-1] / ("report-unsealed" if a.unseal else "report")
    rp.write(rep, out)
    print(f"report: {out / 'report.md'}")
    for m, sb in rep["success_bar"].items():
        print(f"  {m}: {sb['outcome']} ({sb.get('reason')})")
    return 0


def cmd_rescore(a) -> int:
    from crazeeval.rescore import RescoreError, rescore

    try:
        summary = rescore(Path(a.batch), unseal=a.unseal, dry_run=a.dry_run)
    except RescoreError as e:
        print(str(e), file=sys.stderr)
        return 2
    print(json.dumps(summary, indent=2))
    return 0


def cmd_captures(a) -> int:
    from crazeeval import captures

    out = captures.write(Path(a.run), _opt_path(a.out), a.unseal)
    print(f"captures: {out / 'captures.md'}")
    return 0


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="crazeeval", description="craze native harness evaluation (plan 029)")
    sub = p.add_subparsers(dest="cmd", required=True)

    r = sub.add_parser("run", help="run a batch of sandboxed harness runs through the proxy")
    r.add_argument("--harness", help="comma list of craze,gx,opencode,codex (default: all)")
    r.add_argument("--model", help="comma list of eval model keys (default: all)")
    r.add_argument("--tasks", help="comma list of task ids and/or split:<dev|heldout|smoke|all>")
    r.add_argument("--reps", type=int, default=1)
    r.add_argument("--first-rep", type=int, default=1,
                    help="number reps from N (a later batch adding rep N to an earlier one)")
    r.add_argument("--craze-bin", help="the craze binary under test (required for craze)")
    r.add_argument("--label", default="batch")
    r.add_argument("--parallel", type=int, default=4)
    r.add_argument("--cap-zai", type=int, default=None, help="concurrent runs on Z.AI (default 2)")
    r.add_argument("--cap-other", type=int, default=3, help="concurrent runs per other provider (default 3)")
    r.add_argument("--out", help="batch directory (default: <plan>/eval-runs/<label>-<timestamp>)")
    r.add_argument("--ledger", help=f"ledger path (default {paths.DEFAULT_LEDGER})")
    r.add_argument("--budget-cap", type=float, default=95.0)
    r.add_argument("--run-cap", type=float, default=3.0)
    r.add_argument("--timeout", type=int, default=None, help="per-run timeout in seconds (default: the task's, 20 min)")
    r.add_argument("--config", help="config snapshot directory (default: CURRENT)")
    r.add_argument("--resume", action="store_true",
                   help="reopen --out (its batch.json must match) and run only what has no final result")
    r.set_defaults(fn=cmd_run)

    v = sub.add_parser("validate", help="run every task's category controls")
    v.add_argument("--tasks", help="comma list of task ids and/or split:<name> (default: all)")
    v.add_argument("--keep", help="keep the validation workspaces under this directory")
    v.add_argument("--json", help="also write the report here")
    v.set_defaults(fn=cmd_validate)

    x = sub.add_parser("proxy", help="run the recording proxy standalone (for the live smoke)")
    x.add_argument("--port", type=int, default=0)
    x.add_argument("--model", required=True, help="comma list of eval model keys the route allows")
    x.add_argument("--run-id", default="live")
    x.add_argument("--harness", default="manual")
    x.add_argument("--out")
    x.add_argument("--ledger")
    x.add_argument("--budget-cap", type=float, default=95.0)
    x.add_argument("--run-cap", type=float, default=3.0)
    x.add_argument("--craze-home", help="write a CRAZE_HOME here pointing at the proxy")
    x.add_argument("--config", help="config snapshot directory (default: CURRENT)")
    x.set_defaults(fn=cmd_proxy)

    s = sub.add_parser("snapshot-config", help="copy the target definitions (no secrets) into a versioned snapshot")
    s.add_argument("--root", help=f"snapshot root (default {paths.DEFAULT_CONFIG_ROOT})")
    s.add_argument("--models-dev", help="use this saved models.dev api.json instead of fetching")
    s.add_argument("--no-variants", action="store_true", help="skip asking opencode for its variants")
    s.set_defaults(fn=cmd_snapshot_config)

    pr = sub.add_parser("probe", help="prove the sandbox hides the owner's files and every key")
    pr.add_argument("--out")
    pr.add_argument("--craze-bin")
    pr.add_argument("--model", default="muse-spark-1.3-contributor")
    pr.add_argument("--config")
    pr.set_defaults(fn=cmd_probe)

    k = sub.add_parser("keyscan", help="grep a directory for every loaded key (prints counts and paths only)")
    k.add_argument("dir")
    k.set_defaults(fn=cmd_keyscan)

    lg = sub.add_parser("ledger", help="print the ledger totals")
    lg.add_argument("--ledger")
    lg.set_defaults(fn=cmd_ledger)

    jh = sub.add_parser("judge-hash", help="print the frozen judge fingerprint (instruction, schema, rubrics)")
    jh.set_defaults(fn=cmd_judge_hash)

    jp = sub.add_parser("judge-pair", help="judge two runs against each other (rep directories)")
    jp.add_argument("--a", required=True, help="the first run's rep directory")
    jp.add_argument("--b", required=True, help="the second run's rep directory")
    jp.add_argument("--task", help="task id (default: the first run's)")
    jp.add_argument("--judge", default="sol", help="sol, luna or astra (default sol)")
    jp.add_argument("--both-orders", action="store_true")
    jp.add_argument("--seed")
    jp.add_argument("--parallel", type=int, default=4)
    jp.add_argument("--unseal", action="store_true", help="allow a held-out task")
    jp.add_argument("--out")
    jp.set_defaults(fn=cmd_judge_pair)

    jb = sub.add_parser("judge-batch", help="judge every run pair between harnesses (held-out verdicts sealed)")
    jb.add_argument("--batch", required=True)
    jb.add_argument("--batch-y", help="take the y runs from this batch (compare two builds)")
    jb.add_argument("--x", default="craze", help="the x harness (default craze)")
    jb.add_argument("--y", default="gx,opencode,codex", help="comma list of y harnesses")
    jb.add_argument("--model", help="comma list of eval model keys (default: all)")
    jb.add_argument("--tasks", help="comma list of task ids (default: all)")
    jb.add_argument("--judge", default="sol")
    jb.add_argument("--both-orders", action="store_true")
    jb.add_argument("--success-bar", action="store_true", help="both orders by sol, astra on disagreement/low confidence")
    jb.add_argument("--seed")
    jb.add_argument("--parallel", type=int, default=4)
    jb.add_argument("--out", help="verdict directory (default <batch>/judging)")
    jb.add_argument("--calibration", help="the calibration decisions to enforce (default <batch>/calibration/calibration.json)")
    jb.add_argument("--override-calibration", metavar="REASON",
                    help="judge as asked despite the calibration decisions (the reason is recorded in every verdict)")
    jb.set_defaults(fn=cmd_judge_batch)

    ca = sub.add_parser("calibrate", help="judge calibration: flip rate, luna agreement, padding (§3.1.6)")
    ca.add_argument("--batch", required=True)
    ca.add_argument("--pairs", type=int, default=30)
    ca.add_argument("--padding", type=int, default=10)
    ca.add_argument("--seed")
    ca.add_argument("--parallel", type=int, default=4)
    ca.add_argument("--out")
    ca.set_defaults(fn=cmd_calibrate)

    rp_ = sub.add_parser("report", help="objective counts, win rates, the success bar (held-out sealed unless --unseal)")
    rp_.add_argument("--batch", action="append", required=True, help="a batch directory (repeatable; later wins)")
    rp_.add_argument("--verdicts", action="append", help="a verdict file or directory (repeatable; default <batch>/judging)")
    rp_.add_argument("--unseal", action="store_true")
    rp_.add_argument("--baseline", help="the baseline batch that fixes the best open harness (default: the first --batch)")
    rp_.add_argument("--compare", help="an earlier batch: compare its craze build with this one")
    rp_.add_argument("--compare-verdicts", action="append", help="verdicts of the new build vs the old (craze vs craze)")
    rp_.add_argument("--out")
    rp_.set_defaults(fn=cmd_report)

    rs = sub.add_parser("rescore", help="recompute a batch's diff and diff-derived checks from its manifests (X15)")
    rs.add_argument("--batch", required=True, help="a batch directory")
    rs.add_argument("--unseal", action="store_true", help="also rescore the held-out runs")
    rs.add_argument("--dry-run", action="store_true", help="compute and print; write nothing")
    rs.set_defaults(fn=cmd_rescore)

    cp = sub.add_parser("captures", help="the wire-capture report (§3.1.10) and AC-A8 fidelity checks")
    cp.add_argument("--run", required=True, help="a batch directory")
    cp.add_argument("--out")
    cp.add_argument("--unseal", action="store_true")
    cp.set_defaults(fn=cmd_captures)

    a = p.parse_args(argv)
    return a.fn(a)


if __name__ == "__main__":
    raise SystemExit(main())
