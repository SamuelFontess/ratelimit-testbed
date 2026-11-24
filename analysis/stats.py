import math
from collections.abc import Sequence

from scipy.stats import t

DEFAULT_CONFIDENCE = 0.95
MIN_SEEDS = 2


def paired_ci(
    a: Sequence[float], b: Sequence[float], confidence: float = DEFAULT_CONFIDENCE
) -> tuple[float, float, float]:
    if len(a) != len(b):
        raise ValueError(f"series de seeds com tamanhos diferentes: {len(a)} != {len(b)}")
    n = len(a)
    if n < MIN_SEEDS:
        raise ValueError(f"IC pareado exige ao menos {MIN_SEEDS} seeds, recebeu {n}")
    diffs = [x - y for x, y in zip(a, b)]
    mean = math.fsum(diffs) / n
    sq = math.fsum((d - mean) ** 2 for d in diffs)
    half = t.ppf(0.5 + confidence / 2, n - 1) * math.sqrt(sq / (n - 1) / n)
    return mean, mean - half, mean + half


def significant(ci: tuple[float, float, float]) -> bool:
    _, lo, hi = ci
    return lo > 0 or hi < 0


def pareto_frontier(points: Sequence[tuple[float, float]]) -> list[tuple[float, float]]:
    frontier: list[tuple[float, float]] = []
    best_pft = math.inf
    for contention, pft in sorted(set(points), key=lambda p: (-p[0], p[1])):
        if pft < best_pft:
            frontier.append((contention, pft))
            best_pft = pft
    return frontier[::-1]
