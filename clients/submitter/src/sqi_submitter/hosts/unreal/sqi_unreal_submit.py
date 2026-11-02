# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi Submitter for the Unreal Editor: one Tools menu entry that hands the Movie Render Queue to
the sqi dialog.

Install it with ``sqi-submit install-host unreal``, which copies this file and its startup hook
``init_unreal.py`` into ``Documents/UnrealEngine/Python`` (a folder the Editor puts on
``sys.path`` and scans for ``init_unreal.py``), then restart the Editor; Submit to sqi...
appears in the Tools menu. This file uses only the Editor's own Python and the standard library
and never imports ``sqi_submitter``: it writes a small context file describing the enabled queue
jobs and runs ``sqi-submit --context FILE``, whose Qt dialog does the rest.

``unreal`` is imported inside function bodies only, so the module imports (and its queue reading
can be tested) without the Editor. The block between the ``sqi handoff`` markers is a verbatim
copy of ``sqi_submitter/hosts/_handoff_block.py``; edit it there and re-copy it.
"""

from __future__ import annotations

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
    'pip install "sqi-submitter[qt]" — then set its full path as '
    "sqi_submit_command in ~/.sqi/submitter.json."
)


class SubmitCommandNotFound(RuntimeError):
    """``sqi-submit`` is in none of the places ``resolve_submit_command`` looks."""


def _settings_value(key: str) -> str:
    """The string ``key`` holds in ``~/.sqi/submitter.json``, or ``""``."""
    # Match sqi-submit's Path.home(): on Windows that is USERPROFILE. Python 3.7's expanduser
    # prefers HOME, which is often a network share there.
    home = (os.environ.get("USERPROFILE") if sys.platform == "win32" else None) or (
        os.path.expanduser("~")
    )
    path = os.environ.get("SQI_SUBMITTER_SETTINGS") or os.path.join(home, ".sqi", "submitter.json")
    try:
        # utf-8-sig: PowerShell 5 writes UTF-8 with a BOM, which plain utf-8 rejects.
        with open(path, encoding="utf-8-sig") as handle:
            value = json.load(handle).get(key)
    except (OSError, ValueError, AttributeError):
        return ""
    return value if isinstance(value, str) else ""


def resolve_submit_command(configured: str) -> str:
    """Find ``sqi-submit``: the host's own setting, the settings file, then PATH."""
    if configured and os.path.exists(configured):
        return configured
    from_file = _settings_value("sqi_submit_command")
    if from_file and os.path.exists(from_file):
        return from_file
    found = shutil.which("sqi-submit")
    if found:
        return found
    raise SubmitCommandNotFound(NOT_FOUND_MESSAGE)


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


def windowed_command(command: str) -> str:
    """The GUI-subsystem ``sqi-submitw.exe`` beside a Windows ``sqi-submit.exe``, if any.

    ``sqi-submit.exe`` is a console launcher: even started detached, the ``python.exe`` it
    runs gets a console window of its own, open for as long as the dialog is.
    ``sqi-submitw.exe`` runs ``pythonw.exe``, which has none. Anything else (a ``.cmd``
    wrapper, an install from before ``sqi-submitw`` existed) is used as it is.
    """
    folder, name = os.path.split(command)
    if name.lower() != "sqi-submit.exe":
        return command
    windowed = os.path.join(folder, "sqi-submitw.exe")
    return windowed if os.path.isfile(windowed) else command


def launch(command: str, context_path: str) -> None:
    """Start ``sqi-submit --context`` detached, and let its window take focus."""
    if sys.platform == "win32":
        command = windowed_command(command)
    argv = [command, "--context", context_path]
    if sys.platform == "win32":
        flags = getattr(subprocess, "DETACHED_PROCESS", 0x00000008)
        flags |= getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0x00000200)
        subprocess.Popen(argv, creationflags=flags, close_fds=True)
        try:
            import ctypes

            # ASFW_ANY (-1): a pip/pipx/uv sqi-submit.exe runs python.exe as a child of its own.
            ctypes.windll.user32.AllowSetForegroundWindow(-1)
        except Exception:
            pass
    else:
        subprocess.Popen(argv, start_new_session=True, close_fds=True)


# --- sqi handoff end ---

