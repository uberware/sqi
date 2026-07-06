# SPDX-License-Identifier: AGPL-3.0-or-later
"""Houdini submitter adapter and menu installation."""

from __future__ import annotations

from sqi_submitter.hosts.houdini.adapter import HoudiniAdapter
from sqi_submitter.hosts.houdini.menu import open_submitter

__all__ = ["HoudiniAdapter", "open_submitter"]
