# SPDX-License-Identifier: AGPL-3.0-or-later
"""Offscreen dialog e2e against a respx-mocked server (no DCC required)."""

import json
import os
from pathlib import Path
from typing import ClassVar

import httpx
import pytest
import respx

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
compat = pytest.importorskip("sqi_submitter.qt._compat")
if not compat.QT_BINDING:
    pytest.skip("no Qt binding installed", allow_module_level=True)

from sqi_client import SqiClient  # noqa: E402
from sqi_client.models import Product  # noqa: E402
from sqi_submitter import cli  # noqa: E402
from sqi_submitter.core import SubmitterSession  # noqa: E402
from sqi_submitter.core.handoff import ContextAdapter, dump_context, load_context  # noqa: E402
from sqi_submitter.qt import dialog as qt_dialog  # noqa: E402
from sqi_submitter.qt.dialog import SubmitDialog  # noqa: E402
from tests.test_adapter_contract import BASE, MiniAdapter  # noqa: E402

QtWidgets = compat.QtWidgets

pytestmark = pytest.mark.usefixtures("_clean_settings")


@pytest.fixture(scope="module")
def app() -> object:
    return QtWidgets.QApplication.instance() or QtWidgets.QApplication([])


def _mock_server(products: "list[dict[str, str]] | None" = None) -> respx.Route:
    # NOTE: `products` distinguishes "not given" (None, use the default single
    # product) from "given as []" (an explicitly empty catalog) — `products or
    # [default]` would silently discard a caller-supplied empty list.
    if products is None:
        products = [{"name": "mini-render", "title": "Mini Render"}]
    respx.get(f"{BASE}/api/v1/products").mock(return_value=httpx.Response(200, json=products))
    respx.get(f"{BASE}/api/v1/products/mini-render/parameters").mock(
        return_value=httpx.Response(
            200,
            json=[
                {"name": "SceneFile", "type": "PATH"},
                {"name": "Frames", "type": "STRING"},
            ],
        )
    )
    # NOTE: list_farms is a bare-array endpoint in the real SDK (see
    # tests/test_session.py::test_farms_and_queues_fetch); an {"items": [...]}
    # mock would not match SqiClient.list_farms and would leave farmCombo
    # empty.
    respx.get(f"{BASE}/api/v1/farms").mock(
        return_value=httpx.Response(200, json=[{"id": "f1", "name": "Farm"}])
    )
    respx.get(f"{BASE}/api/v1/queues", params={"farm_id": "f1"}).mock(
        return_value=httpx.Response(200, json={"items": [{"id": "q1", "name": "Q"}]})
    )
    return respx.post(f"{BASE}/api/v1/products/mini-render/jobs").mock(
        return_value=httpx.Response(201, json={"id": "j1", "name": "Mini Render"})
    )


@respx.mock
def test_dialog_prefills_and_submits(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    route = _mock_server()
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MiniAdapter())
    monkeypatch.setattr(QtWidgets.QMessageBox, "information", lambda *a, **k: None)
    line = dialog.findChild(QtWidgets.QLineEdit, "field_Frames")
    assert line is not None
    assert line.text() == "1-4"  # pre-filled from MiniAdapter's SceneContext
    dialog.submit_and_wait()  # test hook: runs the worker synchronously
    body = json.loads(route.calls.last.request.content)
    assert body["parameters"]["SceneFile"] == "/x/s.mini"
    assert body["name"] == "Mini Render"
    # Settings are persisted from values captured at submit-start, not from
    # post-completion widget state; this pins the capture code path.
    assert dialog.session.settings.get("last_product.mini") == "mini-render"


@respx.mock
def test_validation_error_shows_banner_not_post(app: object) -> None:
    route = _mock_server()
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=None)
    line = dialog.findChild(QtWidgets.QLineEdit, "field_SceneFile")
    assert line is not None
    line.setText("")
    dialog.submit_and_wait()
    assert not route.called
    error_label = dialog.findChild(QtWidgets.QLabel, "errorLabel")
    assert error_label is not None
    assert not error_label.isHidden()


