# SPDX-License-Identifier: AGPL-3.0-or-later
"""Fusion handoff script: ``Submit to sqi.lua`` (Lua 5.1; not importable Python).

Fusion bundles Lua but not Python, so its submitter is one self-contained Lua file that writes
the handoff context file and launches ``sqi-submit --context``. It never imports
``sqi_submitter``.
"""
