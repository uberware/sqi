# SPDX-License-Identifier: AGPL-3.0-or-later
"""``sqi-submit install-host``: copy a host's files into place.

The files for Blender, Cinema 4D, After Effects, Unreal, Katana, KeyShot, Silhouette, Modo and
Fusion ship inside this wheel as package data and use only the host's own standard library, so
installing is a file copy: no pip into the host's Python. VRED's plugin is the exception: it
runs ``sqi_submitter`` inside VRED's own Python, so that package has to be installed there too
(see the VRED docs). Where the documented location needs admin rights (After Effects) or is not
knowable (Blender's add-on is installed through its UI, Cinema 4D's preferences folder is
versioned) the command prints what to do instead of guessing.
"""

from __future__ import annotations

import os
import shutil
import sys
from collections.abc import Mapping
from importlib import resources
from pathlib import Path, PurePosixPath
from typing import TextIO

__all__ = [
    "HOOK_FILES",
    "HOOK_MARKER",
    "HOST_FILES",
    "KNOWN_HOSTS",
    "default_destination",
    "dest_relative",
    "install_host",
]

# Package-relative files per host.
HOST_FILES: dict[str, list[str]] = {
    "blender": ["hosts/blender/sqi_blender_addon.py"],
    "cinema4d": ["hosts/cinema4d/sqi_submit.pyp"],
    "aftereffects": ["hosts/aftereffects/Submit to sqi.jsx", "hosts/aftereffects/json2.js"],
    "unreal": ["hosts/unreal/sqi_unreal_submit.py", "hosts/unreal/init_unreal.py"],
    "katana": ["hosts/katana/sqi_katana_submit.py", "hosts/katana/init.py"],
    # KeyShot runs no startup file: the script is run from its Scripting Console.
    "keyshot": ["hosts/keyshot/sqi_keyshot_submit.py"],
    # Silhouette's startup script imports a module named sfxuser from its script search path.
    "silhouette": ["hosts/silhouette/sqi_silhouette_submit.py", "hosts/silhouette/sfxuser.py"],
    # Modo loads a kit (a folder with index.cfg), copied as <dest>/sqi/...; it has no hook file.
    "modo": ["hosts/modo/sqi/index.cfg", "hosts/modo/sqi/lxserv/sqi_modo_submit.py"],
    # Fusion bundles Lua but not Python: one Lua comp script, no hook file. The name has spaces
    # (it is the menu entry), and lands flat in the user's Scripts/Comp folder.
    "fusion": ["hosts/fusion/Submit to sqi.lua"],
    # VRED is in-process: this plugin may import sqi_submitter (installed into VRED's Python),
    # so it is not a handoff file. It lands as <dest>/SubmitToSqi/SubmitToSqi.py.
    "vred": ["hosts/vred/SubmitToSqi/SubmitToSqi.py"],
}

# Taken from HOST_FILES once, at import, so the two cannot drift; a patched HOST_FILES
# (tests swap in stand-in files) leaves the set of hosts this command accepts unchanged.
KNOWN_HOSTS: tuple[str, ...] = tuple(HOST_FILES)

# Startup files whose *name the host fixes* (Unreal's init_unreal.py, Katana's init.py,
# Silhouette's sfxuser.py), so a
# studio may already have its own. A shipped one carries HOOK_MARKER; install never overwrites
# a file of such a name that lacks the marker, it prints the lines to append instead.
HOOK_MARKER = "# sqi-submitter hook"
HOOK_FILES: frozenset[str] = frozenset({"init_unreal.py", "init.py", "sfxuser.py"})

_C4D_PREFS_GLOB = "Maxon Cinema 4D *"
_KEYSHOT_GLOB = "KeyShot*"
_VRED_GLOB = "VRED-*"

