# SPDX-License-Identifier: AGPL-3.0-or-later
"""Live-server fixtures for submitter integration tests."""

from __future__ import annotations

import os
import pathlib

import pytest

from sqi_submitter.core import SubmitterSession

# clients/submitter/tests/integration/conftest.py -> repo root is four levels up.
_PRESETS_DIR = pathlib.Path(__file__).resolve().parents[4] / "presets" / "dcc"
PRESETS = [p for p in sorted(_PRESETS_DIR.glob("*.yaml")) if not p.name.startswith("._")]


@pytest.fixture(scope="session")
def server_url() -> str:
    url = os.environ.get("SQI_TEST_SERVER_URL")
    if not url:
        pytest.skip("SQI_TEST_SERVER_URL not set")
    return url


@pytest.fixture(scope="session")
def session(server_url: str) -> SubmitterSession:
    return SubmitterSession(server_url=server_url)


@pytest.fixture(scope="session")
def farm_and_queue(session: SubmitterSession) -> tuple[str, str]:
    farms = session.farms()
    if not farms:
        pytest.skip("server has no farm")
    queues = session.queues(farms[0].id)
    if not queues:
        pytest.skip("farm has no queue")
    return farms[0].id, queues[0].id
