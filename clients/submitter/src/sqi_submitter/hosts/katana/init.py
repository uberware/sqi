# SPDX-License-Identifier: AGPL-3.0-or-later
from __future__ import annotations

# sqi-submitter hook
import os
import sys

try:
    from Katana import Callbacks

    try:
        _sqi_folder = os.path.dirname(os.path.abspath(__file__))
    except NameError:  # Katana runs init.py with execfile, which may not define __file__
        _sqi_folder = os.path.join(os.path.expanduser("~"), ".katana", "Startup")
    if _sqi_folder not in sys.path:
        sys.path.insert(0, _sqi_folder)

    import sqi_katana_submit

    Callbacks.addCallback(Callbacks.Type.onStartupComplete, sqi_katana_submit.install_menu)
except Exception as _sqi_error:  # a broken sqi install must not stop the code after it
    # ASCII only: a narrow-encoding console must not raise inside this guard.
    _sqi_message = "sqi: Submit to sqi... was not added: " + str(_sqi_error)
    print(_sqi_message.encode("ascii", "replace").decode("ascii"))
