import csv
import json
import re

import pytest
from hypothesis import given, settings, strategies as st

from consolidate import (
    COLUMNS,
    OUTCOMES,
    PFT_COLUMNS,
    consolidate,
    main,
    pft,
    read_jsonl,
    write_results,
)

WINDOW = (10.0, 20.0)
GROUPS = ("same_ip", "same_prefix", "control")
LIMIT_COLUMNS = ("pft", "pft_threshold_2s", "pft_threshold_1s")
NUMERIC = (int, float)
DURATION = 20.0


def tx(cid, t_plan, group="control", latency=0.1, **overrides):
    return {
        "t_start": t_plan,
        "t_end": t_plan + latency,
        "t_plan": t_plan,
        "late_ms": 0.0,
        "status": 200,
        "cid": cid,
        "outcome": "ok",
        "group": group,
        **overrides,
    }


C1_EVENTS = [
    (0, 2.0, "same_ip"),
    (0, 12.0, "same_ip"),
    (0, 25.0, "same_ip"),
    (1, 9.95, "same_prefix"),
    (1, 15.0, "same_prefix"),
    (1, 16.0, "same_prefix"),
    (2, 5.0, "control"),
    (2, 19.99, "control"),
    (3, 13.0, "same_ip"),
    (3, 16.0, "same_ip"),
    (3, 17.0, "control"),
    (4, 11.0, "control"),
    (4, 12.0, "control"),
]
C4_CHANGES = {
    (0, 2.0): {"outcome": "fail_refused", "status": 429},
    (0, 12.0): {"outcome": "fail_refused", "status": 429, "latency": 0.001},
    (0, 25.0): {"outcome": "fail_total", "status": 0},
    (1, 9.95): {"latency": 3.0},
    (1, 15.0): {"latency": 5.0},
    (2, 5.0): {"outcome": "fail_refused", "status": 0},
    (2, 19.99): {"t_start": 24.0, "t_end": 24.1},
    (3, 13.0): {"outcome": "fail_total", "status": 0},
    (3, 16.0): {"latency": 1.5},
    (4, 11.0): {"outcome": "fail_refused", "status": 0},
}


def synthetic_pair():
    c1 = [tx(cid, t, group=group) for cid, t, group in C1_EVENTS]
    c4 = [
        tx(cid, t, group=group, **C4_CHANGES.get((cid, t), {}))
        for cid, t, group in reversed(C1_EVENTS)
    ]
    not_in_c1 = [tx(9, 15.0, outcome="fail_total", status=0), tx(0, 18.0, outcome="fail_total")]
    return c1, c4 + not_in_c1


def test_hand_counted_pft_is_four_out_of_ten():
    c1, c4 = synthetic_pair()

    result = pft(c1, c4, WINDOW)

    assert result["pft"] == pytest.approx(4 / 10)
    assert result["pft_same_ip"] == pytest.approx(2 / 3)
    assert result["pft_same_prefix"] == pytest.approx(1 / 3)
    assert result["pft_control"] == pytest.approx(1 / 4)


def test_threshold_sensitivity_recomputes_from_elapsed():
    c1, c4 = synthetic_pair()
    failed_at_4s = 4
    also_failed_at_2s = 1
    also_failed_at_1s = 1

    result = pft(c1, c4, WINDOW)

    assert result["pft_threshold_2s"] == pytest.approx((failed_at_4s + also_failed_at_2s) / 10)
    assert result["pft_threshold_1s"] == pytest.approx(
        (failed_at_4s + also_failed_at_2s + also_failed_at_1s) / 10
    )


def test_failures_outside_the_window_and_transactions_absent_from_c1_do_not_count():
    c1, c4 = synthetic_pair()
    before_attack = [tx(7, 3.0, outcome="fail_total", status=0)]
    after_attack = [tx(7, 30.0, outcome="fail_refused", status= 429)]

    for outside in (before_attack, after_attack):
        assert pft(c1, c4 + outside, WINDOW) == pft(c1, c4, WINDOW)
    assert pft(c1 + before_attack + after_attack, c4, WINDOW)["pft"] == pytest.approx(0.4)


