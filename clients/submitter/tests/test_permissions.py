# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for submitter-side permission resolution and Owner-field gating."""

from __future__ import annotations

from typing import Any

import pytest

from sqi_submitter.core.session import SubmitterSession


class _StubClient:
    """Minimal SqiClient stand-in exposing only me()."""

    def __init__(self, permissions: list[str] | None, raises: Exception | None = None) -> None:
        self._permissions = permissions
        self._raises = raises
        self.calls = 0

    def me(self) -> Any:
        self.calls += 1
        if self._raises is not None:
            raise self._raises
        return type("P", (), {"permissions": self._permissions or []})()


def test_may_submit_as_true_when_granted(monkeypatch: pytest.MonkeyPatch) -> None:
    session = SubmitterSession(server_url="http://x")
    session.client = _StubClient(["jobs.read", "jobs.submit_as"])  # type: ignore[assignment]
    assert session.may_submit_as is True


def test_may_submit_as_false_when_absent(monkeypatch: pytest.MonkeyPatch) -> None:
    session = SubmitterSession(server_url="http://x")
    session.client = _StubClient(["jobs.read"])  # type: ignore[assignment]
    assert session.may_submit_as is False


def test_permissions_are_cached(monkeypatch: pytest.MonkeyPatch) -> None:
    session = SubmitterSession(server_url="http://x")
    stub = _StubClient(["jobs.submit_as"])
    session.client = stub  # type: ignore[assignment]
    _ = session.may_submit_as
    _ = session.may_submit_as
    assert stub.calls == 1


def test_may_submit_as_defaults_true_when_me_unavailable(monkeypatch: pytest.MonkeyPatch) -> None:
    """An older server has no permissions in /auth/me — keep the field usable
    rather than breaking submission against it."""
    session = SubmitterSession(server_url="http://x")
    session.client = _StubClient(None, raises=RuntimeError("404"))  # type: ignore[assignment]
    assert session.may_submit_as is True