@respx.mock
def test_stale_model_never_submits_under_new_product_name(app: object) -> None:
    route = _mock_server(
        products=[
            {"name": "mini-render", "title": "Mini Render"},
            {"name": "broken", "title": "Broken"},
        ]
    )
    respx.get(f"{BASE}/api/v1/products/broken/parameters").mock(
        return_value=httpx.Response(500, json={"error": "internal server error"})
    )
    # max_attempts=1 disables the SDK's GET retry/backoff so the test is fast
    # (mirrors tests/test_session.py::test_server_error_translates...).
    session = SubmitterSession(server_url=BASE, client=SqiClient(BASE, max_attempts=1))
    dialog = SubmitDialog(session, adapter=None)

    combo = dialog.findChild(QtWidgets.QComboBox, "productCombo")
    assert combo is not None
    broken_index = next(
        i
        for i in range(combo.count())
        if isinstance(combo.itemData(i), Product) and combo.itemData(i).name == "broken"
    )
    combo.setCurrentIndex(broken_index)  # rebuild fails: model is now stale

    dialog.submit_and_wait()
    assert not route.called
    error_label = dialog.findChild(QtWidgets.QLabel, "errorLabel")
    assert error_label is not None
    assert not error_label.isHidden()


@respx.mock
def test_empty_catalog_shows_guidance_banner_and_disables_submit(app: object) -> None:
    post_route = _mock_server(products=[])
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=None)

    error_label = dialog.findChild(QtWidgets.QLabel, "errorLabel")
    assert error_label is not None
    assert not error_label.isHidden()
    assert (
        error_label.text() == "No products are available on this server. Ask your admin to "
        "install a preset from the preset library (Admin → Preset Library), then Reload."
    )
    submit_button = dialog.findChild(QtWidgets.QPushButton, "submitButton")
    assert submit_button is not None
    assert not submit_button.isEnabled()

    dialog.submit_and_wait()
    assert not post_route.called


@respx.mock
def test_owner_field_hidden_without_submit_as_permission(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    route = _mock_server()
    respx.get(f"{BASE}/api/v1/auth/me").mock(
        return_value=httpx.Response(
            200,
            json={
                "subject": "u1",
                "display_name": "User",
                "roles": ["operator"],
                "permissions": ["jobs.read"],
                "kind": "user",
            },
        )
    )
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MiniAdapter())
    assert dialog.session.may_submit_as is False

    owner_edit = dialog.findChild(QtWidgets.QLineEdit, "ownerEdit")
    assert owner_edit is not None
    assert owner_edit.isHidden()

    # Even if something set stale text into the hidden field, it must not
    # leak into the submit payload.
    owner_edit.setText("mallory")
    group = dialog.findChild(QtWidgets.QGroupBox, "advancedGroup")
    assert group is not None
    group.setChecked(True)
    priority_spin = dialog.findChild(QtWidgets.QSpinBox, "prioritySpin")
    assert priority_spin is not None
    priority_spin.setValue(80)
    monkeypatch.setattr(QtWidgets.QMessageBox, "information", lambda *a, **k: None)
    dialog.submit_and_wait()
    body = json.loads(route.calls.last.request.content)
    assert "owner" not in body
    assert body["priority"] == 80


@respx.mock
def test_owner_field_visible_when_me_endpoint_missing(app: object) -> None:
    """A server predating /auth/me must not lose the Owner field (fail open)."""
    _mock_server()
    respx.get(f"{BASE}/api/v1/auth/me").mock(return_value=httpx.Response(404))
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MiniAdapter())
    assert dialog.session.may_submit_as is True

    owner_edit = dialog.findChild(QtWidgets.QLineEdit, "ownerEdit")
    assert owner_edit is not None
    assert not owner_edit.isHidden()


def test_main_is_importable_entry_point() -> None:
    assert callable(cli.main)


@respx.mock
def test_dialog_hides_scene_field_with_adapter(app: object) -> None:
    _mock_server()
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MiniAdapter())
    assert dialog.findChild(QtWidgets.QLineEdit, "field_SceneFile") is None
    assert dialog.findChild(QtWidgets.QLineEdit, "field_Frames") is not None


@respx.mock
def test_advanced_overrides_forwarded(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    route = _mock_server()
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MiniAdapter())
    monkeypatch.setattr(QtWidgets.QMessageBox, "information", lambda *a, **k: None)
    group = dialog.findChild(QtWidgets.QGroupBox, "advancedGroup")
    assert group is not None
    group.setChecked(True)
    priority_spin = dialog.findChild(QtWidgets.QSpinBox, "prioritySpin")
    assert priority_spin is not None
    priority_spin.setValue(80)
    max_attempts_spin = dialog.findChild(QtWidgets.QSpinBox, "maxAttemptsSpin")
    assert max_attempts_spin is not None
    max_attempts_spin.setValue(5)
    owner_edit = dialog.findChild(QtWidgets.QLineEdit, "ownerEdit")
    assert owner_edit is not None
    owner_edit.setText("alice")
    dialog.submit_and_wait()
    body = json.loads(route.calls.last.request.content)
    assert body["owner"] == "alice"
    assert body["priority"] == 80
    assert body["max_attempts"] == 5
    assert "retry_delay_seconds" not in body  # untouched (0) -> omitted -> inherit


