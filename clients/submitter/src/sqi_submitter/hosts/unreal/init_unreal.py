# SPDX-License-Identifier: AGPL-3.0-or-later
from __future__ import annotations

# sqi-submitter hook
try:
    import sqi_unreal_submit

    sqi_unreal_submit.register_menu()
except Exception as _sqi_error:  # a broken sqi install must not stop the code after it
    # ASCII only: a narrow-encoding console must not raise inside this guard.
    _sqi_message = "sqi: Submit to sqi... was not added: " + str(_sqi_error)
    print(_sqi_message.encode("ascii", "replace").decode("ascii"))
