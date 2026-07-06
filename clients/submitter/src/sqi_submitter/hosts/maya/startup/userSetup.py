# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi submitter bootstrap: adds the sqi menu once Maya's UI is ready."""

from maya import cmds

cmds.evalDeferred("from sqi_submitter.hosts.maya.menu import install_menu; install_menu()")
