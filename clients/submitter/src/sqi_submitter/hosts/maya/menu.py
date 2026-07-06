# SPDX-License-Identifier: AGPL-3.0-or-later
"""Maya menu registration; call install_menu() from userSetup.py."""

from __future__ import annotations

_MENU = "sqiSubmitMenu"


def open_submitter() -> None:
    from sqi_submitter.hosts.maya.adapter import MayaAdapter
    from sqi_submitter.qt.dialog import open_for_adapter

    open_for_adapter(MayaAdapter())


def install_menu() -> None:
    from maya import cmds

    if cmds.menu(_MENU, exists=True):
        cmds.deleteUI(_MENU)
    cmds.menu(_MENU, label="sqi", parent="MayaWindow", tearOff=False)
    cmds.menuItem(label="Submit…", parent=_MENU, command=lambda *_: open_submitter())
