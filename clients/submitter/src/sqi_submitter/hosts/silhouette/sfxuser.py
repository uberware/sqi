# SPDX-License-Identifier: AGPL-3.0-or-later
from __future__ import annotations

# sqi-submitter hook
import os
import sys

try:
    _sqi_folder = os.path.dirname(os.path.abspath(__file__))
except NameError:  # Silhouette may run this without defining __file__
    _sqi_folder = os.path.join(os.environ.get("SFX_USER_PATH", ""), "scripts")
if _sqi_folder not in sys.path:
    sys.path.insert(0, _sqi_folder)

try:
    import sqi_silhouette_submit

    sqi_silhouette_submit.register()
except Exception as _sqi_error:  # a broken sqi install must not stop the code after it
    # ASCII only: a narrow-encoding console must not raise inside this guard.
    _sqi_message = "sqi: Submit to sqi... was not added: " + str(_sqi_error)
    print(_sqi_message.encode("ascii", "replace").decode("ascii"))
