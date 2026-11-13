# SPDX-License-Identifier: AGPL-3.0-or-later
# Builds the content TestPresetDCC_Unreal_MRQRender renders, inside a project
# whose .uproject enables the Movie Render Queue, Python and editor scripting
# plug-ins. Run by the test as:
#   UnrealEditor-Cmd <project> -run=pythonscript -script=unreal_project.py
# with SQI_BUILD_LOG naming a file this script appends its progress to; it ends
# with "BUILD OK", or "BUILD FAILED" and the traceback. The commandlet's own
# exit status does not report a Python exception, hence the log.
#
# Content:
#   /Game/Maps/Test                a cube and a directional light
#   /Game/Cinematics/SEQ_Single    3 frames, no shot track
#   /Game/Cinematics/SEQ_Shots     shots SH010 and SH020, 2 frames each
#   /Game/Cinematics/MRQ_Test      64x36 PNG frames named
#                                  {sequence_name}.{shot_name}.{frame_number}
#   /Game/Cinematics/Q_Test        a saved queue of one job: SEQ_Single
import os
import traceback

import unreal

LOG = os.environ["SQI_BUILD_LOG"]


def say(msg):
    with open(LOG, "a", encoding="utf-8") as f:
        f.write(msg + "\n")


def save(asset):
    if not unreal.EditorAssetLibrary.save_loaded_asset(asset, only_if_is_dirty=False):
        raise RuntimeError("could not save " + asset.get_path_name())


def sequence(tools, name, end):
    seq = tools.create_asset(name, "/Game/Cinematics", unreal.LevelSequence, unreal.LevelSequenceFactoryNew())
    seq.set_display_rate(unreal.FrameRate(24, 1))
    seq.set_playback_start(0)
    seq.set_playback_end(end)
    return seq


try:
    tools = unreal.AssetToolsHelpers.get_asset_tools()
    levels = unreal.get_editor_subsystem(unreal.LevelEditorSubsystem)
    actors = unreal.get_editor_subsystem(unreal.EditorActorSubsystem)

    levels.new_level("/Game/Maps/Test")
    cube = unreal.EditorAssetLibrary.load_asset("/Engine/BasicShapes/Cube.Cube")
    # From the class, then given its mesh: spawn_actor_from_object(cube, ...)
    # places the asset through the level editor's placement subsystem, which a
    # commandlet does not have; on UE 5.7 on macOS it crashes with a SIGSEGV in
    # UPlacementSubsystem::FindAssetFactoryFromAssetData.
    cube_actor = actors.spawn_actor_from_class(unreal.StaticMeshActor, unreal.Vector(400, 0, 0))
    cube_actor.static_mesh_component.set_static_mesh(cube)
    actors.spawn_actor_from_class(unreal.DirectionalLight, unreal.Vector(0, 0, 500))
    levels.save_current_level()

    save(sequence(tools, "SEQ_Single", 3))

    shots = sequence(tools, "SEQ_Shots", 4)
    track = shots.add_track(unreal.MovieSceneCinematicShotTrack)
    for i, name in enumerate(["SH010", "SH020"]):
        sub = sequence(tools, "SEQ_" + name, 2)
        save(sub)
        section = track.add_section()
        section.set_sequence(sub)
        section.set_range(i * 2, i * 2 + 2)
        section.set_shot_display_name(name)
    save(shots)

    config = tools.create_asset(
        "MRQ_Test", "/Game/Cinematics", unreal.MoviePipelinePrimaryConfig, unreal.MoviePipelinePrimaryConfigFactory()
    )
    output = config.find_or_add_setting_by_class(unreal.MoviePipelineOutputSetting)
    output.output_resolution = unreal.IntPoint(64, 36)
    output.file_name_format = "{sequence_name}.{shot_name}.{frame_number}"
    config.find_or_add_setting_by_class(unreal.MoviePipelineDeferredPassBase)
    config.find_or_add_setting_by_class(unreal.MoviePipelineImageSequenceOutput_PNG)
    save(config)

    queue = tools.create_asset("Q_Test", "/Game/Cinematics", unreal.MoviePipelineQueue, None)
    if queue is None:
        raise RuntimeError("create_asset returned no queue")
    job = queue.allocate_new_job(unreal.MoviePipelineExecutorJob)
    job.sequence = unreal.SoftObjectPath("/Game/Cinematics/SEQ_Single.SEQ_Single")
    job.map = unreal.SoftObjectPath("/Game/Maps/Test.Test")
    job.set_configuration(config)
    save(queue)
    say("BUILD OK")
except Exception:
    say("BUILD FAILED\n" + traceback.format_exc())