def test_denominator_comes_from_the_c1_schedule_not_from_observed_times():
    c1, c4 = synthetic_pair()
    shifted = [dict(r, t_start=r["t_start"] + 100, t_end=r["t_end"] + 100) for r in c4]

    assert pft(c1, shifted, WINDOW) == pft(c1, c4, WINDOW)


def test_window_edges():
    def all_failed(records):
        return [dict(r, outcome="fail_total", status=0) for r in records]

    straddling = [tx(0, 9.5, latency=1.0)]
    ends_at_start = [tx(0, 9.5, latency=0.5)]
    ends_before = [tx(0, 9.0, latency=0.5)]
    starts_at_end = [tx(0, 20.0)]

    assert pft(straddling, straddling, WINDOW)["pft"] == 0
    assert pft(straddling, all_failed(straddling), WINDOW)["pft"] == 1
    for outside in (ends_at_start, ends_before, starts_at_end):
        assert pft(outside, all_failed(outside), WINDOW)["pft"] is None


def test_401_of_k3_is_a_failure_and_stays_out_of_percentiles(tmp_path):
    c1 = [tx(0, 12.0), tx(0, 13.0)]
    k3 = [
        tx(0, 12.0, latency=0.2),
        tx(0, 13.0, latency=0.001, status=401, outcome="fail_refused"),
    ]
    write_run(tmp_path, "c1", meta(cond="c1", key="none"), c1)
    write_run(tmp_path, "c4", meta(key="k3"), k3)

    rows = consolidate(tmp_path)

    assert rows[1]["pft"] == pytest.approx(1 / 2)
    assert (rows[1]["p95"], rows[1]["p99"]) == pytest.approx((0.2, 0.2))


EVENT = st.tuples(
    st.integers(min_value=0, max_value=4),
    st.floats(min_value=-5, max_value=30, allow_nan=False),
    st.sampled_from(sorted(OUTCOMES)),
    st.floats(min_value=0, max_value=70, allow_nan=False),
)
EVENTS = st.lists(EVENT, min_size=1, max_size=30, unique_by=lambda e: (e[0], e[1]))


def group_of(cid):
    return GROUPS[cid % 3]


@settings(max_examples=200, deadline=None)
@given(EVENTS, st.data())
def test_property_4_pft_is_bounded_monotonic_and_zero_for_c1(events, data):
    window = (5.0, 20.0)
    flips = data.draw(st.lists(st.booleans(), min_size=len(events), max_size=len(events)))
    c1 = [tx(cid, t, group=group_of(cid)) for cid, t, _, _ in events]
    run = [
        tx(cid, t, group=group_of(cid), latency=latency, outcome=outcome)
        for cid, t, outcome, latency in events
    ]
    worse = [
        dict(r, outcome="fail_total", status=0) if flip else r for r, flip in zip(run, flips)
    ]
    all_failed = [dict(r, outcome="fail_total", status=0) for r in run]

    result = pft(c1, run, window)
    monotonic = pft(c1, worse, window)
    identical = pft(c1, c1, window)
    saturated = pft(c1, all_failed, window)

    for column, value in result.items():
        if value is None:
            assert all(table[column] is None for table in (monotonic, identical, saturated))
            continue
        assert 0 <= value <= 1
        assert value <= monotonic[column] <= 1
        assert identical[column] == 0
        assert saturated[column] == 1
    limits = [result[column] for column in LIMIT_COLUMNS if result[column] is not None]
    assert limits == sorted(limits)


def meta(**overrides):
    return {
        "scenario": "s1",
        "key": "k1",
        "cond": "c4",
        "rate": 100,
        "burst": 100,
        "seed": 7,
        "queue_mode": "nodelay",
        "warmup": 10.0,
        "duration": DURATION,
        "status": "done",
        **overrides,
    }


