# SPDX-License-Identifier: AGPL-3.0-or-later
"""VRED launch glue; the script plugin in ``SubmitToSqi/`` calls open_submitter()."""

from __future__ import annotations

from sqi_submitter.hosts.vred.adapter import VREDAdapter, VREDServices


def open_submitter(services: VREDServices) -> None:
    from sqi_submitter.qt.dialog import open_for_adapter

    open_for_adapter(VREDAdapter(services), parent=services.main_window)
