# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for the idempotent-request retry policy (task 20)."""

from __future__ import annotations

import time

import httpx
import pytest
import respx

from sqi_client.errors import ServerError, SqiConnectionError
from tests.conftest import BASE_URL, ClientFactory

_URL = f"{BASE_URL}/api/v1/probe"


@pytest.fixture
def recorded_sleeps(monkeypatch: pytest.MonkeyPatch) -> list[float]:
    """Patch ``time.sleep`` to record (and skip) backoff delays."""
    delays: list[float] = []
    monkeypatch.setattr(time, "sleep", delays.append)
    return delays


@respx.mock
def test_get_retried_on_5xx_then_succeeds(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    route = respx.get(_URL).mock(
        side_effect=[
            httpx.Response(503),
            httpx.Response(503),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3)

    response = client._request("GET", "/probe")

    assert response.status_code == 200
    assert route.call_count == 3
    assert len(recorded_sleeps) == 2  # one sleep before each retry


@respx.mock
def test_get_retried_on_connect_error_then_succeeds(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    route = respx.get(_URL).mock(
        side_effect=[
            httpx.ConnectError("refused"),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3)

    response = client._request("GET", "/probe")

    assert response.status_code == 200
    assert route.call_count == 2
    assert len(recorded_sleeps) == 1


@respx.mock
def test_get_exhausts_attempts_and_raises(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    route = respx.get(_URL).mock(return_value=httpx.Response(503))
    client = make_client(max_attempts=3)

    with pytest.raises(ServerError):
        client._request("GET", "/probe")

    assert route.call_count == 3  # all attempts used
    assert len(recorded_sleeps) == 2


@respx.mock
def test_post_is_never_retried(make_client: ClientFactory, recorded_sleeps: list[float]) -> None:
    route = respx.post(_URL).mock(return_value=httpx.Response(503))
    client = make_client(max_attempts=3)

    with pytest.raises(ServerError):
        client._request("POST", "/probe")

    assert route.call_count == 1  # POST is not idempotent — single attempt
    assert recorded_sleeps == []


@respx.mock
def test_retry_after_header_is_honored(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    route = respx.get(_URL).mock(
        side_effect=[
            httpx.Response(429, headers={"Retry-After": "2"}),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3)

    response = client._request("GET", "/probe")

    assert response.status_code == 200
    assert route.call_count == 2
    assert recorded_sleeps == [2.0]  # honored the server's Retry-After, not backoff


@respx.mock
def test_exponential_backoff_without_jitter(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    respx.get(_URL).mock(
        side_effect=[
            httpx.Response(500),
            httpx.Response(500),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3, retry_backoff=0.5, retry_jitter=False)

    client._request("GET", "/probe")

    # base * 2**(attempt-1): 0.5, then 1.0
    assert recorded_sleeps == [0.5, 1.0]


@respx.mock
def test_backoff_is_capped(make_client: ClientFactory, recorded_sleeps: list[float]) -> None:
    respx.get(_URL).mock(
        side_effect=[
            httpx.Response(500),
            httpx.Response(500),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(
        max_attempts=3, retry_backoff=10.0, retry_backoff_max=12.0, retry_jitter=False
    )

    client._request("GET", "/probe")

    # 10.0, then min(20.0, 12.0) == 12.0
    assert recorded_sleeps == [10.0, 12.0]


@respx.mock
def test_get_timeout_is_retried_then_succeeds(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    route = respx.get(_URL).mock(
        side_effect=[
            httpx.ConnectTimeout("slow"),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3)

    response = client._request("GET", "/probe")

    assert response.status_code == 200
    assert route.call_count == 2
    assert len(recorded_sleeps) == 1


@respx.mock
def test_non_get_transport_error_is_single_attempt(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    route = respx.delete(_URL).mock(side_effect=httpx.ConnectError("refused"))
    client = make_client(max_attempts=3)

    with pytest.raises(SqiConnectionError):
        client._request("DELETE", "/probe")

    assert route.call_count == 1  # mutations are never retried, even on transport errors
    assert recorded_sleeps == []


@respx.mock
def test_429_without_retry_after_falls_back_to_backoff(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    respx.get(_URL).mock(
        side_effect=[
            httpx.Response(429),  # no Retry-After header
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3, retry_backoff=0.5, retry_jitter=False)

    client._request("GET", "/probe")

    assert recorded_sleeps == [0.5]  # exponential backoff, not a honored Retry-After


@respx.mock
def test_negative_retry_after_does_not_crash(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    # A bad server value must degrade to an immediate retry, not raise from sleep().
    respx.get(_URL).mock(
        side_effect=[
            httpx.Response(429, headers={"Retry-After": "-5"}),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3)

    response = client._request("GET", "/probe")

    assert response.status_code == 200
    assert recorded_sleeps == [0.0]  # clamped to zero


@respx.mock
def test_non_numeric_retry_after_falls_back_to_backoff(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    # A non-delta-seconds Retry-After (e.g. an HTTP-date) is unparseable and must
    # fall back to exponential backoff rather than raise.
    respx.get(_URL).mock(
        side_effect=[
            httpx.Response(429, headers={"Retry-After": "Wed, 21 Oct 2026 07:28:00 GMT"}),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3, retry_backoff=0.5, retry_jitter=False)

    client._request("GET", "/probe")

    assert recorded_sleeps == [0.5]


@respx.mock
def test_non_finite_retry_after_falls_back_to_backoff(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    # float("inf") parses, but honoring it would block forever (or overflow in
    # time.sleep); it must be treated as unparseable.
    respx.get(_URL).mock(
        side_effect=[
            httpx.Response(429, headers={"Retry-After": "inf"}),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3, retry_backoff=0.5, retry_jitter=False)

    client._request("GET", "/probe")

    assert recorded_sleeps == [0.5]


@respx.mock
def test_retry_after_is_capped_at_backoff_max(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    # A server-supplied delay must never stall a request beyond the caller's
    # configured ceiling.
    respx.get(_URL).mock(
        side_effect=[
            httpx.Response(429, headers={"Retry-After": "3600"}),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3, retry_backoff_max=12.0)

    client._request("GET", "/probe")

    assert recorded_sleeps == [12.0]


@respx.mock
def test_jitter_keeps_delay_within_bounds(
    make_client: ClientFactory, recorded_sleeps: list[float]
) -> None:
    respx.get(_URL).mock(
        side_effect=[
            httpx.Response(500),
            httpx.Response(500),
            httpx.Response(200, json={"ok": True}),
        ]
    )
    client = make_client(max_attempts=3, retry_backoff=1.0, retry_jitter=True)

    client._request("GET", "/probe")

    # Full jitter: each delay lies in [0, capped] where capped = base * 2**(n-1).
    assert len(recorded_sleeps) == 2
    assert 0.0 <= recorded_sleeps[0] <= 1.0
    assert 0.0 <= recorded_sleeps[1] <= 2.0