def log_line(status, upstream="0.002"):
    return f"1700000000.000\t10.66.0.1\tk1\t{status}\t{upstream}\t{upstream}\tPASSED\tk\n"


def atk(t_start, outcome="ok", status=200):
    return {"t_start": t_start, "t_end": t_start + 0.01, "status": status, "outcome": outcome}


def write_run(root, name, meta_json, legit=(), attacker=(), log=()):
    run = root / name
    run.mkdir()
    (run / "meta.json").write_text(json.dumps(meta_json), encoding="utf-8")
    for filename, records in (
        ("legit.jsonl", legit),
        ("attacker.jsonl", attacker),
    ):
        (run / filename).write_text(
            "".join(json.dumps(r) + "\n" for r in records), encoding="utf-8"
        )
    (run / "nginx.access.log").write_text("".join(log), encoding="utf-8")
    return run


def rows_by_run(runs_dir):
    return {row["run"]: row for row in consolidate(runs_dir)}


def read_csv(path):
    with open(path, encoding="utf-8", newline="") as f:
        return list(csv.DictReader(f))


def test_measures_of_a_run(tmp_path):
    name = "s1__k1__c4__r100__burst100__seed7__nodelay"
    legit = [tx(0, 12.0), tx(1, 15.0), tx(2, 13.0, late_ms=60.0), tx(3, 3.0)]
    attacker = [
        atk(11, "fail_refused", 429),
        atk(12, "fail_refused", 0),
        atk(13, "fail_refused", 401),
        atk(14),
        atk(15),
        atk(5, "fail_refused", 429),
    ]
    log = [
        log_line(200),
        log_line(200),
        log_line(429, "-"),
        log_line(200, "-"),
        log_line(401, "-"),
    ]
    write_run(tmp_path, name, meta(cpu_pct=12.5, zone_states={"k1": 0.5}), legit, attacker, log)
    out = tmp_path / "sub" / "results.csv"

    write_results(consolidate(tmp_path), out)

    (row,) = read_csv(out)
    assert list(row) == list(COLUMNS)
    with open(out, encoding="utf-8", newline="") as f:
        header = next(csv.reader(f))
    assert header[header.index("pft") : header.index("goodput")] == [
        "pft",
        "pft_same_ip",
        "pft_same_prefix",
        "pft_control",
        "pft_threshold_1s",
        "pft_threshold_2s",
    ]
    assert row["run"] == name
    assert (row["scenario"], row["key"], row["cond"], row["seed"], row["queue_mode"]) == (
        "s1",
        "k1",
        "c4",
        "7",
        "nodelay",
    )
    assert float(row["containment_rps_refused"]) == pytest.approx(3 / 20)
    assert float(row["containment_rps_upstream"]) == pytest.approx(2 / 20)
    assert float(row["goodput"]) == pytest.approx(3 / 20)
    assert float(row["p95"]) == pytest.approx(0.1)
    assert row["late_events"] == "1"
    assert row["cpu_pct"] == "12.5"
    assert row["zone_states"] == '{"k1": 0.5}'


def test_cpu_and_zone_states_absent_are_empty_not_invented(tmp_path):
    write_run(tmp_path, "c4", meta(), [tx(0, 12.0)])

    (row,) = consolidate(tmp_path)

    assert (row["cpu_pct"], row["zone_states"]) == (None, None)


def test_command_line_writes_results_and_missing_c1_leaves_pft_empty(tmp_path):
    write_run(tmp_path, "c4", meta(), [tx(0, 12.0)])
    out = tmp_path / "results" / "results.csv"

    main([str(tmp_path), "--out", str(out)])

    (row,) = read_csv(out)
    assert row["computable"] == "False"
    assert all(row[column] == "" for column in PFT_COLUMNS)


