# SPDX-License-Identifier: AGPL-3.0-or-later
from __future__ import annotations

from pathlib import Path

import pytest

from sqi_submitter.core.session import Settings, SubmitterSession, resolve_api_key


def _settings(tmp_path: Path, api_key: str | None = None) -> Settings:
    """A real Settings backed by a throwaway file, optionally pre-seeded."""
    s = Settings(str(tmp_path / "submitter.json"))
    if api_key is not None:
        s.set("api_key", api_key)
    return s


def test_resolve_precedence_explicit_wins(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("SQI_API_KEY", "env-key")
    assert resolve_api_key("explicit", _settings(tmp_path, "stored")) == "explicit"


def test_resolve_env_over_settings(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("SQI_API_KEY", "env-key")
    assert resolve_api_key(None, _settings(tmp_path, "stored")) == "env-key"


def test_resolve_settings_when_no_env(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.delenv("SQI_API_KEY", raising=False)
    assert resolve_api_key(None, _settings(tmp_path, "stored")) == "stored"


def test_resolve_none_when_unset(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.delenv("SQI_API_KEY", raising=False)
    assert resolve_api_key(None, _settings(tmp_path)) is None


def test_session_forwards_key_to_client(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setenv("SQI_API_KEY", "forwarded-key")
    captured: dict[str, object] = {}

    class FakeClient:
        def __init__(self, server_url: str, token: str | None = None) -> None:
            captured["server_url"] = server_url
            captured["token"] = token

    monkeypatch.setattr("sqi_submitter.core.session.SqiClient", FakeClient)
    SubmitterSession(server_url="http://x", settings=_settings(tmp_path))
    assert captured["token"] == "forwarded-key"