_NEXT_STEP = {
    "blender": "In Blender: Edit > Preferences > Add-ons > Install from Disk, choose the file "
    "above, then enable the add-on.",
    "cinema4d": "Restart Cinema 4D; the sqi command appears under Extensions.",
    "aftereffects": "Copy the files above into After Effects' Scripts/ScriptUI Panels folder "
    "(it needs administrator rights), restart After Effects, then open the panel from the "
    "Window menu.",
    "unreal": "Restart the Unreal Editor; Submit to sqi… appears in the Tools menu.",
    "katana": "Restart Katana; Submit to sqi… appears in the sqi menu.",
    "keyshot": "In KeyShot open Window > Scripting Console and run sqi_keyshot_submit.",
    "silhouette": "Restart Silhouette; Submit to sqi… appears in the Actions menu.",
    "modo": "Restart Modo; run sqi.submit from the command entry, or map it to a key.",
    "fusion": "In Fusion choose Script > Submit to sqi.",
    "vred": "Turn VRED's Python sandbox off, restart VRED; Submit to sqi… appears in the sqi menu.",
}


def dest_relative(host: str, rel: str) -> PurePosixPath:
    """Where ``rel`` (a package-relative file of ``host``) lands, relative to the destination.

    A file under ``hosts/<host>/`` keeps its path relative to that folder, so a host that
    needs a folder tree (Modo's kit, Fusion's Scripts) gets one. Any other file is copied
    flat under its basename.
    """
    path = PurePosixPath(rel)
    try:
        return path.relative_to(PurePosixPath("hosts") / host)
    except ValueError:
        return PurePosixPath(path.name)


def _cinema4d_prefs_root(platform: str, env: Mapping[str, str]) -> Path | None:
    if platform == "win32":
        appdata = env.get("APPDATA")
        return Path(appdata) / "Maxon" if appdata else None
    if platform == "darwin":
        home = env.get("HOME")
        return Path(home) / "Library" / "Preferences" / "Maxon" if home else None
    return None


def _only_folder(root: Path, pattern: str) -> Path | None:
    """The one folder under ``root`` matching ``pattern``; ``None`` for none or several."""
    if not root.is_dir():
        return None
    matches = [p for p in root.glob(pattern) if p.is_dir()]
    return matches[0] if len(matches) == 1 else None


def _modo_scripts_folder(platform: str, env: Mapping[str, str]) -> Path | None:
    if platform == "win32":
        appdata = env.get("APPDATA")
        return Path(appdata) / "Luxology" / "Scripts" if appdata else None
    home = env.get("HOME")
    if not home:
        return None
    if platform == "darwin":
        return Path(home) / "Library" / "Application Support" / "Luxology" / "Scripts"
    return Path(home) / ".luxology" / "Scripts"


def _fusion_scripts_folder(platform: str, env: Mapping[str, str]) -> Path | None:
    """Fusion's per-user ``Scripts/Comp`` folder (the ``Scripts:/Comp`` path map)."""
    leaf = Path("Blackmagic Design") / "Fusion" / "Scripts" / "Comp"
    # Linux spells the vendor folder without the space, as the Fusion Render Node's own
    # install folder does (/opt/BlackmagicDesign/...).
    linux_leaf = Path("BlackmagicDesign") / "Fusion" / "Scripts" / "Comp"
    if platform == "win32":
        appdata = env.get("APPDATA")
        return Path(appdata) / leaf if appdata else None
    home = env.get("HOME")
    if not home:
        return None
    if platform == "darwin":
        return Path(home) / "Library" / "Application Support" / leaf
    return Path(home) / ".fusion" / linux_leaf


