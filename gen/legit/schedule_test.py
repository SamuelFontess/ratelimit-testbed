import ipaddress
import json
import math
import os
import random
import statistics
import subprocess
import sys
import time
from pathlib import Path

import pytest
from hypothesis import given, settings
from hypothesis import strategies as st

import schedule

SCRIPT = Path(__file__).with_name("schedule.py")
SEEDS = st.integers(min_value=0, max_value=2**32)
SCENARIOS = st.sampled_from(schedule.SCENARIOS)

LAM = 40.0
N_CLIENTS = 20
DUR = 100.0
SIGMAS = 5

ATTACKER_V4 = ipaddress.ip_network("10.66.0.0/16")
CONTROL_V4 = ipaddress.ip_network("10.67.0.0/16")
ATTACKER_V6 = ipaddress.ip_network("fd12:3456:789a::/48")
CONTROL_V6 = ipaddress.ip_network("fd12:3456:789b::/48")
S2A_V6_64 = ipaddress.ip_network("fd12:3456:789a:1::/64")
S3_ATTACKER_24 = ipaddress.ip_network("10.66.0.0/24")


def make(seed, scenario="s1", **kw):
    return schedule.gen(seed, LAM, N_CLIENTS, DUR, scenario, **kw)


def by_group(sched):
    groups = {}
    for c in sched["clients"]:
        groups.setdefault(c["group"], []).append(ipaddress.ip_address(c["src"]))
    return groups


def within(addrs, net):
    return all(a in net for a in addrs)


def merged_gaps(events):
    return [b["t"] - a["t"] for a, b in zip(events, events[1:])]


def per_client_gaps(events):
    per_client = {}
    for e in events:
        per_client.setdefault(e["cid"], []).append(e["t"])
    gaps = []
    for times in per_client.values():
        gaps += [b - a for a, b in zip([0.0] + times, times)]
    return gaps


def coefficient_of_variation(gaps):
    return statistics.pstdev(gaps) / statistics.fmean(gaps)


@settings(max_examples=40, deadline=None)
@given(seed=SEEDS, scenario=SCENARIOS)
def test_same_seed_same_schedule(seed, scenario):
    assert make(seed, scenario) == make(seed, scenario)


@settings(max_examples=40, deadline=None)
@given(a=SEEDS, b=SEEDS, scenario=SCENARIOS)
def test_different_seeds_different_events(a, b, scenario):
    if a != b:
        assert make(a, scenario)["events"] != make(b, scenario)["events"]


@settings(max_examples=40, deadline=None)
@given(seed=SEEDS, scenario=SCENARIOS)
def test_events_sorted_within_duration_and_known_clients(seed, scenario):
    sched = make(seed, scenario)
    times = [e["t"] for e in sched["events"]]
    assert times == sorted(times)
    assert all(0 <= t < DUR for t in times)
    cids = {c["cid"] for c in sched["clients"]}
    assert {e["cid"] for e in sched["events"]} <= cids


@settings(max_examples=25, deadline=None)
@given(seed=SEEDS)
def test_interarrival_means(seed):
    events = make(seed)["events"]
    expected_events = LAM * DUR
    tol = SIGMAS / math.sqrt(expected_events)

    gaps = merged_gaps(events)
    assert abs(sum(gaps) / len(gaps) - 1 / LAM) <= tol / LAM

    client_gaps = per_client_gaps(events)
    assert abs(sum(client_gaps) / len(client_gaps) - N_CLIENTS / LAM) <= tol * N_CLIENTS / LAM


@settings(max_examples=25, deadline=None)
@given(seed=SEEDS)
def test_interarrivals_are_exponential_by_coefficient_of_variation(seed):
    events = make(seed)["events"]
    tol = SIGMAS / math.sqrt(LAM * DUR)
    for gaps in (merged_gaps(events), per_client_gaps(events)):
        assert abs(coefficient_of_variation(gaps) - 1.0) <= tol


