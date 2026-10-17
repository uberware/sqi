# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi Submitter for Blender: one operator that hands the open scene to the sqi dialog.

This is a single-file legacy add-on. Install it with Preferences > Add-ons > Install from
Disk (``sqi-submit install-host blender`` prints the file's location). It uses only
Blender's own Python and the standard library and never imports ``sqi_submitter``: it writes
a small context file describing the scene and runs ``sqi-submit --context FILE``, whose Qt
dialog does the rest.

``bpy`` is imported inside function bodies only, so the module imports (and its scene
reading can be tested) without Blender. The block between the ``sqi handoff`` markers is a
verbatim copy of ``sqi_submitter/hosts/_handoff_block.py``; edit it there and re-copy it.
"""

from collections.abc import Callable
from typing import Any

# isort: split

# --- sqi handoff begin ---
import contextlib
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile

NOT_FOUND_MESSAGE = (
    "sqi-submit was not found. Install it on this machine with: "
    'pip install "sqi-submitter[qt]" — then set its full path in the sqi '
    "add-on/plugin settings, or as sqi_submit_command in ~/.sqi/submitter.json."
)


def _settings_command() -> str:
    path = os.environ.get("SQI_SUBMITTER_SETTINGS") or os.path.join(
        os.path.expanduser("~"), ".sqi", "submitter.json"
    )
    try:
        # utf-8-sig: PowerShell 5 writes UTF-8 with a BOM, which plain utf-8 rejects.
        with open(path, encoding="utf-8-sig") as handle:
            value = json.load(handle).get("sqi_submit_command")
    except (OSError, ValueError, AttributeError):
        return ""
    return value if isinstance(value, str) else ""


def resolve_submit_command(configured: str) -> str:
    """Find ``sqi-submit``: the host's own setting, the settings file, then PATH."""
    if configured and os.path.exists(configured):
        return configured
    from_file = _settings_command()
    if from_file and os.path.exists(from_file):
        return from_file
    found = shutil.which("sqi-submit")
    if found:
        return found
    raise RuntimeError(NOT_FOUND_MESSAGE)


def _handoff_folder() -> str:
    """``<tmp>/sqi-handoff`` if it is a directory only this user can enter, else a new one.

    On Linux the temp folder is the shared ``/tmp``: another local user could create
    ``sqi-handoff`` first, open to all, and swap the context file (and with it the
    server the API key is sent to) before ``sqi-submit`` reads it.
    """
    base = tempfile.gettempdir()
    folder = os.path.join(base, "sqi-handoff")
    with contextlib.suppress(FileExistsError):  # not a directory: lstat below decides
        os.makedirs(folder, mode=0o700, exist_ok=True)
    try:
        info = os.lstat(folder)
    except OSError:
        return tempfile.mkdtemp(prefix="sqi-handoff-", dir=base)
    getuid = getattr(os, "getuid", None)  # POSIX only
    if not stat.S_ISDIR(info.st_mode) or (getuid is not None and info.st_uid != getuid()):
        # A symlink, a file, or someone else's folder: use a fresh private one.
        return tempfile.mkdtemp(prefix="sqi-handoff-", dir=base)
    if getuid is not None and info.st_mode & 0o077:
        os.chmod(folder, 0o700)
    return folder


def write_context(data: dict[str, object]) -> str:
    """Write the handoff context to a new private file in the temp folder; return its path."""
    # mkstemp creates the file exclusively and readable by this user only (0600).
    fd, path = tempfile.mkstemp(prefix="context-", suffix=".json", dir=_handoff_folder())
    with os.fdopen(fd, "w", encoding="utf-8") as handle:
        json.dump(data, handle)
    return path


def launch(command: str, context_path: str) -> None:
    """Start ``sqi-submit --context`` detached, and let its window take focus."""
    argv = [command, "--context", context_path]
    if sys.platform == "win32":
        flags = getattr(subprocess, "DETACHED_PROCESS", 0x00000008)
        flags |= getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0x00000200)
        proc = subprocess.Popen(argv, creationflags=flags, close_fds=True)
        try:
            import ctypes

            ctypes.windll.user32.AllowSetForegroundWindow(proc.pid)
        except Exception:
            pass
    else:
        subprocess.Popen(argv, start_new_session=True, close_fds=True)


# --- sqi handoff end ---

bl_info = {
    "name": "sqi Submitter",
    "author": "Uberware Inc.",
    "version": (0, 2, 0),
    "blender": (3, 0, 0),
    "location": "View3D > Sidebar > sqi",
    "description": "Submit render jobs to a sqi farm",
    "category": "Render",
}

__all__ = ["bl_info", "read_scene", "register", "submit_scene", "unregister"]

