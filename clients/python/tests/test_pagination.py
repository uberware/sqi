# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for the Page wrapper and the auto-paging helper (task 20)."""

from __future__ import annotations

from collections.abc import Callable

from sqi_client.models import Page, iter_pages

FetchPage = Callable[[int, int], Page[int]]


def _server(
    items: list[int], *, total: int | None = None
) -> tuple[FetchPage, list[tuple[int, int]]]:
    """Build a fake paged data source over ``items``.

    Returns the ``fetch_page`` callable plus the list it records each requested
    ``(offset, limit)`` into, so tests can assert the access pattern. ``total``
    defaults to ``len(items)`` but can be overridden to model a server whose
    reported total differs from what a single page returns.
    """
    reported_total = len(items) if total is None else total
    calls: list[tuple[int, int]] = []

    def fetch_page(offset: int, limit: int) -> Page[int]:
        calls.append((offset, limit))
        return Page(
            items=items[offset : offset + limit],
            total=reported_total,
            limit=limit,
            offset=offset,
        )

    return fetch_page, calls


def test_walks_multiple_pages_in_order() -> None:
    items = list(range(10))
    fetch_page, calls = _server(items)

    result = list(iter_pages(fetch_page, limit=3))

    assert result == items
    # 0..2, 3..5, 6..8, 9 (short page stops iteration)
    assert calls == [(0, 3), (3, 3), (6, 3), (9, 3)]


def test_empty_result_yields_nothing() -> None:
    fetch_page, calls = _server([])

    result = list(iter_pages(fetch_page, limit=50))

    assert result == []
    assert calls == [(0, 50)]


def test_total_smaller_than_one_page() -> None:
    items = [1, 2, 3]
    fetch_page, calls = _server(items)

    result = list(iter_pages(fetch_page, limit=50))

    assert result == items
    # A single short page ends iteration without a second request.
    assert calls == [(0, 50)]


def test_exact_multiple_stops_via_total() -> None:
    items = list(range(6))
    fetch_page, calls = _server(items)

    result = list(iter_pages(fetch_page, limit=3))

    assert result == items
    # Second page returns exactly `limit` items but seen == total, so we stop
    # without issuing a third (empty) request.
    assert calls == [(0, 3), (3, 3)]


def test_server_returns_fewer_than_limit_midstream_is_treated_as_last_page() -> None:
    # Server claims a large total but hands back a short page; the helper treats
    # the short page as the end rather than spinning forever.
    fetch_page, calls = _server([1, 2], total=1000)

    result = list(iter_pages(fetch_page, limit=10))

    assert result == [1, 2]
    assert calls == [(0, 10)]


def test_honors_starting_offset() -> None:
    items = list(range(10))
    fetch_page, calls = _server(items)

    result = list(iter_pages(fetch_page, limit=4, offset=4))

    assert result == [4, 5, 6, 7, 8, 9]
    assert calls == [(4, 4), (8, 4)]


def test_offset_with_exact_page_boundary_makes_no_extra_request() -> None:
    # offset > 0 with full pages landing exactly on `total`: termination must use
    # absolute position so no redundant empty page is fetched.
    items = list(range(8))
    fetch_page, calls = _server(items)

    result = list(iter_pages(fetch_page, limit=3, offset=2))

    assert result == [2, 3, 4, 5, 6, 7]
    assert calls == [(2, 3), (5, 3)]


def test_server_clamped_limit_does_not_truncate() -> None:
    # sqi-server clamps `limit` to its MaxLimit (1000) instead of rejecting it.
    # A clamped full page must not read as a short page — that would silently
    # drop the rest of the result set. The page's `limit` field carries the
    # size the server actually applied.
    items = list(range(8))
    server_max = 3
    calls: list[tuple[int, int]] = []

    def fetch_page(offset: int, limit: int) -> Page[int]:
        calls.append((offset, limit))
        applied = min(limit, server_max)
        return Page(
            items=items[offset : offset + applied],
            total=len(items),
            limit=applied,
            offset=offset,
        )

    result = list(iter_pages(fetch_page, limit=10))

    assert result == items
    # Pages of 3 (clamped) until the result set is exhausted.
    assert calls == [(0, 10), (3, 10), (6, 10)]


def test_page_is_frozen() -> None:
    page: Page[int] = Page(items=[1], total=1, limit=50, offset=0)
    try:
        page.total = 2  # type: ignore[misc]
    except AttributeError:
        return
    raise AssertionError("Page should be immutable")
