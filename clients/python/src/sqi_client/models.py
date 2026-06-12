# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Shared model types for the sqi client.

This module currently holds the pagination primitives used by every list
endpoint. Resource dataclasses (Job, Task, Worker, ...) are added in a later
section.
"""

from __future__ import annotations

from collections.abc import Callable, Iterator
from dataclasses import dataclass
from typing import Generic, TypeVar

__all__ = ["Page", "iter_pages"]

T = TypeVar("T")


@dataclass(frozen=True)
class Page(Generic[T]):
    """One page of a list response, mirroring the server's wrapper object.

    Attributes:
        items: The items on this page.
        total: Total number of items across all pages matching the query.
        limit: The page size the server applied.
        offset: The zero-based offset of this page.
    """

    items: list[T]
    total: int
    limit: int
    offset: int


def iter_pages(
    fetch_page: Callable[[int, int], Page[T]],
    *,
    limit: int,
    offset: int = 0,
) -> Iterator[T]:
    """Yield every item across pages, fetching lazily as the caller consumes.

    ``fetch_page`` is called as ``fetch_page(offset, limit)`` and must return the
    corresponding :class:`Page`. Iteration advances ``offset`` by the number of
    items received and stops as soon as the result set is exhausted — when a page
    comes back empty, when the server's ``total`` has been reached, or when a
    short page signals the end. "Short" is judged against the page size the
    server actually applied (``Page.limit``), not the requested ``limit``:
    sqi-server clamps oversized limits (max 1000) rather than rejecting them, and
    a clamped full page must not end the iteration early.

    Args:
        fetch_page: Callable returning the page at a given ``(offset, limit)``.
        limit: Page size to request.
        offset: Starting offset (defaults to the beginning of the result set).

    Yields:
        Each item, in the server's order, across page boundaries.
    """
    current = offset
    while True:
        page = fetch_page(current, limit)
        yield from page.items
        count = len(page.items)
        # The server may clamp the requested page size (sqi-server caps limit
        # at 1000), so a page is only "short" relative to the size the server
        # actually applied — judging by the requested limit would silently
        # truncate a clamped result set after its first page.
        applied_limit = page.limit if 0 < page.limit < limit else limit
        # Stop on an empty page, once the absolute position reaches the server's
        # reported total, or on a short page — all of which mark the end of the
        # result set. Using absolute position rather than a running count keeps
        # termination correct when `offset > 0`.
        if count == 0 or current + count >= page.total or count < applied_limit:
            return
        current += count
