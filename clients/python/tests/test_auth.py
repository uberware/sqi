# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for bearer-token/env auth plumbing (forward-looking; A2 issues keys).

A1 ships no issuable headless credential — the browser uses an HttpOnly
session cookie. These tests only cover the client-side consumption seam (token
resolution -> ``Authorization`` header) and the ``SqiAuthError`` mapping for
401/403, with no live credential to round-trip against.
"""

from __future__ import annotations

import httpx
import pytest

from sqi_client import SqiAuthError
from sqi_client.errors import api_error_from_response
from tests.conftest import BASE_URL, ClientFactory


@pytest.fixture(autouse=True)
def _clear_auth_env(monkeypatch: pytest.MonkeyPatch) -> None:
    """Ensure no ambient SQI_TOKEN/SQI_API_KEY from the caller's shell leaks in."""
    monkeypatch.delenv("SQI_TOKEN", raising=False)
    monkeypatch.delenv("SQI_API_KEY", raising=False)


def test_token_sets_bearer_header(make_client: ClientFactory) -> None:
    client = make_client(token="secret-token")
    assert client._default_headers["Authorization"] == "Bearer secret-token"


def test_token_from_env(monkeypatch: pytest.MonkeyPatch, make_client: ClientFactory) -> None:
    monkeypatch.setenv("SQI_TOKEN", "env-token")
    client = make_client()
    assert client._default_headers["Authorization"] == "Bearer env-token"


def test_explicit_token_beats_env(
    monkeypatch: pytest.MonkeyPatch, make_client: ClientFactory
) -> None:
    monkeypatch.setenv("SQI_TOKEN", "env-token")
    client = make_client(token="explicit")
    assert client._default_headers["Authorization"] == "Bearer explicit"


def test_sqi_api_key_env_fallback(
    monkeypatch: pytest.MonkeyPatch, make_client: ClientFactory
) -> None:
    monkeypatch.setenv("SQI_API_KEY", "api-key-token")
    client = make_client()
    assert client._default_headers["Authorization"] == "Bearer api-key-token"


def test_sqi_token_env_beats_sqi_api_key_env(
    monkeypatch: pytest.MonkeyPatch, make_client: ClientFactory
) -> None:
    monkeypatch.setenv("SQI_TOKEN", "token-wins")
    monkeypatch.setenv("SQI_API_KEY", "api-key-loses")
    client = make_client()
    assert client._default_headers["Authorization"] == "Bearer token-wins"


def test_explicit_headers_beat_resolved_token(make_client: ClientFactory) -> None:
    client = make_client(token="from-token-arg", headers={"Authorization": "Bearer from-headers"})
    assert client._default_headers["Authorization"] == "Bearer from-headers"


def test_no_token_anywhere_means_no_authorization_header(make_client: ClientFactory) -> None:
    client = make_client()
    assert "Authorization" not in client._default_headers


@pytest.mark.parametrize("status", [401, 403])
def test_401_and_403_raise_sqi_auth_error(status: int) -> None:
    resp = httpx.Response(
        status,
        headers={"Content-Type": "application/problem+json"},
        json={"title": "Unauthorized", "detail": "authentication required"},
        request=httpx.Request("GET", f"{BASE_URL}/api/v1/jobs"),
    )
    err = api_error_from_response(resp)
    assert isinstance(err, SqiAuthError)
    assert err.status == status


def test_sqi_auth_error_importable_from_top_level_package() -> None:
    assert SqiAuthError is not None