def test_missing_runs_directory_is_an_error(tmp_path):
    with pytest.raises(FileNotFoundError):
        consolidate(tmp_path / "nao_existe")


def test_attacker_outcome_moves_containment_but_never_the_pft(tmp_path):
    c1 = [tx(0, 12.0), tx(0, 13.0)]
    c4 = [tx(0, 12.0), tx(0, 13.0, outcome="fail_refused", status=429)]
    write_run(tmp_path, "c1", meta(cond="c1", key="none"), c1)
    write_run(tmp_path, "contido", meta(), c4, [atk(11, "fail_refused", 429)] * 3)
    write_run(tmp_path, "passou", meta(), c4, [atk(11)] * 3)

    rows = rows_by_run(tmp_path)

    assert rows["contido"]["pft"] == rows["passou"]["pft"] == pytest.approx(1 / 2)
    assert rows["contido"]["containment_rps_refused"] == pytest.approx(3 / 20)
    assert rows["passou"]["containment_rps_refused"] == 0


@pytest.mark.parametrize(
    "baseline",
    [
        meta(cond="c1", key="none", seed=8),
        meta(cond="c1", key="none", scenario="s2a"),
        meta(cond="c1", key="none", status="failed"),
    ],
)
def test_c1_of_another_seed_scenario_or_failed_does_not_pair(tmp_path, baseline):
    legit = [tx(0, 12.0)]
    write_run(tmp_path, "baseline", baseline, legit)
    write_run(tmp_path, "c4", meta(), legit)

    row = rows_by_run(tmp_path)["c4"]

    assert row["computable"] is False
    assert all(row[column] is None for column in PFT_COLUMNS)


def test_failed_run_has_no_row(tmp_path):
    write_run(tmp_path, "a", meta(status="failed"))
    write_run(tmp_path, "b", meta(seed=8))

    assert list(rows_by_run(tmp_path)) == ["b"]


def test_c1_row_reports_its_own_failure_fraction(tmp_path):
    c1 = [tx(0, 12.0), tx(0, 13.0), tx(0, 14.0), tx(0, 15.0, outcome="fail_total", status=0)]
    write_run(tmp_path, "c1", meta(cond="c1", key="none"), c1)

    row = rows_by_run(tmp_path)["c1"]

    assert row["computable"] is True
    assert row["pft"] == pytest.approx(1 / 4)


def test_429_fails_in_pft_but_is_out_of_percentiles_in_nodelay(tmp_path):
    c1 = [tx(0, 12.0, latency=0.1), tx(0, 13.0, latency=0.2), tx(0, 14.0, latency=0.3)]
    c1 += [tx(0, 15.0, latency=0.4), tx(0, 17.0, latency=0.001)]
    refused = tx(0, 17.0, latency=0.001, status=429, outcome="fail_refused")
    c4 = c1[:-1] + [refused]
    write_run(tmp_path, "c1", meta(cond="c1", key="none"), c1)
    write_run(tmp_path, "delay", meta(queue_mode="delay"), c4)
    write_run(tmp_path, "nodelay", meta(), c4)

    rows = rows_by_run(tmp_path)

    assert rows["nodelay"]["pft"] == pytest.approx(1 / 5)
    assert (rows["nodelay"]["p95"], rows["nodelay"]["p99"]) == pytest.approx((0.385, 0.397))
    assert rows["delay"]["pft"] == pytest.approx(1 / 5)
    assert (rows["delay"]["p95"], rows["delay"]["p99"]) == pytest.approx((0.38, 0.396))


def test_truncated_jsonl_cites_file_and_line(tmp_path):
    path = tmp_path / "legit.jsonl"
    path.write_text('{"t_start": 1}\n{"t_start": 2, "t_end": ', encoding="utf-8")

    with pytest.raises(ValueError, match=re.escape(f"{path}:2: JSONL truncado")):
        read_jsonl(path, {"t_start": NUMERIC})