_UNSAVED_MESSAGE = "Save your scene before submitting — the farm renders the file on disk."

_classes: list[Any] = []  # registered bpy.types classes, in registration order


def _frame_range(start: float, end: float) -> str:
    """The ``Frames`` convention: ``"1-100"``, or ``"5"`` for a single frame."""
    lo, hi = int(start), int(end)
    return str(lo) if lo == hi else f"{lo}-{hi}"


def read_scene() -> dict[str, object]:
    """Describe the open scene as a version-1 handoff context document.

    Every scene's view layers are targets, each with its scene's frame range and output, and
    ``extra`` carrying the scene and layer names that ``blender-view-layer-render`` pre-fills.
    """
    import bpy

    scene = bpy.context.scene
    targets: list[dict[str, object]] = []
    for each in bpy.data.scenes:
        frame_range = _frame_range(each.frame_start, each.frame_end)
        for layer in each.view_layers:
            targets.append(
                {
                    "name": f"{each.name} / {layer.name}",
                    "kind": "view_layer",
                    "frame_range": frame_range,
                    "output_path": each.render.filepath,
                    "extra": {"Scene": each.name, "ViewLayer": layer.name},
                    "locked": [],
                }
            )
    return {
        "version": 1,
        "host": {
            "name": "blender",
            "display_name": "Blender",
            "version": bpy.app.version_string,
        },
        "scene": {
            "path": bpy.data.filepath,
            "frame_range": _frame_range(scene.frame_start, scene.frame_end),
            "output_path": scene.render.filepath,
            "renderer": scene.render.engine,
        },
        "targets": targets,
        "server_url": None,
    }


def submit_scene(report: Callable[[set[str], str], None], configured: str) -> set[str]:
    """Save if needed, write the context file and launch ``sqi-submit``.

    ``report`` is the operator's ``self.report``. Returns the operator result set.
    """
    import bpy

    if not bpy.data.filepath:
        report({"ERROR"}, _UNSAVED_MESSAGE)
        return {"CANCELLED"}
    try:
        if bpy.data.is_dirty:
            bpy.ops.wm.save_mainfile()
        command = resolve_submit_command(configured)
        launch(command, write_context(read_scene()))
    except RuntimeError as exc:
        report({"ERROR"}, str(exc))
        return {"CANCELLED"}
    return {"FINISHED"}


def _configured_command(context: Any) -> str:
    """The ``sqi-submit`` path from this add-on's preferences, or ``""``."""
    try:
        prefs = context.preferences.addons[__name__].preferences
        value = prefs.sqi_submit_command
    except (AttributeError, KeyError):
        return ""
    return value if isinstance(value, str) else ""


def _make_classes() -> list[Any]:
    """Build the operator, panel and preferences classes; called from :func:`register`.

    They subclass ``bpy.types`` bases and so cannot exist without ``bpy``. The preferences
    property is a runtime ``__annotations__`` entry, which is what ``register_class`` reads.
    """
    import bpy

    class SQI_OT_submit(bpy.types.Operator):  # type: ignore[misc]
        bl_idname = "sqi.submit"
        bl_label = "Submit to sqi"
        bl_description = "Open the sqi submit dialog for this scene"

        def execute(self, context: Any) -> set[str]:
            return submit_scene(self.report, _configured_command(context))

    class SQI_PT_panel(bpy.types.Panel):  # type: ignore[misc]
        bl_idname = "SQI_PT_panel"
        bl_label = "sqi Submitter"
        bl_space_type = "VIEW_3D"
        bl_region_type = "UI"
        bl_category = "sqi"

        def draw(self, context: Any) -> None:
            self.layout.operator(SQI_OT_submit.bl_idname, icon="EXPORT")

    def draw_preferences(self: Any, context: Any) -> None:
        self.layout.prop(self, "sqi_submit_command")

    preferences = type(
        "SQI_Preferences",
        (bpy.types.AddonPreferences,),
        {
            "bl_idname": __name__,
            "__annotations__": {
                "sqi_submit_command": bpy.props.StringProperty(
                    name="sqi-submit path",
                    description="Full path to the sqi-submit program; leave empty to find it "
                    "automatically",
                    subtype="FILE_PATH",
                )
            },
            "draw": draw_preferences,
        },
    )
    return [SQI_OT_submit, SQI_PT_panel, preferences]


def register() -> None:
    import bpy

    _classes[:] = _make_classes()
    for cls in _classes:
        bpy.utils.register_class(cls)


def unregister() -> None:
    import bpy

    for cls in reversed(_classes):
        bpy.utils.unregister_class(cls)
    _classes.clear()
