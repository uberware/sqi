# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Shared pytest fixtures and helpers for the unit-test suite.

Unit tests mock HTTP at the transport layer with ``respx`` and never require a
running server.
"""

from __future__ import annotations

import json
from collections.abc import Callable, Iterator
from typing import Any

import httpx
import pytest

from sqi_client.client import SqiClient

BASE_URL = "http://sqi.test"

ClientFactory = Callable[..., SqiClient]
ProblemFactory = Callable[..., httpx.Response]


def pytest_configure(config: pytest.Config) -> None:
    """Register the ``integration`` marker.

    It is also declared in ``pyproject.toml``, but that config is only loaded
    when pytest runs from ``clients/python/``. Registering it here too keeps the
    marker recognized (no ``PytestUnknownMarkWarning``) even when the suite is
    invoked from the repository root.
    """
    config.addinivalue_line(
        "markers",
        "integration: tests that require a running sqi-server binary (deselected by default)",
    )


def problem_response(
    status: int,
    *,
    detail: str,
    title: str = "Error",
    instance: str | None = None,
    request_id_header: str | None = None,
    content_type: str = "application/problem+json",
) -> httpx.Response:
    """Build an RFC 7807 problem-details response for tests."""
    body: dict[str, Any] = {
        "type": "about:blank",
        "title": title,
        "status": status,
        "detail": detail,
    }
    if instance is not None:
        body["instance"] = instance
    headers = {"Content-Type": content_type}
    if request_id_header is not None:
        headers["X-Request-Id"] = request_id_header
    return httpx.Response(status, content=json.dumps(body).encode(), headers=headers)


@pytest.fixture
def make_client() -> Iterator[ClientFactory]:
    """Factory for ``SqiClient`` instances pointed at ``BASE_URL``.

    Defaults disable retries and backoff so most tests run fast and
    deterministically; pass overrides (e.g. ``max_attempts=3``) where the retry
    machinery is under test. Every created client is closed on teardown.
    """
    created: list[SqiClient] = []

    def _make(**kwargs: Any) -> SqiClient:
        params: dict[str, Any] = {
            "max_attempts": 1,
            "retry_backoff": 0.0,
            "retry_jitter": False,
        }
        params.update(kwargs)
        client = SqiClient(BASE_URL, **params)
        created.append(client)
        return client

    yield _make
    for client in created:
        client.close()


@pytest.fixture
def problem() -> ProblemFactory:
    """Expose :func:`problem_response` as a fixture."""
    return problem_response
