# SPDX-License-Identifier: AGPL-3.0-or-later
"""``sqi-submit install-host``: copying a handoff host's files into place."""

from __future__ import annotations

import io
import shutil
from importlib import resources
from pathlib import Path, PurePosixPath

import pytest

from sqi_submitter import cli, install
from tests.conftest import no_qt

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


def test_install_host_keeps_a_foreign_hook_file(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _package_with(tmp_path, monkeypatch, {"hosts/blender/init.py": _HOOK_TEXT})
    monkeypatch.setattr(install, "HOST_FILES", {"blender": ["hosts/blender/init.py"]})
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
    _package_with(tmp_path, monkeypatch, {"hosts/blender/init.py": _HOOK_TEXT})
    monkeypatch.setattr(install, "HOST_FILES", {"blender": ["hosts/blender/init.py"]})
    out = io.StringIO()
    assert install.install_host("blender", tmp_path / "fresh", out) == 0
    assert (tmp_path / "fresh" / "init.py").read_text(encoding="utf-8") == _HOOK_TEXT
    assert "kept your existing" not in out.getvalue()


def test_install_host_replaces_its_own_hook_file(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _package_with(tmp_path, monkeypatch, {"hosts/blender/init.py": _HOOK_TEXT})
    monkeypatch.setattr(install, "HOST_FILES", {"blender": ["hosts/blender/init.py"]})
    dest = tmp_path / "startup"
    dest.mkdir()
    (dest / "init.py").write_text(f"{install.HOOK_MARKER}\nold\n", encoding="utf-8")
    out = io.StringIO()
    assert install.install_host("blender", dest, out) == 0
    assert (dest / "init.py").read_text(encoding="utf-8") == _HOOK_TEXT
    assert "kept your existing" not in out.getvalue()


def test_install_host_foreign_hook_stays_foreign_after_appending_printed_lines(
    monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _package_with(tmp_path, monkeypatch, {"hosts/blender/init.py": _HOOK_TEXT})
    monkeypatch.setattr(install, "HOST_FILES", {"blender": ["hosts/blender/init.py"]})
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
    assert "kept your existing" in again.getvalue()
