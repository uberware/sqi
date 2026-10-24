# SPDX-License-Identifier: AGPL-3.0-or-later
"""3ds Max launch glue; the startup MAXScript in ``package/`` calls open_submitter()."""

from __future__ import annotations


def open_submitter() -> None:
    import qtmax

    from sqi_submitter.hosts.max.adapter import MaxAdapter
    from sqi_submitter.qt.dialog import open_for_adapter

    open_for_adapter(MaxAdapter(), parent=qtmax.GetQMaxMainWindow())
