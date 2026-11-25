import argparse
import csv
import json
import math
import sys
from pathlib import Path

import numpy as np

KEY_COLUMNS = ("scenario", "key", "cond", "rate", "burst", "seed", "queue_mode")
META_FILE = "meta.json"
BASELINE_COND = "c1"
DONE = "done"
RUN_STATUSES = (DONE, "failed")
DELAY_MODE = "delay"
QUEUE_MODES = (DELAY_MODE, "nodelay")
GROUPS = ("same_ip", "same_prefix", "control")
OUTCOMES = frozenset({"ok", "fail_partial", "fail_total", "fail_refused"})
REFUSED = "fail_refused"
SUCCESS = "ok"
NUMERIC = (int, float)
PARTIAL_LIMIT_S = 4.0
SENSITIVITY_LIMITS_S = {"pft_threshold_1s": 1.0, "pft_threshold_2s": 2.0}
PFT_COLUMNS = ("pft", *("pft_" + g for g in GROUPS), *SENSITIVITY_LIMITS_S)
PERCENTILES = {"p95": 95, "p99": 99}
LOG_FIELDS = 8
UPSTREAM_TIME_COLUMN = 5
NO_UPSTREAM = "-"
LATE_MS_LIMIT = 50.0
LEGIT_FIELDS = {
    "t_start": NUMERIC,
    "t_end": NUMERIC,
    "t_plan": NUMERIC,
    "late_ms": NUMERIC,
    "cid": NUMERIC,
    "outcome": OUTCOMES,
    "group": frozenset(GROUPS),
}
ATTACKER_FIELDS = {"t_start": NUMERIC, "outcome": OUTCOMES}
COLUMNS = (
    "run",
    *KEY_COLUMNS,
    "containment_rps_refused",
    "containment_rps_upstream",
    *PFT_COLUMNS,
    "goodput",
    "p95",
    "p99",
    "cpu_pct",
    "zone_states",
    "late_events",
    "computable",
)
DEFAULT_OUT = Path("results/results.csv")


def _valid(value, rule):
    if isinstance(rule, frozenset):
        return isinstance(value, str) and value in rule
    return type(value) in rule and math.isfinite(value)


def read_jsonl(path, fields):
    records = []
    with open(path, encoding="utf-8") as lines:
        for lineno, line in enumerate(lines, 1):
            try:
                record = json.loads(line)
            except ValueError as e:
                raise ValueError(f"{path}:{lineno}: JSONL truncado ou invalido: {e}") from None
            for name, rule in fields.items():
                value = record.get(name) if isinstance(record, dict) else None
                if not _valid(value, rule):
                    raise ValueError(
                        f"{path}:{lineno}: campo {name!r} ausente ou invalido: {value!r}"
                    )
            records.append(record)
    return records


def _reached_target(log_path):
    count = 0
    with open(log_path, encoding="utf-8") as lines:
        for lineno, line in enumerate(lines, 1):
            fields = line.rstrip("\n").split("\t")
            if len(fields) != LOG_FIELDS:
                raise ValueError(
                    f"{log_path}:{lineno}: esperava {LOG_FIELDS} campos, veio {len(fields)}"
                )
            count += fields[UPSTREAM_TIME_COLUMN] != NO_UPSTREAM
    return count


def _overlaps(record, window):
    start, end = window
    return record["t_start"] < end and record["t_end"] > start


def _failed(record, partial_limit):
    return record["outcome"] != SUCCESS or record["t_end"] - record["t_start"] >= partial_limit


def _by_transaction(records):
    per_client = {}
    for record in records:
        per_client.setdefault(record["cid"], []).append(record)
    index = {}
    for client_records in per_client.values():
        for ordem, record in enumerate(sorted(client_records, key=lambda r: r["t_plan"])):
            index[(record["cid"], ordem)] = record
    return index


def _pairs(c1, run, window):
    observed = _by_transaction(run)
    pairs = []
    for (cid, ordem), reference in _by_transaction(c1).items():
        if not _overlaps(reference, window):
            continue
        match = observed.get((cid, ordem))
        if match is None:
            raise ValueError(f"corrida sem a transacao cid={cid} ordem={ordem} presente em C1")
        if match["t_plan"] != reference["t_plan"]:
            raise ValueError(
                f"agenda diferente entre C1 e a corrida em cid={cid} ordem={ordem}: "
                f"t_plan {reference['t_plan']} e {match['t_plan']}"
            )
        pairs.append((reference, match))
    return pairs


def _failure_ratio(pairs, partial_limit):
    if not pairs:
        return None
    return sum(_failed(match, partial_limit) for _, match in pairs) / len(pairs)


def pft(c1, run, window):
    pairs = _pairs(c1, run, window)
    limits = {"pft": PARTIAL_LIMIT_S, **SENSITIVITY_LIMITS_S}
    result = {column: _failure_ratio(pairs, limit) for column, limit in limits.items()}
    for group in GROUPS:
        in_group = [pair for pair in pairs if pair[0]["group"] == group]
        result[f"pft_{group}"] = _failure_ratio(in_group, PARTIAL_LIMIT_S)
    return result


def _latency_percentiles(legit, queue_mode):
    accepted = [r for r in legit if queue_mode == DELAY_MODE or r["outcome"] != REFUSED]
    if not accepted:
        return dict.fromkeys(PERCENTILES)
    latencies = [r["t_end"] - r["t_start"] for r in accepted]
    values = np.percentile(latencies, list(PERCENTILES.values()))
    return dict(zip(PERCENTILES, map(float, values)))


