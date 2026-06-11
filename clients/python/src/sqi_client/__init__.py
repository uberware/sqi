# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi-client — a pure-Python client for the sqi distributed task farm manager.

This package wraps the sqi-server REST and WebSocket API for scripted job
submission, querying, and management. It is the foundation for the Phase 2 DCC
submitters and for pipeline automation scripts (see ``sqi.md`` §13.2).

The curated public API surface is defined in ``__all__``; everything else is
underscore-private and not part of the supported interface.
"""

from __future__ import annotations

from ._version import __version__

__all__ = ["__version__"]
