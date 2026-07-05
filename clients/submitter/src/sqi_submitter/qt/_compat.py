# SPDX-License-Identifier: AGPL-3.0-or-later
"""PySide6-preferred Qt binding shim; DCCs supply their own binding."""

from __future__ import annotations

from sqi_submitter.core.errors import SubmitterError

QT_BINDING = ""
QtCore = QtGui = QtWidgets = None  # type: ignore[assignment]

try:  # pragma: no cover - depends on environment
    from PySide6 import QtCore, QtGui, QtWidgets  # type: ignore[no-redef]

    QT_BINDING = "PySide6"
except ImportError:  # pragma: no cover
    try:
        from PySide2 import QtCore, QtGui, QtWidgets  # type: ignore[no-redef] # noqa: F401

        QT_BINDING = "PySide2"
    except ImportError:
        pass


def require_qt() -> None:
    """Fail with an actionable message when no Qt binding is importable."""
    if not QT_BINDING:
        raise SubmitterError(
            "No Qt binding found. Run inside a DCC that bundles PySide, or "
            "install one with: pip install 'sqi-submitter[qt]'"
        )
