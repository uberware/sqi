# SPDX-License-Identifier: AGPL-3.0-or-later
"""Maya submitter adapter and menu installation."""

from __future__ import annotations

from sqi_submitter.hosts.maya.adapter import MayaAdapter
from sqi_submitter.hosts.maya.menu import install_menu, open_submitter

__all__ = ["MayaAdapter", "install_menu", "open_submitter"]
