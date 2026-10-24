# SPDX-License-Identifier: AGPL-3.0-or-later
# Builds the hip file TestPresetDCC_Houdini_* render. Run by the test as:
#   hython houdini_scene.py STEM OUTPUT_DIR
# A box translated by $FF, and a Geometry ROP /out/geo1 writing
# OUTPUT_DIR/box.$F4.bgeo.sc, so frame N's file holds a box centerd at x=N. A
# Geometry ROP needs no GPU and works under Apprentice, which saves .hipnc.
# Prints "SAVED <path>".
import sys

import hou

stem, outdir = sys.argv[1], sys.argv[2]
hip = stem + (".hipnc" if hou.isApprentice() else ".hip")

hou.hipFile.clear(suppress_save_prompt=True)
geo = hou.node("/obj").createNode("geo", "src")
box = geo.createNode("box")
xform = geo.createNode("xform")
xform.setInput(0, box)
xform.parm("tx").setExpression("$FF")
xform.setDisplayFlag(True)
xform.setRenderFlag(True)
rop = hou.node("/out").createNode("geometry", "geo1")
rop.parm("soppath").set(xform.path())
rop.parm("sopoutput").set(outdir + "/box.$F4.bgeo.sc")
hou.setFrame(1)
hou.playbar.setFrameRange(1, 24)
hou.hipFile.save(hip)
print("SAVED", hip)
