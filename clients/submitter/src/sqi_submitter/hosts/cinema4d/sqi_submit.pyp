# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi Submitter for Cinema 4D: one command that hands the open scene to the sqi dialog.

Install it by copying this file into Cinema 4D's user ``plugins`` folder
(``sqi-submit install-host cinema4d``), then restart Cinema 4D; the command appears under
Extensions. It uses only Cinema 4D's own Python and the standard library and never imports
``sqi_submitter``: it writes a small context file describing the scene and runs
``sqi-submit --context FILE``, whose Qt dialog does the rest.

``c4d`` is imported inside function bodies only, so the module imports (and its scene reading
can be tested) without Cinema 4D. The block between the ``sqi handoff`` markers is a verbatim
copy of ``sqi_submitter/hosts/_handoff_block.py``; edit it there and re-copy it.
"""

from __future__ import annotations

from collections.abc import Iterator
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


# Cinema 4D plugin IDs must be registered with Maxon; 1000001 is from the range Maxon documents
# for testing and must never ship.
# TODO(release): replace with an ID registered at developers.maxon.net before release
PLUGIN_ID = 1000001

_UNSAVED_MESSAGE = "Save your scene before submitting — the farm renders the file on disk."
_SAVE_FAILED_MESSAGE = "Cinema 4D could not save the scene, so nothing was submitted."


def _frames(base_time: Any, fps: int) -> int:
    """The frame number of a ``c4d.BaseTime``."""
    return int(base_time.GetFrame(fps))


def frame_range_from(
    mode: int,
    frm: int,
    to: int,
    doc_min: int,
    doc_max: int,
    preview_min: int,
    preview_max: int,
    current: int,
) -> str:
    """The ``Frames`` convention (``"1-100"``, or ``"5"``) for a render data's frame mode."""
    import c4d

    if mode == c4d.RDATA_FRAMESEQUENCE_CURRENTFRAME:
        low = high = current
    elif mode == c4d.RDATA_FRAMESEQUENCE_ALLFRAMES:
        low, high = doc_min, doc_max
    elif mode == c4d.RDATA_FRAMESEQUENCE_PREVIEWRANGE:
        low, high = preview_min, preview_max
    else:  # RDATA_FRAMESEQUENCE_MANUAL
        low, high = frm, to
    low, high = min(low, high), max(low, high)
    return str(low) if low == high else f"{low}-{high}"


def _render_frames(doc: Any, render_data: Any) -> str:
    import c4d

    fps = doc.GetFps()
    return frame_range_from(
        int(render_data[c4d.RDATA_FRAMESEQUENCE]),
        _frames(render_data[c4d.RDATA_FRAMEFROM], fps),
        _frames(render_data[c4d.RDATA_FRAMETO], fps),
        _frames(doc.GetMinTime(), fps),
        _frames(doc.GetMaxTime(), fps),
        _frames(doc.GetLoopMinTime(), fps),
        _frames(doc.GetLoopMaxTime(), fps),
        _frames(doc.GetTime(), fps),
    )


def _render_output(doc: Any, render_data: Any, take: Any = None) -> str | None:
    """The image path with ``$tokens`` expanded where Cinema 4D can; ``None`` if none is saved."""
    import c4d

    if not render_data[c4d.RDATA_SAVEIMAGE]:
        return None
    path = render_data[c4d.RDATA_PATH]
    if not isinstance(path, str) or not path:
        return None
    params = {"_doc": doc, "_rData": render_data, "_rBc": render_data.GetData()}
    if take is not None:
        params["_take"] = take
    try:
        expanded = c4d.modules.tokensystem.FilenameConvertTokens(path, params)
    except Exception:
        return path
    return expanded if isinstance(expanded, str) and expanded else path


def _walk_takes(take: Any) -> Iterator[Any]:
    """``take`` and its following siblings, each followed by its children, depth first."""
    while take is not None:
        yield take
        yield from _walk_takes(take.GetDown())
        take = take.GetNext()


def read_scene(doc: Any) -> dict[str, object]:
    """Describe ``doc`` as a version-1 handoff context document.

    Every checked take except the main take is a target, with the frame range and output of its
    own effective render data (a take that overrides none inherits the main one's).
    """
    import c4d

    document_path = doc.GetDocumentPath()
    scene_path = os.path.join(document_path, doc.GetDocumentName()) if document_path else None
    active = doc.GetActiveRenderData()
    take_data = doc.GetTakeData()
    targets: list[dict[str, object]] = []
    for take in _walk_takes(take_data.GetMainTake().GetDown()):
        if take.IsMain() or not take.IsChecked():
            continue
        render_data = take_data.GetEffectiveRenderData(take)[0]
        name = take.GetName()
        targets.append(
            {
                "name": name,
                "kind": "take",
                "frame_range": _render_frames(doc, render_data),
                "output_path": _render_output(doc, render_data, take),
                "extra": {"Take": name},
                "locked": [],
            }
        )
    return {
        "version": 1,
        "host": {
            "name": "cinema4d",
            "display_name": "Cinema 4D",
            "version": str(c4d.GetC4DVersion() // 1000),
        },
        "scene": {
            "path": scene_path,
            "frame_range": _render_frames(doc, active),
            "output_path": _render_output(doc, active),
            "renderer": None,
        },
        "targets": targets,
        "server_url": None,
    }


def submit_document(doc: Any) -> bool:
    """Save if needed, write the context file and launch ``sqi-submit``.

    Anything the artist must act on is shown in a dialog rather than raised into Cinema 4D's
    console. Returns whether the dialog was launched.
    """
    import c4d

    document_path = doc.GetDocumentPath()
    if not document_path:
        c4d.gui.MessageDialog(_UNSAVED_MESSAGE)
        return False
    try:
        if doc.GetChanged():
            path = os.path.join(document_path, doc.GetDocumentName())
            if not c4d.documents.SaveDocument(
                doc, path, c4d.SAVEDOCUMENTFLAGS_NONE, c4d.FORMAT_C4DEXPORT
            ):
                c4d.gui.MessageDialog(_SAVE_FAILED_MESSAGE)
                return False
        command = resolve_submit_command("")
        launch(command, write_context(read_scene(doc)))
    except (RuntimeError, OSError) as exc:
        c4d.gui.MessageDialog(str(exc))
        return False
    return True


def register() -> bool:
    """Register the command; Cinema 4D lists it under Extensions."""
    import c4d

    class SubmitCommand(c4d.plugins.CommandData):  # type: ignore[misc]
        def Execute(self, doc: Any) -> bool:
            return submit_document(doc)

    return bool(
        c4d.plugins.RegisterCommandPlugin(
            id=PLUGIN_ID,
            str="Submit to sqi…",
            info=0,
            icon=None,
            help="Submit this scene to the sqi farm",
            dat=SubmitCommand(),
        )
    )


if __name__ == "__main__":  # Cinema 4D runs a .pyp as __main__; tests and tools import it.
    register()
