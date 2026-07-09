# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Single source of truth for the package version.

Both the wheel metadata (via hatchling's version hook, see ``pyproject.toml``)
and the runtime ``sqi_client.__version__`` read this value, so they can never
diverge.
"""

from __future__ import annotations

__version__ = "0.2.0"
