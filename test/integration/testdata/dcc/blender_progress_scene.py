# SPDX-License-Identifier: AGPL-3.0-or-later
# Builds the scene the Blender SQI_LOG_PROGRESS fixtures were captured from and
# that the Tier-2 progress subtests render. Run as:
#   blender -b --factory-startup --python-exit-code 1 --python blender_progress_scene.py -- \
#     OUT_BLEND ENGINE SAMPLES WIDTH HEIGHT [TILE_SIZE]
# ENGINE is CYCLES or BLENDER_EEVEE. TILE_SIZE, for Cycles, renders the frame in
# tiles of that many pixels square; left out, Blender's default applies. Two
# objects (a cube and a sphere), a camera and a sun, frames 1-3, PNG output.
# The fixtures used 8 samples at 64x36 (160x90 in 64-pixel tiles for the tiled
# one); the Tier-2 subtests raise the samples and resolution so each frame takes
# seconds, longer than the worker's progress throttle.
# Prints "SAVED <path>" once the .blend is written.
import os
import sys

import bpy

args = sys.argv[sys.argv.index("--") + 1 :]
out_blend, engine, samples, width, height = args[:5]
tile_size = int(args[5]) if len(args) > 5 else 0

bpy.ops.wm.read_factory_settings(use_empty=True)
scene = bpy.context.scene

cam = bpy.data.objects.new("Cam", bpy.data.cameras.new("Cam"))
cam.location = (0, -6, 0)
cam.rotation_euler = (1.5708, 0, 0)
scene.collection.objects.link(cam)
scene.camera = cam
light = bpy.data.objects.new("Sun", bpy.data.lights.new("Sun", "SUN"))
scene.collection.objects.link(light)
bpy.ops.mesh.primitive_cube_add(location=(-1, 0, 0))
bpy.ops.mesh.primitive_uv_sphere_add(location=(1, 0, 0))

scene.render.engine = engine
if engine == "CYCLES":
    scene.cycles.device = "CPU"
    scene.cycles.samples = int(samples)
    scene.cycles.use_adaptive_sampling = False
    scene.cycles.use_denoising = False
    if tile_size:
        scene.cycles.use_auto_tile = True
        scene.cycles.tile_size = tile_size
else:
    scene.eevee.taa_render_samples = int(samples)
scene.render.resolution_x = int(width)
scene.render.resolution_y = int(height)
scene.render.resolution_percentage = 100
scene.render.image_settings.file_format = "PNG"
scene.frame_start = 1
scene.frame_end = 3

os.makedirs(os.path.dirname(os.path.abspath(out_blend)), exist_ok=True)
bpy.ops.wm.save_as_mainfile(filepath=out_blend)
print("SAVED", out_blend)
