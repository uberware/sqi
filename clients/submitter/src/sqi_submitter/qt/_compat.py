# SPDX-License-Identifier: AGPL-3.0-or-later
"""PySide6-preferred Qt binding shim; DCCs supply their own binding."""

from __future__ import annotations

from typing import TYPE_CHECKING

from sqi_submitter.core.errors import SubmitterError

__all__ = ["QT_BINDING", "QtCore", "QtGui", "QtWidgets", "require_qt"]

QT_BINDING: str

if TYPE_CHECKING:
    # Type-check qt.* modules against the real PySide6 stubs so attribute
    # typos (e.g. QtWidgets.QLinEdit) are mypy errors; runtime keeps the
    # graceful no-binding fallback below.
    from PySide6 import QtCore, QtGui, QtWidgets

    QT_BINDING = "PySide6"
else:
    QT_BINDING = ""
    QtCore = QtGui = QtWidgets = None

    try:  # pragma: no cover - depends on environment
        from PySide6 import QtCore, QtGui, QtWidgets

        QT_BINDING = "PySide6"
    except ImportError:  # pragma: no cover
        try:
            from PySide2 import QtCore, QtGui, QtWidgets

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