@pytest.mark.parametrize("scenario", schedule.SCENARIOS)
def test_schedule_ignores_clock_and_global_randomness(monkeypatch, scenario):
    expected = make(7, scenario)

    def forbidden(name):
        def raiser(*args, **kwargs):
            raise AssertionError(f"{name} nao pode influenciar a agenda")

        return raiser

    for module, names in (
        (time, ("time", "time_ns", "monotonic", "monotonic_ns", "perf_counter", "perf_counter_ns")),
        (os, ("urandom",)),
        (random, ("random", "seed", "getrandbits", "_urandom")),
    ):
        for name in names:
            monkeypatch.setattr(module, name, forbidden(f"{module.__name__}.{name}"))
    monkeypatch.setattr(random, "SystemRandom", forbidden("random.SystemRandom"))

    assert make(7, scenario) == expected


@settings(max_examples=25, deadline=None)
@given(seed=SEEDS)
def test_s1_groups_and_addresses(seed):
    sched = make(seed, "s1")
    groups = by_group(sched)
    assert set(groups) == {"control", "same_ip"}
    assert within(groups["control"], CONTROL_V4)
    assert within(groups["same_ip"], ATTACKER_V4)
    assert len(groups["control"]) == max(1, round(schedule.DEFAULT_CONTROL_FRACTION * N_CLIENTS))
    assert len({c["src"] for c in sched["clients"]}) == N_CLIENTS


@settings(max_examples=25, deadline=None)
@given(seed=SEEDS)
def test_s2a_groups_and_addresses(seed):
    sched = make(seed, "s2a")
    groups = by_group(sched)
    assert set(groups) == {"control", "same_prefix"}
    assert within(groups["control"], CONTROL_V6)
    assert within(groups["same_prefix"], S2A_V6_64)
    assert len({c["src"] for c in sched["clients"]}) == N_CLIENTS


@settings(max_examples=25, deadline=None)
@given(seed=SEEDS)
def test_s3_default_has_no_colocated_and_distinct_24s(seed):
    sched = make(seed, "s3")
    groups = by_group(sched)
    assert set(groups) == {"control"}
    assert all(a in ATTACKER_V4 or a in CONTROL_V4 for a in groups["control"])
    assert not any(a in S3_ATTACKER_24 for a in groups["control"])
    prefixes = {ipaddress.ip_network(f"{a}/24", strict=False) for a in groups["control"]}
    assert len(prefixes) == N_CLIENTS


@settings(max_examples=25, deadline=None)
@given(seed=SEEDS, fraction=st.sampled_from([0.25, 0.5, 1.0]))
def test_s3_colocated_clients_share_the_attacker_24(seed, fraction):
    sched = make(seed, "s3", colocate_fraction=fraction)
    groups = by_group(sched)
    n_control = max(1, round(schedule.DEFAULT_CONTROL_FRACTION * N_CLIENTS))
    assert len(groups["same_prefix"]) == round(fraction * (N_CLIENTS - n_control))
    assert within(groups["same_prefix"], S3_ATTACKER_24)
    assert len({c["src"] for c in sched["clients"]}) == N_CLIENTS


def test_control_group_has_at_least_one_client():
    sched = schedule.gen(1, LAM, 5, DUR, "s1", control_fraction=0.0)
    assert [c["group"] for c in sched["clients"]].count("control") == 1


@pytest.mark.parametrize(
    "kw",
    [
        {"scenario": "s9"},
        {"lam": 0},
        {"n_clients": 0},
        {"dur": 0},
        {"control_fraction": 1.5},
        {"colocate_fraction": -0.1},
    ],
)
def test_invalid_arguments_rejected(kw):
    args = {"seed": 1, "lam": LAM, "n_clients": N_CLIENTS, "dur": DUR, "scenario": "s1"} | kw
    with pytest.raises(ValueError):
        schedule.gen(**args)


def test_cli_writes_the_same_schedule_as_gen(tmp_path):
    out = tmp_path / "agenda.json"
    subprocess.run(
        [sys.executable, str(SCRIPT), "--seed", "7", "--scenario", "s2a", "--lam", str(LAM),
         "--clients", str(N_CLIENTS), "--dur", str(DUR), "--out", str(out)],
        check=True,
    )
    assert json.loads(out.read_text(encoding="utf-8")) == make(7, "s2a")