_SAVE_FIRST = "Save your changes before submitting — the farm renders the saved project."
_REFUSAL_HEAD = "These queue jobs cannot render on the farm and were left out:\n"
_NO_JOBS = (
    "The Movie Render Queue has no enabled jobs. Add or enable a job (or load a saved queue) "
    "in Window > Cinematics > Movie Render Queue, then submit again."
)
_NONE_RENDERABLE_HEAD = (
    "None of the queue's enabled jobs can render on the farm, so nothing was submitted:"
)
_MAX_SHOTS = 1000  # the unreal-mrq-shot-render preset's limits on Shots
_MAX_SHOT_NAME = 256
_TOOLS_MENU = "LevelEditor.MainMenu.Tools"
_MENU_COMMAND = "import sqi_unreal_submit; sqi_unreal_submit.submit()"


def asset_path_problem(path: str) -> str | None:
    """Why ``path`` cannot name an asset another machine can load, or ``None`` if it can."""
    if not path or path.startswith(("/Temp/", "/Engine/Transient")):
        return "is not saved"
    if path.startswith("/"):
        return None
    return "is not an asset path"


def _object_path(path: str) -> str:
    """``Package.Asset`` for a path that may name only the package (``/Game/Seq/S1``)."""
    leaf = path.rsplit("/", 1)[-1]
    return path if "." in leaf else path + "." + leaf


def _asset_name(path: str) -> str:
    """The asset's own name: ``/Game/Cinematics/SEQ_Hero.SEQ_Hero`` is ``SEQ_Hero``."""
    return path.rsplit("/", 1)[-1].split(".")[0]


def enabled_shots(job: Any, sequence: str) -> tuple[list[str], str | None]:
    """The names of the job's enabled shots, or ``([], "<why they cannot be sent>")``.

    The job's shot list is rebuilt from its level sequence first, as the farm does before it
    picks a shot, so both sides see the same entries. A shot is named by its outer name: its
    section's name in the shot track, or "no shot" in a sequence without one (Unreal 5.8). An
    entry with no outer name, which 5.8 never gives, is named after the sequence. A name
    repeated by several camera cuts is one shot.
    """
    import unreal

    asset = unreal.EditorAssetLibrary.load_asset(_object_path(sequence))
    if asset is None:
        return [], "its level sequence could not be loaded"
    unreal.MoviePipelineLibrary.update_job_shot_list_from_sequence(asset, job)
    fallback = _asset_name(sequence)
    names: list[str] = []
    seen: set[str] = set()
    for entry in job.shot_info:
        if not entry.enabled:
            continue
        name = str(entry.outer_name or "") or fallback
        if name not in seen:
            seen.add(name)
            names.append(name)
    if not names:
        return [], "no enabled shots"
    if len(names) > _MAX_SHOTS:
        return [], f"more than {_MAX_SHOTS} shots"
    if any(len(name) > _MAX_SHOT_NAME for name in names):
        return [], f"shot name longer than {_MAX_SHOT_NAME} characters"
    return names, None


def job_target(job: Any) -> tuple[dict[str, object] | None, str | None]:
    """One enabled queue job as a context target, or ``(None, "<job name>: <reason>")``.

    A job whose render settings were edited in the queue without being saved back to a config
    asset, or whose map or level sequence is not a saved asset, would render something else (or
    nothing) on another machine, so it is left out and named rather than offered silently.
    """
    name = str(job.job_name) or "Unnamed job"
    config = job.get_preset_origin()
    if config is None:
        return None, name + ": its render settings are not saved as a config asset"
    map_path = str(job.map.export_text())
    sequence = str(job.sequence.export_text())
    for label, value in (("map", map_path), ("level sequence", sequence)):
        problem = asset_path_problem(value)
        if problem is not None:
            return None, f"{name}: its {label} {problem}"
    extra: dict[str, str] = {
        "LevelSequence": sequence,
        "Map": map_path,
        "MovieRenderConfig": str(config.get_path_name()),
    }
    try:
        shots, shots_problem = enabled_shots(job, sequence)
    except Exception as exc:
        # The shot read calls Editor APIs no other product needs: if they fail, the job is
        # still offered, without Shots, rather than blocking the job and queue presets too.
        import unreal

        unreal.log_warning(f"sqi: could not read the shots of {name}: {exc}")
    else:
        if shots_problem is not None:
            return None, f"{name}: {shots_problem}"
        extra["Shots"] = json.dumps(shots)
    target: dict[str, object] = {
        "name": name,
        "kind": "mrq_job",
        "frame_range": None,
        "output_path": None,
        "extra": extra,
        "locked": [],
    }
    return target, None


