# SPDX-License-Identifier: AGPL-3.0-or-later
"""Katana: the handoff module and its startup hook, plus the in-process adapter and menu glue.

``sqi_katana_submit.py`` and ``init.py`` are copied into Katana's Startup folder and must have
no import statement for ``sqi_submitter``. On Katana 8 and later the handoff module opens the
dialog in Katana's own Python through ``menu.open_submitter()`` when ``sqi_submitter`` is
installed there.
"""