@pytest.mark.parametrize(
    "line, field",
    [
        ('{"t_end": 1.1}\n', "t_start"),
        ('{"t_start": "1"}\n', "t_start"),
        ('{"t_start": NaN}\n', "t_start"),
        ("[1, 2]\n", "t_start"),
        ('{"t_start": 1, "outcome": "maybe"}\n', "outcome"),
    ],
)
def test_jsonl_with_missing_or_invalid_field_cites_file_line_and_field(tmp_path, line, field):
    path = tmp_path / "legit.jsonl"
    path.write_text('{"t_start": 0, "outcome": "ok"}\n' + line, encoding="utf-8")

    with pytest.raises(ValueError, match=re.escape(f"{path}:2: campo {field!r}")):
        read_jsonl(path, {"t_start": NUMERIC, "outcome": OUTCOMES})


def test_log_with_fewer_fields_cites_file_and_line(tmp_path):
    write_run(tmp_path, "c4", meta(), log=[log_line(200), "1\t2\t3\n"])
    expected = f"{tmp_path}/c4/nginx.access.log:2: esperava 8 campos"

    with pytest.raises(ValueError, match=re.escape(expected)):
        consolidate(tmp_path)


@pytest.mark.parametrize(
    "override, complaint",
    [
        ({"duration": None}, "campo 'duration' ausente"),
        ({"warmup": None}, "campo 'warmup' ausente"),
        ({"warmup": "10"}, "campo 'warmup' deve ser numero"),
        ({"warmup": -1}, "warmup deve ser maior ou igual a zero"),
        ({"cpu_pct": float("nan")}, "campo 'cpu_pct' deve ser numero"),
        ({"duration": 0}, "duration deve ser maior que zero"),
        ({"queue_mode": "maybe"}, "campo 'queue_mode' fora de"),
        ({"status": "running"}, "campo 'status' fora de"),
    ],
)
def test_meta_without_window_or_known_state_is_rejected(tmp_path, override, complaint):
    meta_json = {k: v for k, v in meta(**override).items() if v is not None}
    write_run(tmp_path, "c4", meta_json)

    with pytest.raises(ValueError, match=re.escape(f"{tmp_path}/c4/meta.json: {complaint}")):
        consolidate(tmp_path)


def test_run_missing_a_c1_transaction_is_an_error(tmp_path):
    c1 = [tx(0, 12.0), tx(0, 13.0)]
    write_run(tmp_path, "c1", meta(cond="c1", key="none"), c1)
    write_run(tmp_path, "c4", meta(), c1[:1])

    with pytest.raises(ValueError, match="corrida sem a transacao cid=0 ordem=1"):
        consolidate(tmp_path)


def test_c1_and_run_with_different_schedules_is_an_error(tmp_path):
    write_run(tmp_path, "c1", meta(cond="c1", key="none"), [tx(0, 12.0), tx(0, 13.0)])
    write_run(tmp_path, "c4", meta(), [tx(0, 12.0), tx(0, 14.0)])

    with pytest.raises(ValueError, match="agenda diferente entre C1 e a corrida em cid=0 ordem=1"):
        consolidate(tmp_path)


def test_two_c1_for_the_same_scenario_and_seed_is_an_error(tmp_path):
    write_run(tmp_path, "c1_nodelay", meta(cond="c1", key="none"), [tx(0, 12.0)])
    write_run(tmp_path, "c1_delay", meta(cond="c1", key="none", queue_mode="delay"), [tx(0, 12.0)])

    with pytest.raises(ValueError, match="mais de um C1 para cenario s1 seed 7"):
        consolidate(tmp_path)


def test_zone_states_out_of_range_are_rejected(tmp_path):
    write_run(tmp_path, "c4", meta(zone_states={"k1": 1.5}))
    expected = f"{tmp_path}/c4/meta.json: zone_states"

    with pytest.raises(ValueError, match=re.escape(expected)):
        consolidate(tmp_path)
