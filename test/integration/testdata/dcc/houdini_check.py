# SPDX-License-Identifier: AGPL-3.0-or-later
# Reads geometry files back. Run by the test as: hython houdini_check.py FILE...
# Prints "<basename> <box center x> <point count>" per file, one line each.
import sys

import hou

for path in sys.argv[1:]:
    geo = hou.Geometry()
    geo.loadFromFile(path)
    print(path.replace("\\", "/").rsplit("/", 1)[-1], geo.boundingBox().center()[0], len(geo.points()))