def read_scene() -> tuple[dict[str, object], list[str]]:
    """Describe the Movie Render Queue as a version-1 handoff context document.

    Returns the document and, one per excluded job, the reason it cannot render on the farm.
    """
    import unreal

    subsystem = unreal.get_editor_subsystem(unreal.MoviePipelineQueueSubsystem)
    targets: list[dict[str, object]] = []
    reasons: list[str] = []
    for job in subsystem.get_queue().get_jobs():
        if not job.is_enabled():
            continue
        target, reason = job_target(job)
        if target is not None:
            targets.append(target)
        if reason is not None:
            reasons.append(reason)
    project = unreal.Paths.convert_relative_path_to_full(unreal.Paths.get_project_file_path())
    version = str(unreal.SystemLibrary.get_engine_version()).split("-", 1)[0]
    data: dict[str, object] = {
        "version": 1,
        "host": {"name": "unreal", "display_name": "Unreal Editor", "version": version},
        "scene": {"path": project, "frame_range": None, "output_path": None, "renderer": None},
        "targets": targets,
        "server_url": None,
    }
    return data, reasons


def _any_dirty() -> bool:
    import unreal

    utils = unreal.EditorLoadingAndSavingUtils
    return len(utils.get_dirty_map_packages()) > 0 or len(utils.get_dirty_content_packages()) > 0


def _show(text: str, buttons: Any) -> Any:
    import unreal

    return unreal.EditorDialog.show_message("sqi", text, buttons)


def submit() -> bool:
    """Save what is unsaved, write the context file and launch ``sqi-submit``.

    Anything the artist must act on is shown in a dialog rather than raised into the Editor's
    log. Returns whether the dialog was launched.
    """
    import unreal

    try:
        if _any_dirty():
            unreal.EditorLoadingAndSavingUtils.save_dirty_packages_with_dialog(True, True)
            if _any_dirty():
                _show(_SAVE_FIRST, unreal.AppMsgType.OK)
                return False
        data, reasons = read_scene()
    except Exception as exc:
        # These call Editor APIs run only on Unreal 5.8, which may differ in other versions:
        # whatever they raise is shown to the artist, not left to reach the Editor's log.
        _show(f"sqi could not read the scene: {exc}", unreal.AppMsgType.OK)
        return False
    targets = data["targets"]
    if not targets:
        # Nothing to submit: say why and stop, rather than open a dialog with an empty list.
        if reasons:
            _show("\n".join([_NONE_RENDERABLE_HEAD, *reasons]), unreal.AppMsgType.OK)
        else:
            _show(_NO_JOBS, unreal.AppMsgType.OK)
        return False
    try:
        if reasons:
            answer = _show(_REFUSAL_HEAD + "\n".join(reasons), unreal.AppMsgType.OK_CANCEL)
            if answer != unreal.AppReturnType.OK:
                return False
        command = resolve_submit_command("")
        launch(command, write_context(data))
    except (RuntimeError, OSError) as exc:
        _show(str(exc), unreal.AppMsgType.OK)
        return False
    return True


def register_menu() -> None:
    """Add Submit to sqi... to the Level Editor's Tools menu."""
    import unreal

    menus = unreal.ToolMenus.get()
    menu = menus.find_menu(_TOOLS_MENU)
    if menu is None:
        unreal.log_warning("sqi: the Tools menu was not found, so Submit to sqi… was not added.")
        return
    entry = unreal.ToolMenuEntry(name="SqiSubmit", type=unreal.MultiBlockType.MENU_ENTRY)
    entry.set_label("Submit to sqi…")
    entry.set_string_command(unreal.ToolMenuStringCommandType.PYTHON, "", _MENU_COMMAND)
    menu.add_menu_entry("sqi", entry)
    menus.refresh_all_widgets()
