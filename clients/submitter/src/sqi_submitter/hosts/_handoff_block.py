# SPDX-License-Identifier: AGPL-3.0-or-later
"""The canonical handoff launch block for hosts that cannot run Qt.

Blender and Cinema 4D bundle their own Python and cannot import ``sqi_submitter``,
so each carries a verbatim copy of the text between the two markers below.
``tests/test_handoff_block.py::test_block_identical_in_every_host`` keeps the
copies identical to this file. Edit the block here first, then re-copy it.

The block is standard library only and stays compatible with Python 3.9. Its
imports sit inside the markers so a copy is complete without any import outside
them. Windows-only calls are guarded so it imports and runs on every platform.
"""

# --- sqi handoff begin ---
import json
import os
import shutil
import subprocess
import sys
import tempfile
import uuid

NOT_FOUND_MESSAGE = (
    "sqi-submit was not found. Install it on this machine with: "
    "pip install 'sqi-submitter[qt]' — then set its full path in the sqi "
    "add-on/plugin settings, or as sqi_submit_command in ~/.sqi/submitter.json."
)


def _settings_command() -> str:
    path = os.environ.get("SQI_SUBMITTER_SETTINGS") or os.path.join(
        os.path.expanduser("~"), ".sqi", "submitter.json"
    )
    try:
        with open(path, encoding="utf-8") as handle:
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


def write_context(data: dict[str, object]) -> str:
    """Write the handoff context to a new file in the temp folder; return its path."""
    folder = os.path.join(tempfile.gettempdir(), "sqi-handoff")
    os.makedirs(folder, exist_ok=True)
    path = os.path.join(folder, "context-" + uuid.uuid4().hex + ".json")
    with open(path, "w", encoding="utf-8") as handle:
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
