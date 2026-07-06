# SPDX-License-Identifier: AGPL-3.0-or-later
"""Houdini submitter launch; call open_submitter() from a shelf tool.

To launch the submitter in Houdini, create a shelf tool with the body:
    from sqi_submitter.hosts.houdini.menu import open_submitter
    open_submitter()
"""

from __future__ import annotations


def open_submitter() -> None:
    from sqi_submitter.hosts.houdini.adapter import HoudiniAdapter
    from sqi_submitter.qt.dialog import open_for_adapter

    open_for_adapter(HoudiniAdapter())
