# SPDX-License-Identifier: AGPL-3.0-or-later
"""Nuke menu registration; call install_menu() from a startup/menu.py."""

from __future__ import annotations


def open_submitter() -> None:
    from sqi_submitter.hosts.nuke.adapter import NukeAdapter
    from sqi_submitter.qt.dialog import open_for_adapter

    open_for_adapter(NukeAdapter())


def install_menu() -> None:
    import nuke

    nuke.menu("Nuke").addCommand("sqi/Submit…", open_submitter)
