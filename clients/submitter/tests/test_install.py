# SPDX-License-Identifier: AGPL-3.0-or-later
"""``sqi-submit install-host``: copying a handoff host's files into place."""

from __future__ import annotations

import ast
import io
import shutil
from importlib import resources
from pathlib import Path, PurePosixPath

import pytest

from sqi_submitter import cli, install
from tests.conftest import HOSTS_DIR, no_qt

# Small package files standing in for each host's real files.
_STAND_INS: dict[str, list[str]] = {
    "blender": ["hosts/__init__.py"],
    "cinema4d": ["hosts/__init__.py"],
    "aftereffects": ["hosts/__init__.py", "hosts/_handoff_block.py"],
}


def test_install_host_copies_to_dest(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setattr(install, "HOST_FILES", _STAND_INS)
    for host, files in _STAND_INS.items():
        dest = tmp_path / host / "nested"
        out = io.StringIO()
        assert install.install_host(host, dest, out) == 0
        for rel in files:
            copied = dest / Path(rel).name
            assert copied.is_file()
            source = resources.files("sqi_submitter").joinpath(rel)
            assert copied.read_bytes() == source.read_bytes()
            assert str(copied) in out.getvalue()


def test_install_host_unknown() -> None:
    out = io.StringIO()
    assert install.install_host("maya", None, out) == 2
    for host in ("blender", "cinema4d", "aftereffects"):
        assert host in out.getvalue()


def test_install_host_without_destination_prints_instructions(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(install, "HOST_FILES", _STAND_INS)
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host("blender", None, out) == 0
    assert "Install from Disk" in out.getvalue()
    assert "__init__.py" in out.getvalue()

    out = io.StringIO()
    assert install.install_host("aftereffects", None, out) == 0
    assert "Scripts" in out.getvalue()
    assert "_handoff_block.py" in out.getvalue()

    out = io.StringIO()
    assert install.install_host("cinema4d", None, out) == 0
    assert "Open Preferences Folder" in out.getvalue()


def test_default_destination_blender_and_aftereffects_are_none() -> None:
    for platform in ("win32", "darwin", "linux"):
        assert install.default_destination("blender", platform, {}) is None
        assert install.default_destination("aftereffects", platform, {}) is None


def test_default_destination_cinema4d_single_folder_windows(tmp_path: Path) -> None:
    prefs = tmp_path / "Maxon" / "Maxon Cinema 4D 2025_ABCD1234"
    prefs.mkdir(parents=True)
    got = install.default_destination("cinema4d", "win32", {"APPDATA": str(tmp_path)})
    assert got == prefs / "plugins"


def test_default_destination_cinema4d_single_folder_macos(tmp_path: Path) -> None:
    prefs = tmp_path / "Library" / "Preferences" / "Maxon" / "Maxon Cinema 4D 2025_ABCD1234"
    prefs.mkdir(parents=True)
    got = install.default_destination("cinema4d", "darwin", {"HOME": str(tmp_path)})
    assert got == prefs / "plugins"


def test_default_destination_cinema4d_zero_or_many_folders_is_none(tmp_path: Path) -> None:
    env = {"APPDATA": str(tmp_path)}
    assert install.default_destination("cinema4d", "win32", env) is None  # no Maxon dir
    root = tmp_path / "Maxon"
    root.mkdir()
    assert install.default_destination("cinema4d", "win32", env) is None  # none inside
    (root / "Maxon Cinema 4D 2024_AAAA").mkdir()
    (root / "Maxon Cinema 4D 2025_BBBB").mkdir()
    assert install.default_destination("cinema4d", "win32", env) is None  # ambiguous
    (root / "Unrelated").mkdir()
    assert install.default_destination("cinema4d", "win32", env) is None


def test_default_destination_cinema4d_missing_env_is_none() -> None:
    assert install.default_destination("cinema4d", "win32", {}) is None


@pytest.mark.parametrize(
    ("platform", "env", "expected"),
    [
        (
            "win32",
            {"USERPROFILE": "C:/Users/a"},
            Path("C:/Users/a") / "Documents" / "UnrealEngine" / "Python",
        ),
        (
            "darwin",
            {"HOME": "/Users/a"},
            Path("/Users/a") / "Documents" / "UnrealEngine" / "Python",
        ),
        ("linux", {"HOME": "/home/a"}, Path("/home/a") / "Documents" / "UnrealEngine" / "Python"),
        ("win32", {"HOME": "/home/a"}, None),
        ("win32", {}, None),
        ("darwin", {"USERPROFILE": "C:/Users/a"}, None),
        ("linux", {}, None),
    ],
)
def test_default_destination_unreal(
    platform: str, env: dict[str, str], expected: Path | None
) -> None:
    assert install.default_destination("unreal", platform, env) == expected


def test_unreal_next_step_and_no_destination_hint(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host("unreal", None, out) == 0
    text = out.getvalue()
    assert "init_unreal.py" in text and "sqi_unreal_submit.py" in text
    assert "Restart the Unreal Editor; Submit to sqi… appears in the Tools menu." in text


def test_install_host_uses_default_destination(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr(install, "HOST_FILES", _STAND_INS)
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: tmp_path / "d")
    out = io.StringIO()
    assert install.install_host("cinema4d", None, out) == 0
    assert (tmp_path / "d" / "__init__.py").is_file()


def test_host_files_are_package_resources() -> None:
    root = resources.files("sqi_submitter")
    for host, files in install.HOST_FILES.items():
        assert host in install.KNOWN_HOSTS
        for rel in files:
            assert root.joinpath(rel).is_file(), f"{host}: {rel} is not package data"


def test_cli_install_host_copies(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setattr(install, "HOST_FILES", _STAND_INS)
    dest = tmp_path / "out"
    assert cli.main(["install-host", "blender", "--dest", str(dest)]) == 0
    assert (dest / "__init__.py").is_file()
    assert "__init__.py" in capsys.readouterr().out


def test_cli_install_host_unknown_exits_two(capsys: pytest.CaptureFixture[str]) -> None:
    assert cli.main(["install-host", "maya"]) == 2
    captured = capsys.readouterr()
    assert "cinema4d" in captured.err


def test_install_host_dest_is_a_file(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    monkeypatch.setattr(install, "HOST_FILES", _STAND_INS)
    blocker = tmp_path / "not-a-dir"
    blocker.write_text("x", encoding="utf-8")
    out = io.StringIO()
    err = io.StringIO()
    assert install.install_host("blender", blocker, out, err) == 1
    assert f"could not install into {blocker}" in err.getvalue()
    assert "nothing was installed" in err.getvalue()
    assert out.getvalue() == ""


def test_install_host_mid_loop_failure_reports_partial_copy(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    monkeypatch.setattr(install, "HOST_FILES", _STAND_INS)
    real_copy = shutil.copyfile
    calls: list[int] = []

    def flaky(src: Path, dst: Path) -> Path:
        calls.append(1)
        if len(calls) == 2:
            raise PermissionError(13, "Permission denied")
        return real_copy(src, dst)

    monkeypatch.setattr(shutil, "copyfile", flaky)
    err = io.StringIO()
    assert install.install_host("aftereffects", tmp_path / "d", io.StringIO(), err) == 1
    assert "Permission denied" in err.getvalue()
    assert "1 file(s) already copied were left in place" in err.getvalue()


def test_cli_install_host_bad_dest_exits_one(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, capsys: pytest.CaptureFixture[str]
) -> None:
    monkeypatch.setattr(install, "HOST_FILES", _STAND_INS)
    blocker = tmp_path / "f"
    blocker.write_text("x", encoding="utf-8")
    assert cli.main(["install-host", "blender", "--dest", str(blocker)]) == 1
    assert "could not install into" in capsys.readouterr().err


def test_cli_dialog_without_qt_names_the_install_command(
    monkeypatch: pytest.MonkeyPatch,
    capsys: pytest.CaptureFixture[str],
    tmp_path: Path,
    native_messages: list[str],
) -> None:
    """No Qt binding: an actionable message and exit 1, never an import-time traceback.

    A host launches sqi-submit detached, so the message is also logged and shown natively.
    """
    monkeypatch.setenv("SQI_SUBMITTER_SETTINGS", str(tmp_path / ".sqi" / "submitter.json"))
    no_qt(monkeypatch)
    assert cli.main([]) == 1
    err = capsys.readouterr().err
    assert "No Qt binding found" in err
    assert 'pip install "sqi-submitter[qt]"' in err
    assert "Traceback" not in err
    log = (tmp_path / ".sqi" / "submit-error.log").read_text(encoding="utf-8")
    assert "No Qt binding found" in log
    assert "Traceback" not in log
    (shown,) = native_messages  # pytest's stderr is not a terminal
    assert shown.startswith("No Qt binding found")
    assert shown.endswith(str(tmp_path / ".sqi" / "submit-error.log"))


def test_dest_relative_keeps_tree_under_host_folder() -> None:
    assert install.dest_relative("modo", "hosts/modo/sqi/lxserv/a.py") == PurePosixPath(
        "sqi/lxserv/a.py"
    )
    assert install.dest_relative("cinema4d", "hosts/cinema4d/sqi_submit.pyp") == PurePosixPath(
        "sqi_submit.pyp"
    )
    assert install.dest_relative("blender", "hosts/__init__.py") == PurePosixPath("__init__.py")


def _package_with(tmp_path: Path, monkeypatch: pytest.MonkeyPatch, files: dict[str, str]) -> None:
    """Make install read its package data from a throwaway tree holding ``files``."""
    root = tmp_path / "pkg"
    for rel, text in files.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")
    monkeypatch.setattr(resources, "files", lambda _package: root)


def test_install_host_copies_a_tree(monkeypatch: pytest.MonkeyPatch, tmp_path: Path) -> None:
    _package_with(
        tmp_path,
        monkeypatch,
        {"hosts/blender/sqi/lxserv/a.py": "a\n", "hosts/blender/index.cfg": "cfg\n"},
    )
    monkeypatch.setattr(
        install,
        "HOST_FILES",
        {"blender": ["hosts/blender/sqi/lxserv/a.py", "hosts/blender/index.cfg"]},
    )
    dest = tmp_path / "kit"
    out = io.StringIO()
    assert install.install_host("blender", dest, out) == 0
    assert (dest / "sqi" / "lxserv" / "a.py").read_text(encoding="utf-8") == "a\n"
    assert (dest / "index.cfg").read_text(encoding="utf-8") == "cfg\n"
    assert str(dest / "sqi" / "lxserv" / "a.py") in out.getvalue()


_HOOK_TEXT = f"{install.HOOK_MARKER}\nimport sqi_katana_submit\n"


def _stand_in_hook(tmp_path: Path, monkeypatch: pytest.MonkeyPatch, text: str = _HOOK_TEXT) -> None:
    """Make "blender" ship one file, ``init.py``, holding ``text``."""
    _package_with(tmp_path, monkeypatch, {"hosts/blender/init.py": text})
    monkeypatch.setattr(install, "HOST_FILES", {"blender": ["hosts/blender/init.py"]})


def test_install_host_keeps_a_foreign_hook_file(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _stand_in_hook(tmp_path, monkeypatch)
    dest = tmp_path / "startup"
    dest.mkdir()
    (dest / "init.py").write_text('print("studio")', encoding="utf-8")
    out = io.StringIO()
    assert install.install_host("blender", dest, out) == 0
    assert (dest / "init.py").read_text(encoding="utf-8") == 'print("studio")'
    text = out.getvalue()
    assert f"kept your existing {dest / 'init.py'}; to add sqi, append these lines to it:" in text
    assert "import sqi_katana_submit" in text
    # The marker names the file sqi itself installs; the lines for the artist must not carry it.
    assert install.HOOK_MARKER not in text


def test_install_host_creates_a_missing_hook_file(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _stand_in_hook(tmp_path, monkeypatch)
    out = io.StringIO()
    assert install.install_host("blender", tmp_path / "fresh", out) == 0
    assert (tmp_path / "fresh" / "init.py").read_text(encoding="utf-8") == _HOOK_TEXT
    assert "kept your existing" not in out.getvalue()


def test_install_host_replaces_its_own_hook_file(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _stand_in_hook(tmp_path, monkeypatch)
    dest = tmp_path / "startup"
    dest.mkdir()
    previous = f"{install.HOOK_MARKER}\nold\nprint('a studio edit')\n"
    (dest / "init.py").write_text(previous, encoding="utf-8")
    out = io.StringIO()
    assert install.install_host("blender", dest, out) == 0
    assert (dest / "init.py").read_text(encoding="utf-8") == _HOOK_TEXT
    assert "kept your existing" not in out.getvalue()
    # sqi's own file, but edited since: the edits are kept beside it.
    backup = dest / "init.py.sqi-old"
    assert backup.read_text(encoding="utf-8") == previous
    assert f"saved the previous init.py as {backup}" in out.getvalue()


def test_install_host_reinstalling_an_unchanged_hook_keeps_no_backup(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _stand_in_hook(tmp_path, monkeypatch)
    dest = tmp_path / "startup"
    assert install.install_host("blender", dest, io.StringIO()) == 0
    out = io.StringIO()
    assert install.install_host("blender", dest, out) == 0
    assert [p.name for p in dest.iterdir()] == ["init.py"]
    assert "saved the previous" not in out.getvalue()


def test_install_host_foreign_hook_stays_foreign_after_appending_printed_lines(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _stand_in_hook(tmp_path, monkeypatch)
    dest = tmp_path / "startup"
    dest.mkdir()
    studio = 'print("studio")'
    (dest / "init.py").write_text(studio, encoding="utf-8")
    out = io.StringIO()
    assert install.install_host("blender", dest, out) == 0
    # The artist does what the guidance says: appends the printed lines to their own file.
    printed = out.getvalue().split("append these lines to it:\n", 1)[1]
    printed = printed.split("In Blender:", 1)[0]
    with (dest / "init.py").open("a", encoding="utf-8") as handle:
        handle.write("\n" + printed)
    appended = (dest / "init.py").read_text(encoding="utf-8")
    assert "import sqi_katana_submit" in appended
    again = io.StringIO()
    assert install.install_host("blender", dest, again) == 0
    assert (dest / "init.py").read_text(encoding="utf-8") == appended
    assert appended.startswith(studio)
    # The lines are in already: asking for them again would add a second sqi menu.
    assert f"kept your existing {dest / 'init.py'}; it already loads sqi" in again.getvalue()
    assert "append these lines" not in again.getvalue()


@pytest.mark.parametrize(
    ("host", "hook", "call"),
    [
        ("unreal", "init_unreal.py", "register_menu()"),
        ("katana", "init.py", "install_menu"),
        ("silhouette", "sfxuser.py", "register()"),
    ],
)
def test_install_host_guidance_for_a_real_hook_is_valid_to_append(
    tmp_path: Path, host: str, hook: str, call: str
) -> None:
    """The printed lines go to the end of a studio's own file, after its imports and code.

    A ``from __future__`` line there is a SyntaxError, and without it an annotation is
    evaluated when the line runs (an error on Python 3.7), so the lines may carry neither.
    """
    dest = tmp_path / "startup"
    dest.mkdir()
    studio = "import os\n\nSTUDIO = os.environ.get('STUDIO')\nprint(STUDIO)\n"
    (dest / hook).write_text(studio, encoding="utf-8")
    out = io.StringIO()
    assert install.install_host(host, dest, out) == 0
    assert (dest / hook).read_text(encoding="utf-8") == studio
    lines = install._lines_to_add(HOSTS_DIR / host / hook)
    assert f"append these lines to it:\n{lines}\n" in out.getvalue()
    assert f"import sqi_{host}_submit" in lines and call in lines
    assert "from __future__" not in lines and install.HOOK_MARKER not in lines
    compile(studio + "\n" + lines + "\n", hook, "exec")
    for node in ast.walk(ast.parse(lines)):
        assert not isinstance(node, ast.AnnAssign)
        if isinstance(node, ast.arg):
            assert node.annotation is None
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            assert node.returns is None


def test_shipped_hooks_are_the_files_carrying_the_marker() -> None:
    """The marker alone makes a shipped file a hook; only these three files carry it."""
    root = resources.files("sqi_submitter")
    hooks = {
        rel
        for files in install.HOST_FILES.values()
        for rel in files
        if install.HOOK_MARKER in root.joinpath(rel).read_text(encoding="utf-8").splitlines()
    }
    assert hooks == {
        "hosts/unreal/init_unreal.py",
        "hosts/katana/init.py",
        "hosts/silhouette/sfxuser.py",
    }


@pytest.mark.parametrize("host", install.KNOWN_HOSTS)
def test_every_host_has_guidance_when_no_destination_is_known(
    monkeypatch: pytest.MonkeyPatch, host: str
) -> None:
    """Each host needs its own hint and next step, or this path fails only for its artists."""
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host(host, None, out) == 0
    for rel in install.HOST_FILES[host]:
        assert PurePosixPath(rel).name in out.getvalue()


def test_install_host_guidance_drops_future_imports(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    hook = f"from __future__ import annotations\n{install.HOOK_MARKER}\nimport sqi_x\n"
    _stand_in_hook(tmp_path, monkeypatch, hook)
    dest = tmp_path / "startup"
    dest.mkdir()
    (dest / "init.py").write_text("x = 1\n", encoding="utf-8")
    out = io.StringIO()
    assert install.install_host("blender", dest, out) == 0
    assert "__future__" not in out.getvalue()
    assert "import sqi_x" in out.getvalue()


@pytest.mark.parametrize(
    ("platform", "env", "expected"),
    [
        ("win32", {"USERPROFILE": "C:/Users/a"}, Path("C:/Users/a") / ".katana" / "Startup"),
        ("linux", {"HOME": "/home/a"}, Path("/home/a") / ".katana" / "Startup"),
        ("darwin", {"HOME": "/Users/a"}, None),  # Katana does not run on macOS
        ("win32", {"HOME": "/home/a"}, None),
        ("win32", {}, None),
        ("linux", {"USERPROFILE": "C:/Users/a"}, None),
        ("linux", {}, None),
    ],
)
def test_default_destination_katana(
    platform: str, env: dict[str, str], expected: Path | None
) -> None:
    assert install.default_destination("katana", platform, env) == expected


def test_katana_next_step_and_no_destination_hint(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host("katana", None, out) == 0
    text = out.getvalue()
    assert "init.py" in text and "sqi_katana_submit.py" in text
    assert "macOS" in text
    assert "Restart Katana; Submit to sqi… appears in the sqi menu." in text


def _documents(platform: str, home: Path) -> tuple[dict[str, str], Path]:
    """The environment for ``platform`` and the Documents folder it implies, created."""
    env = {"USERPROFILE": str(home)} if platform == "win32" else {"HOME": str(home)}
    documents = home / "Documents"
    documents.mkdir(parents=True)
    return env, documents


@pytest.mark.parametrize("platform", ["win32", "darwin"])
def test_default_destination_keyshot(platform: str, tmp_path: Path) -> None:
    """Exactly one ``KeyShot*`` folder in Documents gives its ``Scripts``; else nothing."""
    env, documents = _documents(platform, tmp_path)
    assert install.default_destination("keyshot", platform, env) is None  # none
    (documents / "Unrelated").mkdir()
    assert install.default_destination("keyshot", platform, env) is None  # none that match
    (documents / "KeyShot 2024").mkdir()
    assert install.default_destination("keyshot", platform, env) == (
        documents / "KeyShot 2024" / "Scripts"
    )
    (documents / "KeyShot 11").mkdir()
    assert install.default_destination("keyshot", platform, env) is None  # ambiguous


def test_default_destination_keyshot_ignores_a_file_named_like_the_folder(tmp_path: Path) -> None:
    env, documents = _documents("win32", tmp_path)
    (documents / "KeyShot notes.txt").write_text("x", encoding="utf-8")
    assert install.default_destination("keyshot", "win32", env) is None


@pytest.mark.parametrize(
    ("platform", "env"),
    [
        ("linux", {"HOME": "/home/a"}),  # KeyShot has no Linux desktop product
        ("win32", {}),
        ("win32", {"HOME": "/home/a"}),
        ("darwin", {}),
        ("darwin", {"USERPROFILE": "C:/Users/a"}),
    ],
)
def test_default_destination_keyshot_without_a_place_is_none(
    platform: str, env: dict[str, str]
) -> None:
    assert install.default_destination("keyshot", platform, env) is None


def test_keyshot_next_step_and_no_destination_hint(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host("keyshot", None, out) == 0
    text = out.getvalue()
    assert "sqi_keyshot_submit.py" in text
    assert "Scripts" in text
    assert "In KeyShot open Window > Scripting Console and run sqi_keyshot_submit." in text


def test_install_host_copies_the_keyshot_script(tmp_path: Path) -> None:
    dest = tmp_path / "Scripts"
    out = io.StringIO()
    assert install.install_host("keyshot", dest, out) == 0
    assert (dest / "sqi_keyshot_submit.py").is_file()
    assert [p.name for p in dest.iterdir()] == ["sqi_keyshot_submit.py"]  # no hook file


@pytest.mark.parametrize(
    ("host", "name", "module"),
    [
        ("unreal", "init_unreal.py", "sqi_unreal_submit"),
        ("katana", "init.py", "sqi_katana_submit"),
        ("silhouette", "sfxuser.py", "sqi_silhouette_submit"),
    ],
)
def test_no_destination_prints_the_hook_lines_to_append(
    monkeypatch: pytest.MonkeyPatch, host: str, name: str, module: str
) -> None:
    """With no default folder the artist may already own the hook-named file: show the lines.

    Copying the shipped hook verbatim over it would bring back the ``__future__`` import
    (a SyntaxError mid-file) and the marker (a later install would overwrite their file).
    """
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host(host, None, out) == 0
    text = out.getvalue()
    heading = f"If you already have {name} there, append these lines to it instead:\n"
    assert text.count(heading) == 1
    printed = text.split(heading, 1)[1]
    assert f"import {module}" in printed
    assert install.HOOK_MARKER not in text
    assert "from __future__" not in text
    compile(
        "import os\nSTUDIO = os.environ.get('STUDIO')\n" + printed.split("\nRestart", 1)[0],
        name,
        "exec",
    )


def test_no_destination_hint_for_other_hosts_prints_no_hook_lines(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(install, "default_destination", lambda host, platform, env: None)
    out = io.StringIO()
    assert install.install_host("keyshot", None, out) == 0
    assert "append these lines" not in out.getvalue()


@pytest.mark.parametrize(
    "studio",
    [
        '# files carrying "# sqi-submitter hook" are replaced by sqi\nprint("studio")\n',
        'MARKER = "# sqi-submitter hook"\n',
        "x = 1  # sqi-submitter hook\n",
    ],
)
def test_install_host_mentioning_the_marker_is_still_a_foreign_hook(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path, studio: str
) -> None:
    """Only a line that is the marker marks a file as ours, not text that mentions it."""
    _stand_in_hook(tmp_path, monkeypatch)
    dest = tmp_path / "startup"
    dest.mkdir()
    (dest / "init.py").write_text(studio, encoding="utf-8")
    out = io.StringIO()
    assert install.install_host("blender", dest, out) == 0
    assert (dest / "init.py").read_text(encoding="utf-8") == studio
    assert "kept your existing" in out.getvalue()


def test_install_host_replaces_a_hook_with_crlf_line_endings(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _stand_in_hook(tmp_path, monkeypatch)
    dest = tmp_path / "startup"
    dest.mkdir()
    (dest / "init.py").write_bytes(f"{install.HOOK_MARKER}\r\nold\r\n".encode())
    assert install.install_host("blender", dest, io.StringIO()) == 0
    assert (dest / "init.py").read_text(encoding="utf-8") == _HOOK_TEXT
