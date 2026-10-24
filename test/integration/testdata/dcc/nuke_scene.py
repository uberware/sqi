# SPDX-License-Identifier: AGPL-3.0-or-later
# Builds the script TestPresetDCC_Nuke_* render. Run by the test as:
#   Nuke17.1 [--nc] -t nuke_scene.py STEM RENDER_DIR
# Frames 1-4 at 320x180: a blue Constant feeding Write1 and a CheckerBoard
# feeding Write2, each writing PNGs into RENDER_DIR, so a render shows which Write
# node ran. Non-commercial Nuke only renders an encrypted .nknc from the command
# line, so the extension follows the license. Prints "SAVED <path>".
import os
import sys

import nuke

stem, renders = sys.argv[1], sys.argv[2]
out = stem + (".nknc" if nuke.env.get("nc") else ".nk")
os.makedirs(renders, exist_ok=True)

nuke.addFormat("320 180 1 sqitest")
root = nuke.root()
root["format"].setValue("sqitest")
root["first_frame"].setValue(1)
root["last_frame"].setValue(4)

constant = nuke.nodes.Constant()
constant["color"].setValue([0.2, 0.4, 0.8, 1.0])
write1 = nuke.nodes.Write(name="Write1", file=renders + "/wr1.####.png", file_type="png")
write1.setInput(0, constant)

checker = nuke.nodes.CheckerBoard2()
write2 = nuke.nodes.Write(name="Write2", file=renders + "/wr2.####.png", file_type="png")
write2.setInput(0, checker)

nuke.scriptSaveAs(out, 1)
print("SAVED", out)
