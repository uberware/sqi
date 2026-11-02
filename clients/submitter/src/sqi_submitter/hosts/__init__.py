# SPDX-License-Identifier: AGPL-3.0-or-later
"""DCC host integrations.

In-process adapters, which open the Qt dialog inside the application: Maya,
Houdini, Nuke, 3ds Max and VRED. Handoff hosts, which write a context file and
launch ``sqi-submit``: Blender, Cinema 4D, After Effects, Unreal, Katana,
KeyShot, Silhouette, Fusion (Lua) and Modo. Katana is both: on Katana 8 and
later its handoff module opens the dialog in process when ``sqi_submitter`` is
installed for Katana's Python.
"""
