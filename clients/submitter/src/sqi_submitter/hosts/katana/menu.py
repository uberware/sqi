# SPDX-License-Identifier: AGPL-3.0-or-later
"""Katana launch glue: on Katana 8 and later, the handoff module's click calls open_submitter()."""

from __future__ import annotations


def open_submitter() -> None:
    from UI4.App import MainWindow

    from sqi_submitter.hosts.katana.adapter import KatanaAdapter
    from sqi_submitter.qt.dialog import open_for_adapter

    open_for_adapter(KatanaAdapter(), parent=MainWindow.CurrentMainWindow())