def _measures(run_dir, meta, window, legit):
    start, end = window
    window_legit = [r for r in legit if _overlaps(r, window)]
    duration = meta["duration"]
    attacker = read_jsonl(run_dir / "attacker.jsonl", ATTACKER_FIELDS)
    refused = sum(start <= r["t_start"] < end and r["outcome"] == REFUSED for r in attacker)
    return {
        "containment_rps_refused": refused / duration,
        "containment_rps_upstream": _reached_target(run_dir / "nginx.access.log") / duration,
        "goodput": sum(not _failed(r, PARTIAL_LIMIT_S) for r in window_legit) / duration,
        **_latency_percentiles(window_legit, meta["queue_mode"]),
        "late_events": sum(r["late_ms"] > LATE_MS_LIMIT for r in window_legit),
    }


def _zone_states(value, meta_path):
    if value is None:
        return None
    if not isinstance(value, dict) or not all(
        _valid(v, NUMERIC) and 0 <= v <= 1 for v in value.values()
    ):
        raise ValueError(
            f"{meta_path}: zone_states deve mapear zona para fracao em [0, 1], veio {value!r}"
        )
    return json.dumps(value, sort_keys=True)


def _row(run_dir, meta, c1):
    window = (meta["warmup"], meta["warmup"] + meta["duration"])
    legit = read_jsonl(run_dir / "legit.jsonl", LEGIT_FIELDS)
    return {
        "run": run_dir.name,
        **{column: meta[column] for column in KEY_COLUMNS},
        **_measures(run_dir, meta, window, legit),
        **(pft(c1, legit, window) if c1 is not None else dict.fromkeys(PFT_COLUMNS)),
        "cpu_pct": meta.get("cpu_pct"),
        "zone_states": _zone_states(meta.get("zone_states"), run_dir / META_FILE),
    }


def _meta(path):
    try:
        meta = json.loads(path.read_text(encoding="utf-8"))
    except ValueError as e:
        raise ValueError(f"{path}: JSON invalido: {e}") from None
    for name in (*KEY_COLUMNS, "status", "warmup", "duration"):
        if not isinstance(meta, dict) or name not in meta:
            raise ValueError(f"{path}: campo {name!r} ausente")
    for name in ("warmup", "duration"):
        if not _valid(meta[name], NUMERIC):
            raise ValueError(f"{path}: campo {name!r} deve ser numero, veio {meta[name]!r}")
    if meta["warmup"] < 0:
        raise ValueError(f"{path}: warmup deve ser maior ou igual a zero, veio {meta['warmup']}")
    if meta["duration"] <= 0:
        raise ValueError(f"{path}: duration deve ser maior que zero, veio {meta['duration']}")
    for name, allowed in (("status", RUN_STATUSES), ("queue_mode", QUEUE_MODES)):
        if meta[name] not in allowed:
            raise ValueError(f"{path}: campo {name!r} fora de {allowed}, veio {meta[name]!r}")
    if "cpu_pct" in meta and not _valid(meta["cpu_pct"], NUMERIC):
        raise ValueError(f"{path}: campo 'cpu_pct' deve ser numero, veio {meta['cpu_pct']!r}")
    return meta


def consolidate(runs_dir):
    runs_dir = Path(runs_dir)
    run_dirs = sorted(d for d in runs_dir.iterdir() if (d / META_FILE).is_file())
    metas = {run_dir: _meta(run_dir / META_FILE) for run_dir in run_dirs}
    done = {run_dir: meta for run_dir, meta in metas.items() if meta["status"] == DONE}
    baselines = {}
    for run_dir, meta in done.items():
        if meta["cond"] != BASELINE_COND:
            continue
        pair = (meta["scenario"], meta["seed"])
        if pair in baselines:
            raise ValueError(
                f"mais de um C1 para cenario {pair[0]} seed {pair[1]}: "
                f"{baselines[pair].name} e {run_dir.name}"
            )
        baselines[pair] = run_dir
    c1_legit = {
        pair: read_jsonl(run_dir / "legit.jsonl", LEGIT_FIELDS)
        for pair, run_dir in baselines.items()
    }
    rows = []
    for run_dir, meta in done.items():
        c1 = c1_legit.get((meta["scenario"], meta["seed"]))
        rows.append({**_row(run_dir, meta, c1), "computable": c1 is not None})
    return rows


def write_results(rows, path):
    path.parent.mkdir(parents=True, exist_ok=True)
    with open(path, "w", encoding="utf-8", newline="") as out:
        writer = csv.DictWriter(out, fieldnames=COLUMNS, lineterminator="\n")
        writer.writeheader()
        writer.writerows(rows)


def main(argv):
    parser = argparse.ArgumentParser(
        description="Consolida corridas no formato C-NAME em results.csv"
    )
    parser.add_argument("runs", type=Path, help="diretorio com um subdiretorio por corrida")
    parser.add_argument(
        "--out", type=Path, default=DEFAULT_OUT, help=f"CSV de saida (padrao: {DEFAULT_OUT})"
    )
    args = parser.parse_args(argv)
    write_results(consolidate(args.runs), args.out)


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (ValueError, OSError) as e:
        sys.exit(f"consolidate.py: {e}")
