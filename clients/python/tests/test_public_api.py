# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for the curated public API surface."""

from __future__ import annotations

import sqi_client


def test_star_import_exposes_exactly_all() -> None:
    namespace: dict[str, object] = {}
    # Star import respects __all__; this verifies the curated surface exactly.
    exec("from sqi_client import *", namespace)
    exported = set(namespace) - {"__builtins__"}
    assert exported == set(sqi_client.__all__)


def test_all_contains_no_private_names() -> None:
    # Single-underscore names are private; dunders like __version__ are public.
    for name in sqi_client.__all__:
        assert not (name.startswith("_") and not name.startswith("__")), name


def test_all_names_are_importable_attributes() -> None:
    for name in sqi_client.__all__:
        assert hasattr(sqi_client, name), name


def test_key_public_names_are_exported() -> None:
    for required in (
        "__version__",
        "SqiClient",
        "Job",
        "Task",
        "Worker",
        "JobStatus",
        "SqiError",
        "NotFoundError",
        "ValidationError",
        "CancelResult",
        "Page",
        "ServerVersion",
        "SqiEventStream",
        "Event",
    ):
        assert required in sqi_client.__all__, required


def test_repr_shows_base_url_only() -> None:
    client = sqi_client.SqiClient(
        "http://localhost:8080",
        headers={"Authorization": "Bearer s3cret"},
        max_attempts=1,
    )
    text = repr(client)
    assert "http://localhost:8080" in text
    # Never leak credentials/headers in the repr.
    assert "s3cret" not in text
    assert "Authorization" not in text
    client.close()
