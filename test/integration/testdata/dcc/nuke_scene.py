# SPDX-License-Identifier: AGPL-3.0-or-later
# Builds the script TestPresetDCC_Nuke_* render. Run by the test as:
#   Nuke17.1 [--nc] -t nuke_scene.py STEM RENDER_DIR [delay=SECONDS]
# Frames 1-4 at 320x180: a blue Constant feeding Write1 and a CheckerBoard
# feeding Write2, each writing PNGs into RENDER_DIR, so a render shows which Write
# node ran. Non-commercial Nuke only renders an encrypted .nknc from the command
# line, so the extension follows the license. delay=SECONDS makes each Write node
# sleep that long before it writes a frame, so a frame takes longer than the
# worker's 2 s progress throttle; it is not a bare number because Nuke takes a
# trailing number on its command line as a frame range and never passes it to
# the script. Prints "SAVED <path>".
import os
import sys

import nuke

stem, renders = sys.argv[1], sys.argv[2]
frame_delay = float(sys.argv[3].removeprefix("delay=")) if len(sys.argv) > 3 else 0
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

if frame_delay:
    for write in (write1, write2):
        write["beforeFrameRender"].setValue("import time; time.sleep(%r)" % frame_delay)

nuke.scriptSaveAs(out, 1)
print("SAVED", out)
