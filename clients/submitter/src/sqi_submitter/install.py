# SPDX-License-Identifier: AGPL-3.0-or-later
"""``sqi-submit install-host``: copy a handoff host's files into place.

The files for Blender, Cinema 4D and After Effects ship inside this wheel as package data
and use only the host's own standard library, so installing is a file copy: no pip into the
host's Python. Where the documented location needs admin rights (After Effects) or is not
knowable (Blender's add-on is installed through its UI, Cinema 4D's preferences folder is
versioned) the command prints what to do instead of guessing.
"""

from __future__ import annotations

import os
import shutil
import sys
from collections.abc import Mapping
from importlib import resources
from pathlib import Path
from typing import TextIO

__all__ = ["HOST_FILES", "KNOWN_HOSTS", "default_destination", "install_host"]

KNOWN_HOSTS: tuple[str, ...] = ("blender", "cinema4d", "aftereffects")

# Package-relative files per host. The host tasks add their entries as the files land.
HOST_FILES: dict[str, list[str]] = {
    "blender": ["hosts/blender/sqi_blender_addon.py"],
    "cinema4d": ["hosts/cinema4d/sqi_submit.pyp"],
}

_C4D_PREFS_GLOB = "Maxon Cinema 4D *"

_NEXT_STEP = {
    "blender": "In Blender: Edit > Preferences > Add-ons > Install from Disk, choose the file "
    "above, then enable the add-on.",
    "cinema4d": "Restart Cinema 4D; the sqi command appears under Extensions.",
    "aftereffects": "Copy the files above into After Effects' Scripts/ScriptUI Panels folder "
    "(it needs administrator rights), restart After Effects, then open the panel from the "
    "Window menu.",
}


def _cinema4d_prefs_root(platform: str, env: Mapping[str, str]) -> Path | None:
    if platform == "win32":
        appdata = env.get("APPDATA")
        return Path(appdata) / "Maxon" if appdata else None
    if platform == "darwin":
        home = env.get("HOME")
        return Path(home) / "Library" / "Preferences" / "Maxon" if home else None
    return None


def default_destination(host: str, platform: str, env: Mapping[str, str]) -> Path | None:
    """The per-user folder to copy into, or ``None`` when the artist must do it.

    Blender (installed through its UI) and After Effects (under Program Files, needs admin)
    always return ``None``. Cinema 4D's preferences folder carries a version suffix we cannot
    know, so it is returned only when exactly one ``Maxon Cinema 4D *`` folder exists.
    """
    if host != "cinema4d":
        return None
    root = _cinema4d_prefs_root(platform, env)
    if root is None or not root.is_dir():
        return None
    matches = [p for p in root.glob(_C4D_PREFS_GLOB) if p.is_dir()]
    if len(matches) != 1:
        return None
    return matches[0] / "plugins"


def _no_destination_hint(host: str) -> str:
    if host == "blender":
        return "Blender add-ons are installed through its UI, so nothing was copied."
    if host == "cinema4d":
        return (
            "Could not tell which Cinema 4D preferences folder to use. In Cinema 4D choose "
            "Preferences > Open Preferences Folder, copy the files above into its plugins "
            "folder (or pass --dest DIR)."
        )
    return (
        "The After Effects scripts folder is under the program folder and needs "
        "administrator rights, so nothing was copied. Destination pattern: "
        "<After Effects install>/Support Files/Scripts/ScriptUI Panels."
    )


def install_host(host: str, dest: Path | None, out: TextIO, err: TextIO | None = None) -> int:
    """Install ``host``'s files into ``dest`` (or its default), reporting on ``out``.

    Returns 0 on success, 2 for an unknown host, 1 when this build ships no files for it or
    the destination cannot be written (that error goes to ``err``, default ``out``).
    """
    if host not in KNOWN_HOSTS:
        print(f"unknown host {host!r}; known hosts: {', '.join(KNOWN_HOSTS)}", file=out)
        return 2
    files = HOST_FILES.get(host)
    if not files:
        print(f"no files shipped for {host} in this build", file=out)
        return 1

    root = resources.files("sqi_submitter")
    if dest is None:
        dest = default_destination(host, sys.platform, os.environ)
    if dest is None:
        # Nothing to copy to: show where the files are so the artist can place them.
        print(f"Files for {host}:", file=out)
        for rel in files:
            with resources.as_file(root.joinpath(rel)) as path:
                print(f"  {path}", file=out)
        print(_no_destination_hint(host), file=out)
        print(_NEXT_STEP[host], file=out)
        return 0

    copied: list[Path] = []
    try:
        dest.mkdir(parents=True, exist_ok=True)
        for rel in files:
            target = dest / Path(rel).name
            with resources.as_file(root.joinpath(rel)) as path:
                shutil.copyfile(path, target)
            copied.append(target)
            print(f"copied {target}", file=out)
    except OSError as exc:
        reason = exc.strerror or str(exc)
        message = f"could not install into {dest}: {reason}"
        if copied:
            message += f" ({len(copied)} file(s) already copied were left in place)"
        else:
            message += " (nothing was installed)"
        print(message, file=err if err is not None else out)
        return 1
    print(_NEXT_STEP[host], file=out)
    return 0
