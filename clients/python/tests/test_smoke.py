# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Smoke tests for the package skeleton.

These assert the package imports cleanly and exposes a single-sourced version,
giving the bootstrap check suite (``make py-check``) something to collect before
any feature modules exist.
"""

from __future__ import annotations

import sqi_client


def test_package_exposes_version_in_public_api() -> None:
    assert "__version__" in sqi_client.__all__
    assert hasattr(sqi_client, "__version__")


def test_version_is_nonempty_string() -> None:
    assert isinstance(sqi_client.__version__, str)
    assert sqi_client.__version__
