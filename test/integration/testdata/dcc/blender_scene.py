# SPDX-License-Identifier: AGPL-3.0-or-later
# Builds the scene TestPresetDCC_Blender_* render. Run by the test as:
#   blender -b --factory-startup --python-exit-code 1 --python blender_scene.py -- STEM RENDER_DIR
# Two scenes. "Shot" has frames 1-4 and two view layers, "Fg" (a cube) and
# "Bg 'quoted' layer" (a sphere), so a render shows which layer ran. "Alt" has one
# view layer, "AltLayer", so a render shows which scene -S selected. Cycles on CPU
# at 64x36 with 4 samples renders a frame in about a second. Output is multilayer EXR.
# Prints "SAVED <path>" once the .blend is written.
import os
import sys

import bpy

stem, render_dir = sys.argv[sys.argv.index("--") + 1 :][:2]
out_blend = stem + ".blend"

bpy.ops.wm.read_factory_settings(use_empty=True)


def setup(scene, prefix):
    scene.render.engine = "CYCLES"
    scene.cycles.device = "CPU"
    scene.cycles.samples = 4
    scene.render.resolution_x = 64
    scene.render.resolution_y = 36
    scene.render.resolution_percentage = 100
    settings = scene.render.image_settings
    if hasattr(settings, "media_type"):  # Blender 5: multilayer is a media type
        settings.media_type = "MULTI_LAYER_IMAGE"
    settings.file_format = "OPEN_EXR_MULTILAYER"
    scene.render.filepath = f"{render_dir}/{prefix}_####"
    scene.frame_start = 1
    scene.frame_end = 4


scene = bpy.context.scene
scene.name = "Shot"
setup(scene, "scene")

cam = bpy.data.objects.new("Cam", bpy.data.cameras.new("Cam"))
cam.location = (0, -6, 0)
cam.rotation_euler = (1.5708, 0, 0)
scene.collection.objects.link(cam)
scene.camera = cam
light = bpy.data.objects.new("Sun", bpy.data.lights.new("Sun", "SUN"))
scene.collection.objects.link(light)

fg = bpy.data.collections.new("FG")
bg = bpy.data.collections.new("BG")
scene.collection.children.link(fg)
scene.collection.children.link(bg)


def place(add, collection, x):
    add(location=(x, 0, 0))
    obj = bpy.context.active_object
    for c in obj.users_collection:
        c.objects.unlink(obj)
    collection.objects.link(obj)


place(bpy.ops.mesh.primitive_cube_add, fg, -1)
place(bpy.ops.mesh.primitive_uv_sphere_add, bg, 1)

fg_layer = scene.view_layers[0]
fg_layer.name = "Fg"
bg_layer = scene.view_layers.new("Bg 'quoted' layer")
fg_layer.layer_collection.children["BG"].exclude = True
bg_layer.layer_collection.children["FG"].exclude = True

alt = bpy.data.scenes.new("Alt")
setup(alt, "alt")
alt.collection.objects.link(cam)
alt.camera = cam
alt.collection.objects.link(light)
alt.collection.children.link(fg)
alt.view_layers[0].name = "AltLayer"

os.makedirs(os.path.dirname(os.path.abspath(out_blend)), exist_ok=True)  # Blender will not
bpy.ops.wm.save_as_mainfile(filepath=out_blend)
print("SAVED", out_blend)
