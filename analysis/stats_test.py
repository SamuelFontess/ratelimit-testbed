import math

import pytest
from hypothesis import given, settings, strategies as st

from stats import paired_ci, pareto_frontier, significant

VALUE = st.floats(min_value=-1000, max_value=1000, allow_nan=False, allow_infinity=False)
SHIFT = st.floats(min_value=-100, max_value=100, allow_nan=False, allow_infinity=False)
PAIRS = st.integers(min_value=2, max_value=30).flatmap(
    lambda n: st.tuples(st.lists(VALUE, min_size=n, max_size=n), st.lists(VALUE, min_size=n, max_size=n))
)
POINT = st.tuples(
    st.floats(min_value=0, max_value=1, allow_nan=False, allow_infinity=False),
    st.floats(min_value=0, max_value=100, allow_nan=False, allow_infinity=False),
)
TOLERANCE = 1e-6


def dominates(p: tuple[float, float], q: tuple[float, float]) -> bool:
    return p[0] >= q[0] and p[1] <= q[1] and p != q


def test_known_interval_not_significant():
    mean, lo, hi = paired_ci([2, 4, 6], [1, 2, 3])
    assert mean == pytest.approx(2)
    assert lo == pytest.approx(-0.484, abs=1e-3)
    assert hi == pytest.approx(4.484, abs=1e-3)
    assert not significant((mean, lo, hi))


def test_known_interval_significant():
    assert significant(paired_ci([10, 11, 12], [0, 0, 0]))


def test_zero_variance_is_degenerate():
    assert paired_ci([3, 3, 3], [1, 1, 1]) == (2, 2, 2)


def test_mismatched_sizes_rejected():
    with pytest.raises(ValueError, match="tamanhos"):
        paired_ci([1, 2, 3], [1, 2])


def test_single_seed_rejected():
    with pytest.raises(ValueError, match="ao menos"):
        paired_ci([1], [2])


@pytest.mark.parametrize("ci", [(0, 0, 1), (0, -1, 0), (0, 0, 0), (0, -1, 1)])
def test_interval_touching_or_containing_zero_not_significant(ci):
    assert not significant(ci)


@pytest.mark.parametrize("ci", [(1, 0.1, 2), (-1, -2, -0.1)])
def test_interval_excluding_zero_significant(ci):
    assert significant(ci)


@pytest.mark.parametrize("ci", [(math.nan, math.nan, math.nan), (0, math.nan, 1)])
def test_nan_interval_not_significant(ci):
    assert not significant(ci)


@settings(max_examples=200, deadline=None)
@given(PAIRS)
def test_significance_follows_interval_bounds(pair):
    ci = paired_ci(*pair)
    _, lo, hi = ci
    if lo <= 0 <= hi:
        assert not significant(ci)
    if lo > 0 or hi < 0:
        assert significant(ci)


@given(st.lists(VALUE, min_size=2, max_size=30))
def test_identical_series_have_zero_mean(a):
    mean, lo, hi = paired_ci(a, a)
    assert (mean, lo, hi) == (0, 0, 0)
    assert not significant((mean, lo, hi))


@settings(max_examples=200, deadline=None)
@given(PAIRS, SHIFT)
def test_common_shift_keeps_interval(pair, shift):
    a, b = pair
    base = paired_ci(a, b)
    shifted = paired_ci([x + shift for x in a], [y + shift for y in b])
    assert shifted == pytest.approx(base, abs=TOLERANCE)


@settings(max_examples=200, deadline=None)
@given(PAIRS)
def test_swapping_series_mirrors_interval(pair):
    a, b = pair
    mean, lo, hi = paired_ci(a, b)
    assert paired_ci(b, a) == pytest.approx((-mean, -hi, -lo), abs=TOLERANCE)


@given(st.lists(POINT, max_size=40))
def test_frontier_is_non_dominated_and_covers_input(points):
    frontier = pareto_frontier(points)
    assert len(set(frontier)) == len(frontier)
    assert [p[0] for p in frontier] == sorted(p[0] for p in frontier)
    assert not any(dominates(q, p) for p in frontier for q in frontier)
    for p in points:
        assert any(q == p or dominates(q, p) for q in frontier)


@given(st.lists(POINT, max_size=40))
def test_frontier_is_idempotent(points):
    frontier = pareto_frontier(points)
    assert pareto_frontier(frontier) == frontier
