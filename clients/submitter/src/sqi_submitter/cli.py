# SPDX-License-Identifier: AGPL-3.0-or-later
"""``sqi-submit`` console entry point.

Qt is imported only on the dialog path, so ``sqi-submit install-host`` works in
an environment with no Qt binding.
"""

from __future__ import annotations

import argparse
import sys
import traceback
from pathlib import Path
from typing import Any

from sqi_submitter._failure import report_failure
from sqi_submitter._version import __version__
from sqi_submitter.core import SubmitterSession
from sqi_submitter.core.errors import SubmitterError
from sqi_submitter.core.handoff import ContextAdapter, load_context

__all__ = ["main"]


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="sqi-submit", description="Submit a job to sqi.")
    # The After Effects panel runs this before each launch: an sqi-submit too old to know
    # --detached would exit on it in a console that closes at once.
    parser.add_argument("--version", action="version", version=f"sqi-submit {__version__}")
    parser.add_argument("--server", default=None, help="sqi server URL (default: configured)")
    parser.add_argument(
        "--context",
        default=None,
        metavar="FILE",
        help="handoff context file written by a host application (deleted once read)",
    )
    # Passed by a host launch whose console closes with the process (After Effects'
    # `cmd /c start` of a console sqi-submit.exe; sqi-submitw has no console and needs
    # no flag): always show a startup failure natively. Not for users.
    parser.add_argument("--detached", action="store_true", help=argparse.SUPPRESS)
    sub = parser.add_subparsers(dest="command", metavar="COMMAND")
    install = sub.add_parser("install-host", help="install the sqi submitter into a host")
    install.add_argument("host", help="host to install into: blender, cinema4d or aftereffects")
    install.add_argument("--dest", default=None, help="install into this directory")
    return parser


def _exec_app(app: Any) -> int:
    run = getattr(app, "exec", None) or app.exec_
    return int(run())


def _run_dialog(server: str | None, context: str | None) -> int:
    from sqi_submitter.qt._compat import QtWidgets, require_qt

    # Before importing the dialog, which cannot even be imported without a binding.
    require_qt()
    from sqi_submitter.qt import dialog as qt_dialog

    adapter: ContextAdapter | None = None
    error: str | None = None
    if context:
        try:
            adapter = load_context(context)
        except SubmitterError as exc:
            # Open the dialog anyway: a banner naming the file beats a traceback
            # in a process the artist cannot see.
            error = exc.user_message

    # An explicit --server wins; otherwise the file's server_url; otherwise
    # the session's normal resolution order.
    server_url = server or (adapter.server_url if adapter is not None else None)

    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    dialog = qt_dialog.SubmitDialog(
        SubmitterSession(server_url=server_url), adapter=adapter, initial_error=error
    )
    dialog.show()
    dialog.raise_()
    dialog.activateWindow()
    return _exec_app(app)


def main(argv: list[str] | None = None) -> int:
    """Console-script entry point (``sqi-submit``)."""
    args = _build_parser().parse_args(argv)
    if args.command == "install-host":
        from sqi_submitter.install import KNOWN_HOSTS, install_host

        # An unknown host is a usage error, so its message goes to stderr.
        out = sys.stdout if args.host in KNOWN_HOSTS else sys.stderr
        return install_host(args.host, Path(args.dest) if args.dest else None, out, sys.stderr)
    # A host starts sqi-submit detached, so a failure is also logged and, with no
    # terminal to read stderr (or with --detached), shown natively (see _failure).
    try:
        return _run_dialog(args.server, args.context)
    except SubmitterError as exc:
        report_failure(exc.user_message, detached=args.detached)
        return 1
    except Exception as exc:  # an artist cannot read a detached traceback
        report_failure(
            f"sqi-submit stopped unexpectedly: {type(exc).__name__}: {exc}",
            traceback.format_exc(),
            detached=args.detached,
        )
        return 1