def default_destination(host: str, platform: str, env: Mapping[str, str]) -> Path | None:
    """The per-user folder to copy into, or ``None`` when the artist must do it.

    Blender (installed through its UI) and After Effects (under Program Files, needs admin)
    always return ``None``. Unreal's is ``Documents/UnrealEngine/Python`` under the user's
    profile, which the Editor puts on its Python path. Katana's is ``.katana/Startup`` under the
    home folder on Windows and Linux (``None`` on macOS), where Katana runs ``init.py`` at
    startup. Cinema 4D's preferences folder carries a version suffix we cannot know, so it is
    returned only when exactly one ``Maxon Cinema 4D *`` folder exists. Silhouette's is
    ``scripts`` under ``SFX_USER_PATH`` when that variable is set, else ``None``. KeyShot's is the
    ``Scripts`` folder of its one ``KeyShot*`` folder in Documents (Windows and macOS only: KeyShot
    has no Linux desktop product), under the same one-folder rule. Modo's is ``Scripts`` in its
    per-user ``Luxology`` folder: ``%APPDATA%/Luxology`` on Windows, ``~/Library/Application
    Support/Luxology`` on macOS and ``~/.luxology`` on Linux. Fusion's is ``Scripts/Comp`` in
    its per-user ``Blackmagic Design/Fusion`` folder: under ``%APPDATA%`` on Windows, ``~/Library/
    Application Support`` on macOS and ``~/.fusion`` on Linux (where the vendor folder is spelled
    ``BlackmagicDesign``). VRED's is ``ScriptPlugins`` in its one ``VRED-*`` folder under
    ``Documents/Autodesk`` (Windows only: VRED Pro has no other platform).
    """
    if host == "unreal":
        home = env.get("USERPROFILE" if platform == "win32" else "HOME")
        return Path(home) / "Documents" / "UnrealEngine" / "Python" if home else None
    if host == "katana":
        # Katana does not run on macOS, so there is nothing to install there.
        if platform not in ("win32", "linux"):
            return None
        home = env.get("USERPROFILE" if platform == "win32" else "HOME")
        return Path(home) / ".katana" / "Startup" if home else None
    if host == "silhouette":
        # Silhouette searches $(SFX_USER_PATH)/scripts for scripts; the variable is the only
        # place that folder is knowable from, so without it the artist chooses.
        user_path = env.get("SFX_USER_PATH")
        return Path(user_path) / "scripts" if user_path else None
    if host == "keyshot":
        if platform not in ("win32", "darwin"):
            return None
        home = env.get("USERPROFILE" if platform == "win32" else "HOME")
        found = _only_folder(Path(home) / "Documents", _KEYSHOT_GLOB) if home else None
        return found / "Scripts" if found else None
    if host == "modo":
        return _modo_scripts_folder(platform, env)
    if host == "fusion":
        return _fusion_scripts_folder(platform, env)
    if host == "vred":
        # VRED Pro, the edition with a UI, runs on Windows only. Its per-user plugin folder is
        # versioned (VRED-<internal version>), so it is named only when exactly one exists.
        if platform != "win32" or not env.get("USERPROFILE"):
            return None
        found = _only_folder(Path(env["USERPROFILE"]) / "Documents" / "Autodesk", _VRED_GLOB)
        return found / "ScriptPlugins" if found else None
    if host != "cinema4d":
        return None
    root = _cinema4d_prefs_root(platform, env)
    found = _only_folder(root, _C4D_PREFS_GLOB) if root else None
    return found / "plugins" if found else None


def _is_foreign_hook(target: Path) -> bool:
    """True when ``target`` is a startup file the host fixes the name of, and it is not ours."""
    if target.name not in HOOK_FILES or not target.is_file():
        return False
    return HOOK_MARKER.encode("utf-8") not in target.read_bytes()


def _lines_to_add(hook: Path) -> str:
    """The shipped hook as lines to append to an artist's own startup file.

    The marker names the file sqi itself installs. Printed for the artist to append, it would
    make their own file look like ours, and the next install would overwrite it. A
    ``from __future__`` import is a SyntaxError anywhere but the top of a file, which is not
    where these lines land, so it is left out too (a shipped hook must not need one).
    """
    lines = hook.read_text(encoding="utf-8").splitlines()
    keep = [
        line
        for line in lines
        if line.strip() != HOOK_MARKER and not line.lstrip().startswith("from __future__ import")
    ]
    return "\n".join(keep).strip("\n")


