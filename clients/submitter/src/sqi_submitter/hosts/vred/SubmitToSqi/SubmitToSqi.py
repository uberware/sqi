# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi's VRED script plugin: adds sqi > Submit to sqi... to VRED's menu bar.

Unlike the handoff host files, this one runs sqi_submitter inside VRED's own Python, so the
package has to be importable there (VRED supplies Qt itself; never install the ``[qt]`` extra
into it). The import happens lazily, on the first click, so a VRED without it still starts
and the artist gets a message naming the fix instead of a traceback in VRED's terminal.

VRED puts its V2 services (``vrFileIOService``, ``vrCameraService``, ``vrMainWindow``) in
``builtins`` and makes ``vrRenderSettings`` an importable module. Both are read only when the
menu is used or built, so this file loads in a plain interpreter too.
"""

from __future__ import annotations

import builtins
import importlib
import json
import os
import sys
from typing import Any

MENU_TITLE = "sqi"
ACTION_TEXT = "Submit to sqi…"
DIALOG_TITLE = "Submit to sqi"

MISSING_MESSAGE = (
    "sqi-submitter could not be loaded in VRED's Python ({reason}).\n\n"
    "1. Turn VRED's Python sandbox off: Edit > Preferences > General Settings > Script, "
    "untick 'Enable Python Sandbox', then restart VRED. With the sandbox on, VRED blocks the "
    "imports the submitter needs.\n\n"
    "2. Install sqi-submitter into VRED's Python (not the [qt] extra: VRED supplies Qt). For "
    "example, from a command prompt: <VRED install>\\lib\\python\\python.exe -m pip install "
    "--target %USERPROFILE%\\.sqi\\vred-site sqi-submitter, then set "
    '"sqi_submitter_site" to that folder in ~/.sqi/submitter.json (or set the environment '
    "variable SQI_SUBMITTER_SITE). See the VRED section of the sqi submitter documentation."
)

_menu: Any = None
_menu_bar: Any = None


def _settings_path() -> str:
    return os.environ.get("SQI_SUBMITTER_SETTINGS") or os.path.join(
        os.path.expanduser("~"), ".sqi", "submitter.json"
    )


def _configured_site(settings_path: str) -> str:
    """The folder sqi_submitter was installed into: $SQI_SUBMITTER_SITE, else the settings file."""
    from_env = os.environ.get("SQI_SUBMITTER_SITE", "")
    if from_env:
        return from_env
    try:
        # utf-8-sig: PowerShell 5 writes UTF-8 with a BOM, which plain utf-8 rejects.
        with open(settings_path, encoding="utf-8-sig") as handle:
            value = json.load(handle).get("sqi_submitter_site")
    except (OSError, ValueError, AttributeError):
        return ""
    return value if isinstance(value, str) else ""


def _import_menu() -> Any:
    importlib.import_module("sqi_submitter")  # the package itself, before any submodule of it
    return importlib.import_module("sqi_submitter.hosts.vred.menu")


def load_submitter(settings_path: str) -> tuple[object | None, str | None]:
    """Import sqi_submitter's VRED menu glue: ``(module, None)`` or ``(None, message)``.

    When the first import fails, the configured site folder is added to ``sys.path`` and the
    import retried once.
    """
    try:
        return _import_menu(), None
    except ImportError as first:
        reason = str(first)
    site = _configured_site(settings_path)
    if site:
        if site not in sys.path:
            sys.path.insert(0, site)
        try:
            return _import_menu(), None
        except ImportError as second:
            reason = str(second)
    return None, MISSING_MESSAGE.format(reason=reason)


def _message_box() -> Any:
    """QMessageBox from VRED's own PySide (6, else 2); ``None`` when neither imports."""
    for binding in ("PySide6", "PySide2"):
        try:
            importlib.import_module(binding)
            return importlib.import_module(binding + ".QtWidgets").QMessageBox
        except ImportError:
            continue
    return None


def _warn(text: str) -> None:
    """Show ``text`` to the artist, falling back to VRED's terminal."""
    box = _message_box()
    if box is not None:
        try:
            box.warning(getattr(builtins, "vrMainWindow", None), DIALOG_TITLE, text)
            return
        except Exception:  # a dialog that cannot be shown must not hide the message
            pass
    print("sqi: " + text)


def _builtin(name: str) -> Any:
    """One of VRED's injected services; AttributeError when this is not VRED."""
    return getattr(builtins, name)


def _services() -> Any:
    """The VRED objects the adapter reads, as the submitter's ``VREDServices``."""
    menu_module = importlib.import_module("sqi_submitter.hosts.vred.adapter")
    vr_render_settings = importlib.import_module("vrRenderSettings")
    return menu_module.VREDServices(
        file_io=_builtin("vrFileIOService"),
        cameras=_builtin("vrCameraService"),
        render_settings=vr_render_settings,
        main_window=_builtin("vrMainWindow"),
    )


def on_submit_clicked(*_args: object) -> None:
    loaded, message = load_submitter(_settings_path())
    if loaded is None:
        _warn(message or MISSING_MESSAGE.format(reason="unknown"))
        return
    try:
        services = _services()
    except (ImportError, AttributeError) as exc:
        _warn("VRED's API could not be reached from the sqi plugin (" + str(exc) + ").")
        return
    menu: Any = loaded
    try:
        menu.open_submitter(services)
    except Exception as exc:  # the dialog must not take a traceback into VRED's terminal
        _warn("The sqi submit dialog failed to open: " + str(exc))


def register_menu(main_window: Any) -> None:
    """Add sqi > Submit to sqi... to the menu bar (replacing a menu from an earlier load)."""
    global _menu, _menu_bar
    unregister_menu()
    bar = main_window.menuBar()
    menu = bar.addMenu(MENU_TITLE)
    menu.addAction(ACTION_TEXT).triggered.connect(on_submit_clicked)
    _menu, _menu_bar = menu, bar


def unregister_menu() -> None:
    global _menu, _menu_bar
    if _menu is not None and _menu_bar is not None:
        _menu_bar.removeAction(_menu.menuAction())
    _menu = _menu_bar = None


def onDestroyVREDScriptPlugin() -> None:
    """VRED calls this when the plugin is unloaded or reloaded: take the menu away."""
    unregister_menu()


def _register_in_vred() -> None:
    window = getattr(builtins, "vrMainWindow", None)
    if window is None:
        return  # not inside VRED (a test, or a plain interpreter)
    try:
        register_menu(window)
    except Exception as exc:  # a menu that cannot be added must not stop VRED loading plugins
        print("sqi: the Submit to sqi menu was not added: " + str(exc))


_register_in_vred()
