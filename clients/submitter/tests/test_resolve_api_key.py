# SPDX-License-Identifier: AGPL-3.0-or-later
from __future__ import annotations

from sqi_submitter.core.session import SubmitterSession, resolve_api_key


class FakeSettings:
    def __init__(self, values: dict[str, str] | None = None) -> None:
        self._v = values or {}

    def get(self, key: str, default=None):
        return self._v.get(key, default)


def test_resolve_precedence_explicit_wins(monkeypatch):
    monkeypatch.setenv("SQI_API_KEY", "env-key")
    assert resolve_api_key("explicit", FakeSettings({"api_key": "stored"})) == "explicit"


def test_resolve_env_over_settings(monkeypatch):
    monkeypatch.setenv("SQI_API_KEY", "env-key")
    assert resolve_api_key(None, FakeSettings({"api_key": "stored"})) == "env-key"


def test_resolve_settings_when_no_env(monkeypatch):
    monkeypatch.delenv("SQI_API_KEY", raising=False)
    assert resolve_api_key(None, FakeSettings({"api_key": "stored"})) == "stored"


def test_resolve_none_when_unset(monkeypatch):
    monkeypatch.delenv("SQI_API_KEY", raising=False)
    assert resolve_api_key(None, FakeSettings()) is None


def test_session_forwards_key_to_client(monkeypatch):
    monkeypatch.setenv("SQI_API_KEY", "forwarded-key")
    captured: dict[str, object] = {}

    class FakeClient:
        def __init__(self, server_url, token=None):
            captured["server_url"] = server_url
            captured["token"] = token

    monkeypatch.setattr("sqi_submitter.core.session.SqiClient", FakeClient)
    SubmitterSession(server_url="http://x", settings=FakeSettings())
    assert captured["token"] == "forwarded-key"