def _no_destination_hint(host: str) -> str:
    if host == "blender":
        return "Blender add-ons are installed through its UI, so nothing was copied."
    if host == "cinema4d":
        return (
            "Could not tell which Cinema 4D preferences folder to use. In Cinema 4D choose "
            "Preferences > Open Preferences Folder, copy the files above into its plugins "
            "folder (or pass --dest DIR)."
        )
    if host == "unreal":
        return (
            "Could not tell where your Documents folder is. Copy the files above into "
            "<Documents>/UnrealEngine/Python (create it if needed; the Editor runs "
            "init_unreal.py from there at startup), or pass --dest DIR. If you already have "
            "an init_unreal.py there, add the lines of the shipped one to it instead."
        )
    if host == "katana":
        return (
            "Could not tell where your home folder is (Katana does not run on macOS). Copy the "
            "files above into <home>/.katana/Startup (create it if needed; Katana runs init.py "
            "from there at startup), or pass --dest DIR. If you already have an init.py there, "
            "add the lines of the shipped one to it instead."
        )
    if host == "keyshot":
        return (
            "Could not tell which KeyShot folder to use. Copy the file above into the Scripts "
            "folder of your KeyShot folder under Documents (for example "
            "<Documents>/KeyShot 2024/Scripts; create it if needed), or pass --dest DIR."
        )
    if host == "silhouette":
        return (
            "SFX_USER_PATH is not set, so there is no known user scripts folder. Copy the "
            "files above into the scripts folder under your Silhouette user folder "
            "($(SFX_USER_PATH)/scripts; create it if needed), or pass --dest DIR. If you "
            "already have an sfxuser.py there, add the lines of the shipped one to it instead."
        )
    if host == "modo":
        return (
            "Could not tell where Modo's per-user folder is. Copy the sqi folder (index.cfg and "
            "lxserv/sqi_modo_submit.py) shown above into the Scripts folder of your Luxology "
            "user folder (%APPDATA%/Luxology on Windows, ~/Library/Application "
            "Support/Luxology on macOS, ~/.luxology on Linux; create it if needed), or pass "
            "--dest DIR."
        )
    if host == "fusion":
        return (
            "Could not tell where Fusion's per-user folder is. Copy the file above into the "
            "Scripts/Comp folder of your Fusion user folder (the Scripts: path map in Fusion's "
            "Preferences shows it; for example %APPDATA%/Blackmagic Design/Fusion/Scripts/Comp "
            "on Windows; create it if needed), or pass --dest DIR."
        )
    if host == "vred":
        return (
            "Could not tell which VRED folder to use (VRED is Windows-only, and its folder is "
            "versioned). Copy the SubmitToSqi folder shown above into the ScriptPlugins folder "
            "of your VRED folder (<Documents>/Autodesk/VRED-<version>/ScriptPlugins; create it "
            "if needed), or pass --dest DIR."
        )
    return (
        "The After Effects scripts folder is under the program folder and needs "
        "administrator rights, so nothing was copied. Destination pattern: "
        "<After Effects install>/Support Files/Scripts/ScriptUI Panels."
    )


def install_host(host: str, dest: Path | None, out: TextIO, err: TextIO | None = None) -> int:
    """Install ``host``'s files into ``dest`` (or its default), reporting on ``out``.

    Returns 0 on success, 2 for an unknown host, 1 when the destination cannot be written
    (that error goes to ``err``, default ``out``).
    """
    if host not in KNOWN_HOSTS:
        print(f"unknown host {host!r}; known hosts: {', '.join(KNOWN_HOSTS)}", file=out)
        return 2
    files = HOST_FILES[host]

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
            target = dest.joinpath(*dest_relative(host, rel).parts)
            with resources.as_file(root.joinpath(rel)) as path:
                if _is_foreign_hook(target):
                    print(
                        f"kept your existing {target}; to add sqi, append these lines to it:",
                        file=out,
                    )
                    print(_lines_to_add(path), file=out)
                    continue
                target.parent.mkdir(parents=True, exist_ok=True)
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
