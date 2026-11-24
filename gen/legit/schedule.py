import argparse
import json
import random
import sys

SCENARIOS = ("s1", "s2a", "s3")
DEFAULT_CONTROL_FRACTION = 0.1
DEFAULT_COLOCATE_FRACTION = 0.0
MAX_THIRD_OCTET = 254
HOSTS_PER_24 = 254

GROUP_CONTROL = "control"
GROUP_SAME_IP = "same_ip"
GROUP_SAME_PREFIX = "same_prefix"


def _v4_flat(second_octet, index):
    return f"10.{second_octet}.{index // HOSTS_PER_24}.{1 + index % HOSTS_PER_24}"


def _v4_own_24(second_octet, third_octet):
    if third_octet > MAX_THIRD_OCTET:
        raise ValueError(f"s3 comporta no maximo {MAX_THIRD_OCTET} /24 por grupo")
    return f"10.{second_octet}.{third_octet}.1"


def _v4_attacker_24(index):
    if index >= HOSTS_PER_24:
        raise ValueError(f"s3 comporta no maximo {HOSTS_PER_24} clientes colocalizados")
    return f"10.66.0.{1 + index}"


def _v6(prefix, index):
    return f"{prefix}::{1 + index:x}"


def _scenario_sources(scenario):
    if scenario == "s1":
        return (
            lambda i: (_v4_flat(67, i), GROUP_CONTROL),
            lambda i: (_v4_flat(66, i), GROUP_SAME_IP),
            None,
        )
    if scenario == "s2a":
        return (
            lambda i: (_v6("fd12:3456:789b", i), GROUP_CONTROL),
            lambda i: (_v6("fd12:3456:789a:1", i), GROUP_SAME_PREFIX),
            None,
        )
    return (
        lambda i: (_v4_own_24(67, i), GROUP_CONTROL),
        lambda i: (_v4_own_24(66, 1 + i), GROUP_CONTROL),
        lambda i: (_v4_attacker_24(i), GROUP_SAME_PREFIX),
    )


def _clients(scenario, n_clients, control_fraction, colocate_fraction):
    control_src, rest_src, colocated_src = _scenario_sources(scenario)
    n_control = min(n_clients, max(1, round(control_fraction * n_clients)))
    n_colocated = round(colocate_fraction * (n_clients - n_control)) if colocated_src else 0
    clients = []
    for cid in range(n_clients):
        if cid < n_control:
            src, group = control_src(cid)
        elif cid < n_control + n_colocated:
            src, group = colocated_src(cid - n_control)
        else:
            src, group = rest_src(cid - n_control - n_colocated)
        clients.append({"cid": cid, "src": src, "group": group})
    return clients


def _events(rng, lam, n_clients, dur):
    per_client_rate = lam / n_clients
    events = []
    for cid in range(n_clients):
        t = rng.expovariate(per_client_rate)
        while t < dur:
            events.append({"t": t, "cid": cid})
            t += rng.expovariate(per_client_rate)
    events.sort(key=lambda e: (e["t"], e["cid"]))
    return events


def _validate(lam, n_clients, dur, scenario, control_fraction, colocate_fraction):
    if scenario not in SCENARIOS:
        raise ValueError(f"cenario invalido: {scenario!r}, esperado um de {SCENARIOS}")
    if lam <= 0 or dur <= 0 or n_clients < 1:
        raise ValueError("lam e dur devem ser positivos e n_clients >= 1")
    if not 0 <= control_fraction <= 1 or not 0 <= colocate_fraction <= 1:
        raise ValueError("control_fraction e colocate_fraction devem estar em [0, 1]")


def gen(
    seed,
    lam,
    n_clients,
    dur,
    scenario,
    control_fraction=DEFAULT_CONTROL_FRACTION,
    colocate_fraction=DEFAULT_COLOCATE_FRACTION,
):
    _validate(lam, n_clients, dur, scenario, control_fraction, colocate_fraction)
    rng = random.Random(seed)
    return {
        "clients": _clients(scenario, n_clients, control_fraction, colocate_fraction),
        "events": _events(rng, lam, n_clients, dur),
    }


def _parse_args(argv):
    p = argparse.ArgumentParser(description="Gera a agenda open-loop dos clientes legitimos")
    p.add_argument("--seed", type=int, required=True)
    p.add_argument("--scenario", choices=SCENARIOS, required=True)
    p.add_argument("--lam", type=float, required=True)
    p.add_argument("--clients", type=int, required=True)
    p.add_argument("--dur", type=float, required=True)
    p.add_argument("--out", required=True)
    p.add_argument("--control-fraction", type=float, default=DEFAULT_CONTROL_FRACTION)
    p.add_argument("--colocate-fraction", type=float, default=DEFAULT_COLOCATE_FRACTION)
    return p.parse_args(argv)


def main(argv):
    a = _parse_args(argv)
    schedule = gen(
        a.seed, a.lam, a.clients, a.dur, a.scenario, a.control_fraction, a.colocate_fraction
    )
    with open(a.out, "w", encoding="utf-8") as f:
        json.dump(schedule, f, sort_keys=True, separators=(",", ":"))
        f.write("\n")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (ValueError, OSError) as e:
        sys.exit(f"schedule.py: {e}")
