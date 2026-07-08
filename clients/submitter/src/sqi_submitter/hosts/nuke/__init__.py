# SPDX-License-Identifier: AGPL-3.0-or-later
"""Nuke submitter adapter and menu installation."""

from __future__ import annotations

from sqi_submitter.hosts.nuke.adapter import NukeAdapter
from sqi_submitter.hosts.nuke.menu import install_menu, open_submitter

__all__ = ["NukeAdapter", "install_menu", "open_submitter"]
