# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for SqiClient.me() and the Principal model.

HTTP is mocked at the transport layer with respx; no server runs.
"""

from __future__ import annotations

import httpx
import respx

from sqi_client import Principal
from tests.conftest import BASE_URL, ClientFactory

_ME_URL = f"{BASE_URL}/api/v1/auth/me"


@respx.mock
def test_me_parses_principal(make_client: ClientFactory) -> None:
    """me() returns a Principal carrying username and permissions."""
    route = respx.get(_ME_URL).mock(
        return_value=httpx.Response(
            200,
            json={
                "subject": "u-1",
                "username": "alice",
                "display_name": "Alice",
                "roles": ["user"],
                "permissions": ["jobs.read", "jobs.write"],
                "kind": "user",
            },
        )
    )
    client = make_client()

    principal = client.me()

    assert isinstance(principal, Principal)
    assert principal.username == "alice"
    assert "jobs.read" in principal.permissions
    assert principal.roles == ["user"]
    assert route.called


def test_principal_from_dict_defaults_missing_fields() -> None:
    """A response omitting username (the anonymous principal) parses cleanly."""
    p = Principal.from_dict(
        {
            "subject": "",
            "display_name": "anonymous",
            "roles": [],
            "permissions": [],
            "kind": "anonymous",
        }
    )
    assert p.username is None
    assert p.permissions == []