@respx.mock
def test_advanced_unchecked_sends_no_overrides(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    route = _mock_server()
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MiniAdapter())
    monkeypatch.setattr(QtWidgets.QMessageBox, "information", lambda *a, **k: None)
    # advancedGroup starts unchecked; set a value that must be ignored while collapsed.
    priority_spin = dialog.findChild(QtWidgets.QSpinBox, "prioritySpin")
    assert priority_spin is not None
    priority_spin.setValue(80)
    dialog.submit_and_wait()
    body = json.loads(route.calls.last.request.content)
    assert "owner" not in body
    assert "priority" not in body
    assert "max_attempts" not in body


class _CapturingDialog:
    """Stands in for SubmitDialog: records constructor arguments, never builds Qt."""

    instances: ClassVar["list[_CapturingDialog]"] = []

    def __init__(self, session: SubmitterSession, **kwargs: object) -> None:
        self.session = session
        self.kwargs = kwargs
        self.adapter = kwargs.get("adapter")
        self.calls: list[str] = []
        _CapturingDialog.instances.append(self)

    def show(self) -> None:
        self.calls.append("show")

    def raise_(self) -> None:
        self.calls.append("raise_")

    def activateWindow(self) -> None:
        self.calls.append("activateWindow")


def _patch_cli(monkeypatch: pytest.MonkeyPatch, dialog_cls: type) -> None:
    monkeypatch.setattr(qt_dialog, "SubmitDialog", dialog_cls)
    monkeypatch.setattr(cli, "_exec_app", lambda app: 0)


def test_cli_context_opens_dialog_with_context_adapter(
    app: object, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _CapturingDialog.instances = []
    _patch_cli(monkeypatch, _CapturingDialog)
    path = tmp_path / "ctx.json"
    dump_context(MiniAdapter(), str(path))

    assert cli.main(["--context", str(path)]) == 0

    (dialog,) = _CapturingDialog.instances
    assert isinstance(dialog.adapter, ContextAdapter)
    assert dialog.adapter.host_name == "mini"
    assert dialog.kwargs.get("initial_error") is None
    assert dialog.calls == ["show", "raise_", "activateWindow"]
    assert not path.exists()


def test_cli_context_server_url_wins_over_missing_flag_not_explicit_flag(
    app: object, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _CapturingDialog.instances = []
    _patch_cli(monkeypatch, _CapturingDialog)
    for flag, expected in ((["--server", "http://flag:1"], "http://flag:1"), ([], "http://file:2")):
        path = tmp_path / "ctx.json"
        path.write_text(
            json.dumps(
                {
                    "version": 1,
                    "host": {"name": "x"},
                    "scene": {"path": "/a"},
                    "server_url": "http://file:2",
                }
            ),
            encoding="utf-8",
        )
        assert cli.main([*flag, "--context", str(path)]) == 0
        assert _CapturingDialog.instances[-1].session.server_url == expected


@respx.mock
def test_cli_bad_context_file_shows_banner_and_exits_zero(
    app: object, monkeypatch: pytest.MonkeyPatch, tmp_path: Path
) -> None:
    _mock_server()
    made: list[SubmitDialog] = []

    class Recording(SubmitDialog):
        def __init__(self, *args: object, **kwargs: object) -> None:
            super().__init__(*args, **kwargs)  # type: ignore[arg-type]
            made.append(self)

    _patch_cli(monkeypatch, Recording)
    path = tmp_path / "broken.json"
    path.write_text("{not json", encoding="utf-8")

    assert cli.main(["--server", BASE, "--context", str(path)]) == 0

    (dialog,) = made
    assert dialog.adapter is None
    assert not dialog._error_label.isHidden()
    assert "broken.json" in dialog._error_label.text()
    assert path.exists()


@respx.mock
def test_dialog_hides_save_checkbox_for_context_adapter(app: object, tmp_path: Path) -> None:
    _mock_server()
    path = tmp_path / "ctx.json"
    dump_context(MiniAdapter(), str(path))
    handoff = SubmitDialog(SubmitterSession(server_url=BASE), adapter=load_context(path))
    assert handoff._save_scene_check.isHidden()

    in_process = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MiniAdapter())
    # not shown yet, so isHidden() is False only if explicitly visible-flagged
    assert not in_process._save_scene_check.isHidden()


def test_cli_install_host_is_a_stub_exiting_two(capsys: pytest.CaptureFixture[str]) -> None:
    assert cli.main(["install-host", "maya"]) == 2
    assert "not implemented" in capsys.readouterr().err
