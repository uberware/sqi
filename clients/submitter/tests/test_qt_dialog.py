# SPDX-License-Identifier: AGPL-3.0-or-later
"""Offscreen dialog e2e against a respx-mocked server (no DCC required)."""

import json
import os
import threading
import time
from pathlib import Path
from typing import Any, ClassVar

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
from sqi_submitter.core import RenderTarget, SceneContext, SubmitterSession  # noqa: E402
from sqi_submitter.core.errors import SubmitterError  # noqa: E402
from sqi_submitter.core.handoff import ContextAdapter, dump_context, load_context  # noqa: E402
from sqi_submitter.core.submit import SCENE_NOT_SAVED_MESSAGE  # noqa: E402
from sqi_submitter.qt import dialog as qt_dialog  # noqa: E402
from sqi_submitter.qt.dialog import _PER_TARGET_MODE, _WHOLE_SCENE_MODE, SubmitDialog  # noqa: E402
from sqi_submitter.qt.widgets import PREFILL_TIP, build_form  # noqa: E402
from tests.test_adapter_contract import BASE, MiniAdapter  # noqa: E402

QtCore = compat.QtCore
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


# -- multi-target checklist ---------------------------------------------------


class PassAdapter(MiniAdapter):
    """Three targets that each carry a ``Pass`` extra (a target parameter)."""

    def __init__(self, scene_path: str = "/x/s.mini", movie_locked: bool = False) -> None:
        super().__init__()
        self.scene_path = scene_path
        self.movie_locked = movie_locked

    def scene_context(self) -> SceneContext:
        return SceneContext(scene_path=self.scene_path, frame_range="1-4")

    def render_targets(self) -> list[RenderTarget]:
        targets = [RenderTarget(name=n, kind="pass", extra={"Pass": n}) for n in ("A", "B", "C")]
        if self.movie_locked:
            targets[1] = RenderTarget(
                name="B",
                kind="pass",
                frame_range="10-20",
                extra={"Pass": "B", "FramesPerTask": "11"},
                locked=frozenset({"FramesPerTask"}),
            )
        return targets


class SubsetAdapter(PassAdapter):
    """A carries an ``OutputPath`` extra that B lacks: a per-target pre-fill value."""

    def render_targets(self) -> list[RenderTarget]:
        return [
            RenderTarget(name="A", kind="pass", extra={"Pass": "A", "OutputPath": "/a"}),
            RenderTarget(name="B", kind="pass", extra={"Pass": "B"}),
        ]


_MOVIES: "dict[str, RenderTarget]" = {
    # After Effects-shaped: a movie is one file, so it locks its FramesPerTask.
    "M1": RenderTarget(
        name="M1",
        kind="pass",
        extra={"Pass": "M1", "FramesPerTask": "100"},
        locked=frozenset({"FramesPerTask"}),
    ),
    "M2": RenderTarget(
        name="M2",
        kind="pass",
        extra={"Pass": "M2", "FramesPerTask": "50"},
        locked=frozenset({"FramesPerTask"}),
    ),
    "I": RenderTarget(name="I", kind="pass", extra={"Pass": "I"}),
}


class MovieAdapter(PassAdapter):
    """The named ``_MOVIES`` targets, in order."""

    def __init__(self, *names: str) -> None:
        super().__init__()
        self.names = names

    def render_targets(self) -> list[RenderTarget]:
        return [_MOVIES[n] for n in self.names]


def _mock_pass_server(
    responses: "list[httpx.Response] | None" = None,
    extra_params: "list[dict[str, str]] | None" = None,
    sequence: bool = False,
) -> respx.Route:
    """Mock ``pass-render``; ``sequence`` adds ``seq-render`` (no target key)."""
    products = [
        {"name": "mini-render", "title": "Mini Render"},
        {"name": "pass-render", "title": "Pass Render"},
    ]
    if sequence:
        products.append({"name": "seq-render", "title": "Sequence Render"})
        respx.get(f"{BASE}/api/v1/products/seq-render/parameters").mock(
            return_value=httpx.Response(
                200,
                json=[
                    {"name": "Frames", "type": "STRING"},
                    {"name": "FramesPerTask", "type": "INT", "default": "1"},
                ],
            )
        )
    _mock_server(products=products)
    respx.get(f"{BASE}/api/v1/products/pass-render/parameters").mock(
        return_value=httpx.Response(
            200,
            json=[
                {"name": "SceneFile", "type": "PATH"},
                {"name": "Frames", "type": "STRING"},
                {"name": "Pass", "type": "STRING"},
                {"name": "FramesPerTask", "type": "INT", "default": "1"},
                *(extra_params or []),
            ],
        )
    )
    if responses is None:
        responses = [httpx.Response(201, json={"id": f"j{i}", "name": "n"}) for i in range(1, 4)]
    return respx.post(f"{BASE}/api/v1/products/pass-render/jobs").mock(side_effect=responses)


def _select_product(dialog: SubmitDialog, name: str) -> None:
    combo = dialog.findChild(QtWidgets.QComboBox, "productCombo")
    assert combo is not None
    for i in range(combo.count()):
        data = combo.itemData(i)
        if isinstance(data, Product) and data.name == name:
            combo.setCurrentIndex(i)
            return
    raise AssertionError(f"product {name} not in combo")


def _target_list(dialog: SubmitDialog) -> Any:
    widget = dialog.findChild(QtWidgets.QListWidget, "targetList")
    assert widget is not None
    return widget


def _ticked(dialog: SubmitDialog) -> list[str]:
    widget = _target_list(dialog)
    return [
        item.text()
        for item in (widget.item(i) for i in range(widget.count()))
        if item is not None and item.checkState() == QtCore.Qt.CheckState.Checked
    ]


def _tick(dialog: SubmitDialog, name: str, on: bool = True) -> None:
    widget = _target_list(dialog)
    (item,) = widget.findItems(name, QtCore.Qt.MatchFlag.MatchExactly)
    item.setCheckState(QtCore.Qt.CheckState.Checked if on else QtCore.Qt.CheckState.Unchecked)


def _pass_dialog(adapter: "PassAdapter | None" = None) -> SubmitDialog:
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=adapter or PassAdapter())
    _select_product(dialog, "pass-render")
    return dialog


def _capture_boxes(monkeypatch: pytest.MonkeyPatch) -> list[tuple[str, str]]:
    boxes: list[tuple[str, str]] = []

    def record(_parent: object, title: str, text: str, *a: object, **k: object) -> None:
        boxes.append((title, text))

    monkeypatch.setattr(QtWidgets.QMessageBox, "information", record)
    monkeypatch.setattr(QtWidgets.QMessageBox, "warning", record)
    return boxes


def _mode_text(dialog: SubmitDialog) -> str:
    label = dialog.findChild(QtWidgets.QLabel, "modeLabel")
    assert label is not None
    return str(label.text())


@respx.mock
def test_checklist_shown_for_per_target_product(app: object) -> None:
    _mock_pass_server()
    dialog = _pass_dialog()
    widget = _target_list(dialog)
    assert not widget.isHidden()
    assert [widget.item(i).text() for i in range(widget.count())] == ["A", "B", "C"]
    assert _ticked(dialog) == ["A"]  # the first target is ticked by default
    assert dialog.findChild(QtWidgets.QComboBox, "targetCombo") is None


@respx.mock
def test_no_target_control_for_whole_scene_product(app: object) -> None:
    _mock_pass_server()
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=PassAdapter())
    _select_product(dialog, "mini-render")
    assert _target_list(dialog).isHidden()
    assert dialog.findChild(QtWidgets.QComboBox, "targetCombo") is None
    hint = dialog.findChild(QtWidgets.QLabel, "targetHint")
    assert hint is not None
    assert hint.isHidden()


@respx.mock
def test_submit_disabled_with_nothing_ticked(app: object) -> None:
    route = _mock_pass_server()
    dialog = _pass_dialog()
    submit = dialog.findChild(QtWidgets.QPushButton, "submitButton")
    hint = dialog.findChild(QtWidgets.QLabel, "targetHint")
    assert submit is not None and hint is not None
    assert submit.isEnabled()
    assert hint.isHidden()

    _tick(dialog, "A", on=False)
    assert not submit.isEnabled()
    assert not hint.isHidden()
    assert hint.text() == "Tick at least one target."
    dialog.submit_and_wait()
    assert not route.called
    assert not submit.isEnabled()

    _tick(dialog, "C")
    assert submit.isEnabled()
    assert hint.isHidden()


@respx.mock
def test_three_ticked_submits_three_jobs(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    route = _mock_pass_server()
    boxes = _capture_boxes(monkeypatch)
    dialog = _pass_dialog()
    _tick(dialog, "B")
    _tick(dialog, "C")
    dialog.submit_and_wait()

    bodies = [json.loads(c.request.content) for c in route.calls]
    assert [b["parameters"]["Pass"] for b in bodies] == ["A", "B", "C"]
    assert [b["name"] for b in bodies] == [
        "Pass Render - A",
        "Pass Render - B",
        "Pass Render - C",
    ]
    assert all(b["parameters"]["SceneFile"] == "/x/s.mini" for b in bodies)
    ((_title, text),) = boxes
    for name, job_id in (("A", "j1"), ("B", "j2"), ("C", "j3")):
        assert f"{name}: job {job_id}" in text
        assert f"{BASE}/jobs/{job_id}" in text
    assert dialog.session.settings.get("last_product.mini") == "pass-render"


@respx.mock
def test_partial_failure_lists_each_target(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    route = _mock_pass_server(
        responses=[
            httpx.Response(201, json={"id": "j1", "name": "n"}),
            httpx.Response(400, json={"error": "bad pass"}),
            httpx.Response(201, json={"id": "j3", "name": "n"}),
        ]
    )
    boxes = _capture_boxes(monkeypatch)
    dialog = _pass_dialog()
    _tick(dialog, "B")
    _tick(dialog, "C")
    dialog.submit_and_wait()

    assert route.call_count == 3
    ((_title, text),) = boxes
    assert "A: job j1" in text
    assert "B: FAILED — " in text and "bad pass" in text
    assert "C: job j3" in text
    assert f"{BASE}/jobs/j2" not in text


@respx.mock
def test_mode_label_text(app: object) -> None:
    _mock_pass_server()
    dialog = _pass_dialog()
    assert _mode_text(dialog) == _PER_TARGET_MODE
    _select_product(dialog, "mini-render")
    assert _mode_text(dialog) == _WHOLE_SCENE_MODE


@respx.mock
def test_ticks_remembered_per_scene(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    _mock_pass_server()
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog()
    _tick(dialog, "A", on=False)
    _tick(dialog, "C")
    dialog.submit_and_wait()
    assert dialog.session.settings.get("last_targets.mini./x/s.mini") == ["C"]

    assert _ticked(_pass_dialog()) == ["C"]
    # Another scene has nothing remembered: back to the first target.
    assert _ticked(_pass_dialog(PassAdapter(scene_path="/x/other.mini"))) == ["A"]

    # Remembered names that no longer exist are dropped.
    dialog.session.settings.set("last_targets.mini./x/s.mini", ["Gone", "B"])
    assert _ticked(_pass_dialog()) == ["B"]
    dialog.session.settings.set("last_targets.mini./x/s.mini", ["Gone"])
    assert _ticked(_pass_dialog()) == ["A"]


def _field(dialog: SubmitDialog, name: str) -> Any:
    line = dialog.findChild(QtWidgets.QLineEdit, f"field_{name}")
    assert line is not None
    return line


@respx.mock
def test_target_parameter_editor_disabled_when_several_ticked(app: object) -> None:
    _mock_pass_server()
    dialog = _pass_dialog()
    assert _field(dialog, "Pass").isEnabled()
    assert _field(dialog, "Pass").text() == "A"

    _tick(dialog, "B")
    line = _field(dialog, "Pass")
    assert not line.isEnabled()
    assert line.toolTip() == "From each target"
    assert line.text() == "A"  # the form shows the first ticked target
    assert _field(dialog, "Frames").isEnabled()

    _tick(dialog, "A", on=False)
    assert _field(dialog, "Pass").isEnabled()
    assert _field(dialog, "Pass").text() == "B"


@respx.mock
def test_edited_field_applies_to_every_job(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    route = _mock_pass_server()
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog()
    line = _field(dialog, "Frames")
    line.setText("1-2")
    line.editingFinished.emit()
    assert dialog.edited == {"Frames": "1-2"}

    _tick(dialog, "B")  # the edit survives
    assert _field(dialog, "Frames").text() == "1-2"
    dialog.submit_and_wait()
    assert [json.loads(c.request.content)["parameters"]["Frames"] for c in route.calls] == [
        "1-2",
        "1-2",
    ]


def _count_rebuilds(monkeypatch: pytest.MonkeyPatch) -> list[int]:
    """Count the dialog's form builds from now on."""
    calls: list[int] = []

    def counting(*args: Any, **kwargs: Any) -> Any:
        calls.append(1)
        return build_form(*args, **kwargs)

    monkeypatch.setattr(qt_dialog, "build_form", counting)
    return calls


@respx.mock
def test_ticking_a_later_target_keeps_the_form(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    _mock_pass_server()
    dialog = _pass_dialog()
    rebuilds = _count_rebuilds(monkeypatch)
    root = dialog._form_root
    assert _field(dialog, "Pass").toolTip() == PREFILL_TIP

    _tick(dialog, "B")
    assert rebuilds == []
    assert dialog._form_root is root
    assert not _field(dialog, "Pass").isEnabled()
    assert _field(dialog, "Pass").toolTip() == "From each target"

    _tick(dialog, "B", on=False)
    assert rebuilds == []
    assert dialog._form_root is root
    assert _field(dialog, "Pass").isEnabled()
    assert _field(dialog, "Pass").toolTip() == PREFILL_TIP  # what a rebuild would give


@respx.mock
def test_changing_the_first_ticked_target_rebuilds_with_its_prefill(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    _mock_pass_server()
    dialog = _pass_dialog()
    rebuilds = _count_rebuilds(monkeypatch)
    _tick(dialog, "B")
    _tick(dialog, "A", on=False)
    assert rebuilds == [1]
    assert _field(dialog, "Pass").text() == "B"
    assert _field(dialog, "Pass").isEnabled()


@respx.mock
def test_edit_survives_a_later_tick_without_a_rebuild(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    _mock_pass_server()
    dialog = _pass_dialog()
    rebuilds = _count_rebuilds(monkeypatch)
    line = _field(dialog, "Frames")
    line.setText("1-2")
    line.editingFinished.emit()

    _tick(dialog, "B")
    assert rebuilds == []
    assert dialog.edited == {"Frames": "1-2"}
    assert _field(dialog, "Frames").text() == "1-2"
    assert dialog._model is not None and dialog._model.values()["Frames"] == "1-2"


@respx.mock
def test_dropped_target_parameter_edit_rebuilds(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    _mock_pass_server()
    dialog = _pass_dialog()
    rebuilds = _count_rebuilds(monkeypatch)
    line = _field(dialog, "Pass")
    line.setText("custom")
    line.editingFinished.emit()

    _tick(dialog, "B")
    assert rebuilds == [1]
    assert dialog.edited == {}
    assert _field(dialog, "Pass").text() == "A"  # back to the first target's own


@respx.mock
def test_unticking_a_locking_target_clears_its_tooltip_without_a_rebuild(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    _mock_pass_server()
    dialog = _pass_dialog(PassAdapter(movie_locked=True))
    rebuilds = _count_rebuilds(monkeypatch)
    _tick(dialog, "B")
    spin = dialog.findChild(QtWidgets.QSpinBox, "field_FramesPerTask")
    assert spin is not None
    assert spin.toolTip() == "Kept per target (e.g. a movie renders as one task)"

    _tick(dialog, "B", on=False)
    assert rebuilds == []
    assert spin.toolTip() == ""


@respx.mock
def test_tick_change_clears_field_errors_like_a_rebuild(app: object) -> None:
    _mock_pass_server()
    dialog = _pass_dialog()
    dialog._apply_field_errors({"Frames": "bad frames"})
    assert _field(dialog, "Frames").toolTip() == "bad frames"

    _tick(dialog, "B")
    assert _field(dialog, "Frames").styleSheet() == ""
    assert _field(dialog, "Frames").toolTip() == PREFILL_TIP


@respx.mock
def test_untouched_field_takes_each_targets_value(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    route = _mock_pass_server()
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog(PassAdapter(movie_locked=True))
    _tick(dialog, "B")
    dialog.submit_and_wait()
    frames = [json.loads(c.request.content)["parameters"]["Frames"] for c in route.calls]
    assert frames == ["1-4", "10-20"]


@respx.mock
def test_locked_key_editor_tooltip_and_value(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    route = _mock_pass_server()
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog(PassAdapter(movie_locked=True))
    spin = dialog.findChild(QtWidgets.QSpinBox, "field_FramesPerTask")
    assert spin is not None
    assert spin.toolTip() == ""  # no ticked target locks it yet

    _tick(dialog, "B")
    spin = dialog.findChild(QtWidgets.QSpinBox, "field_FramesPerTask")
    assert spin is not None
    assert spin.isEnabled()
    assert spin.toolTip() == "Kept per target (e.g. a movie renders as one task)"
    spin.setValue(5)
    assert dialog.edited == {"FramesPerTask": "5"}
    dialog.submit_and_wait()
    per_task = [json.loads(c.request.content)["parameters"]["FramesPerTask"] for c in route.calls]
    assert per_task == ["5", "11"]  # the movie target keeps its own value


_LOCKED = "Kept per target (e.g. a movie renders as one task)"


def _spin(dialog: SubmitDialog, name: str) -> Any:
    spin = dialog.findChild(QtWidgets.QSpinBox, f"field_{name}")
    assert spin is not None
    return spin


def _posted(route: respx.Route, name: str) -> list[str]:
    return [json.loads(c.request.content)["parameters"][name] for c in route.calls]


@respx.mock
def test_single_queued_movie_is_never_split(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    """One movie means every target carries FramesPerTask: it is still not the artist's."""
    route = _mock_pass_server()
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog(MovieAdapter("M1"))
    assert _ticked(dialog) == ["M1"]
    spin = _spin(dialog, "FramesPerTask")
    assert not spin.isEnabled()
    assert spin.toolTip() == _LOCKED
    assert spin.value() == 100

    spin.setValue(10)  # the artist cannot reach a disabled editor; force it anyway
    dialog.edited["FramesPerTask"] = "10"
    dialog.submit_and_wait()
    assert _posted(route, "FramesPerTask") == ["100"]


@respx.mock
def test_every_ticked_movie_keeps_its_frames_per_task(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    route = _mock_pass_server()
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog(MovieAdapter("M1", "M2"))
    _tick(dialog, "M2")
    spin = _spin(dialog, "FramesPerTask")
    assert not spin.isEnabled()
    assert spin.toolTip() == _LOCKED

    dialog.edited["FramesPerTask"] = "10"
    dialog.submit_and_wait()
    assert _posted(route, "FramesPerTask") == ["100", "50"]


@respx.mock
def test_movie_and_images_edit_applies_to_the_images_only(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    route = _mock_pass_server()
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog(MovieAdapter("M1", "I"))
    _tick(dialog, "I")
    spin = _spin(dialog, "FramesPerTask")
    assert spin.isEnabled()
    assert spin.toolTip() == _LOCKED

    spin.setValue(5)
    assert dialog.edited == {"FramesPerTask": "5"}
    dialog.submit_and_wait()
    assert _posted(route, "FramesPerTask") == ["100", "5"]


@respx.mock
def test_movie_lock_follows_the_ticks(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    _mock_pass_server()
    dialog = _pass_dialog(MovieAdapter("M1", "I"))
    _tick(dialog, "I")
    spin = _spin(dialog, "FramesPerTask")
    spin.setValue(5)
    rebuilds = _count_rebuilds(monkeypatch)

    _tick(dialog, "I", on=False)  # only the movie is left
    assert rebuilds == []
    assert not spin.isEnabled()
    assert spin.toolTip() == _LOCKED
    assert spin.value() == 100  # what the movie submits, not the images' edit
    assert dialog.edited == {"FramesPerTask": "5"}

    _tick(dialog, "I")
    assert rebuilds == []
    assert spin.isEnabled()
    assert spin.toolTip() == _LOCKED

    _tick(dialog, "M1", on=False)  # only the images are left
    spin = _spin(dialog, "FramesPerTask")
    assert spin.isEnabled()
    assert spin.toolTip() == ""
    assert spin.value() == 5

    _tick(dialog, "M1")
    spin = _spin(dialog, "FramesPerTask")
    assert spin.isEnabled()
    assert spin.toolTip() == _LOCKED


@respx.mock
def test_sequence_product_is_whole_scene_for_an_all_movie_queue(app: object) -> None:
    """A locked FramesPerTask is a pre-fill value: it never makes a product per-target."""
    _mock_pass_server(sequence=True)
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MovieAdapter("M1", "M2"))
    _select_product(dialog, "seq-render")
    assert _target_list(dialog).isHidden()
    assert _mode_text(dialog) == _WHOLE_SCENE_MODE
    assert _spin(dialog, "FramesPerTask").isEnabled()

    _select_product(dialog, "pass-render")
    assert not _target_list(dialog).isHidden()
    assert _mode_text(dialog) == _PER_TARGET_MODE


@respx.mock
def test_single_ticked_target_honours_an_edit_to_its_target_parameter(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    """One ticked target behaves like today's single-target pre-fill."""
    route = _mock_pass_server()
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog()
    line = _field(dialog, "Pass")
    line.setText("custom")
    line.editingFinished.emit()
    dialog.submit_and_wait()
    body = json.loads(route.calls.last.request.content)
    assert body["parameters"]["Pass"] == "custom"
    assert body["name"] == "Pass Render - A"


@respx.mock
def test_whole_scene_submit_reports_one_job(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    route = _mock_server()
    boxes = _capture_boxes(monkeypatch)
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=MiniAdapter())
    dialog.submit_and_wait()
    assert route.call_count == 1
    ((_title, text),) = boxes
    assert "job j1" in text
    assert f"{BASE}/jobs/j1" in text


@respx.mock
def test_edit_to_a_key_only_some_targets_share_applies_to_every_job(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Whether an edit is honoured must not depend on which other targets are ticked."""
    route = _mock_pass_server(
        extra_params=[{"name": "OutputPath", "type": "PATH", "default": "/default"}]
    )
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog(SubsetAdapter())
    assert _ticked(dialog) == ["A"]
    line = _field(dialog, "OutputPath")
    assert line.text() == "/a"
    line.setText("/edited")
    line.editingFinished.emit()
    assert dialog.edited == {"OutputPath": "/edited"}

    dialog.submit_and_wait()  # only A ticked
    assert json.loads(route.calls.last.request.content)["parameters"]["OutputPath"] == "/edited"

    _tick(dialog, "B")
    assert _field(dialog, "OutputPath").text() == "/edited"
    assert _field(dialog, "OutputPath").isEnabled()
    dialog.submit_and_wait()  # A and B ticked
    outputs = [json.loads(c.request.content)["parameters"]["OutputPath"] for c in route.calls]
    assert outputs == ["/edited", "/edited", "/edited"]


# -- After Effects movie targets ----------------------------------------------

# The shape the After Effects panel's sqiTarget writes (pinned by
# tests/test_aftereffects_panel.py): a movie locks its frame count and its own range.
_AE_TARGETS: "dict[str, RenderTarget]" = {
    "Movie": RenderTarget(
        name="Movie",
        kind="render_queue_item",
        frame_range="1-100",
        extra={"RenderQueueItem": "1", "FramesPerTask": "100", "Frames": "1-100"},
        locked=frozenset({"FramesPerTask", "Frames"}),
    ),
    "Promo": RenderTarget(
        name="Promo",
        kind="render_queue_item",
        frame_range="1-30",
        extra={"RenderQueueItem": "2", "FramesPerTask": "30", "Frames": "1-30"},
        locked=frozenset({"FramesPerTask", "Frames"}),
    ),
    "Frames": RenderTarget(
        name="Frames",
        kind="render_queue_item",
        frame_range="1001-1250",
        extra={"RenderQueueItem": "3"},
    ),
}


class AfterEffectsAdapter(PassAdapter):
    """The named ``_AE_TARGETS``, in order."""

    def __init__(self, *names: str) -> None:
        super().__init__()
        self.names = names

    def scene_context(self) -> SceneContext:
        return SceneContext(scene_path="/x/spot.aep", frame_range="1-1250")

    def render_targets(self) -> list[RenderTarget]:
        return [_AE_TARGETS[n] for n in self.names]


def _mock_ae_server() -> respx.Route:
    """Mock ``ae-render`` with the After Effects presets' parameters."""
    _mock_server(products=[{"name": "ae-render", "title": "AE Render"}])
    respx.get(f"{BASE}/api/v1/products/ae-render/parameters").mock(
        return_value=httpx.Response(
            200,
            json=[
                {"name": "Frames", "type": "STRING"},
                {"name": "RenderQueueItem", "type": "INT"},
                {"name": "FramesPerTask", "type": "INT", "default": "10"},
            ],
        )
    )
    responses = [httpx.Response(201, json={"id": f"j{i}", "name": "n"}) for i in range(1, 4)]
    return respx.post(f"{BASE}/api/v1/products/ae-render/jobs").mock(side_effect=responses)


def _ae_dialog(*names: str) -> SubmitDialog:
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=AfterEffectsAdapter(*names))
    _select_product(dialog, "ae-render")
    assert _mode_text(dialog) == _PER_TARGET_MODE
    return dialog


@respx.mock
def test_single_movie_frames_are_its_own(app: object, monkeypatch: pytest.MonkeyPatch) -> None:
    """A widened, stepped or gapped range would split the movie: Frames is not the artist's."""
    route = _mock_ae_server()
    _capture_boxes(monkeypatch)
    dialog = _ae_dialog("Movie", "Frames")
    assert _ticked(dialog) == ["Movie"]
    line = _field(dialog, "Frames")
    assert not line.isEnabled()
    assert line.toolTip() == _LOCKED
    assert line.text() == "1-100"
    assert not _spin(dialog, "FramesPerTask").isEnabled()
    assert _spin(dialog, "FramesPerTask").value() == 100

    line.setText("1-150")  # the artist cannot reach a disabled editor; force it anyway
    dialog.edited["Frames"] = "1-150"
    dialog.edited["FramesPerTask"] = "10"
    dialog.submit_and_wait()
    assert _posted(route, "Frames") == ["1-100"]
    assert _posted(route, "FramesPerTask") == ["100"]
    assert _posted(route, "RenderQueueItem") == ["1"]


@respx.mock
def test_movie_and_sequence_frames_edit_applies_to_the_sequence_only(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    route = _mock_ae_server()
    _capture_boxes(monkeypatch)
    dialog = _ae_dialog("Movie", "Frames")
    _tick(dialog, "Frames")
    line = _field(dialog, "Frames")
    assert line.isEnabled()
    assert line.toolTip() == _LOCKED

    line.setText("5-9")
    line.editingFinished.emit()
    assert dialog.edited == {"Frames": "5-9"}
    _spin(dialog, "FramesPerTask").setValue(2)
    dialog.submit_and_wait()
    assert _posted(route, "Frames") == ["1-100", "5-9"]
    assert _posted(route, "FramesPerTask") == ["100", "2"]
    assert _posted(route, "RenderQueueItem") == ["1", "3"]


@respx.mock
def test_every_ticked_movie_keeps_its_own_frames(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    route = _mock_ae_server()
    _capture_boxes(monkeypatch)
    dialog = _ae_dialog("Movie", "Promo", "Frames")
    _tick(dialog, "Promo")
    line = _field(dialog, "Frames")
    assert not line.isEnabled()
    assert line.toolTip() == _LOCKED
    assert line.text() == "1-100"  # the first ticked movie's own

    dialog.edited["Frames"] = "1-150"
    dialog.edited["FramesPerTask"] = "10"
    dialog.submit_and_wait()
    assert _posted(route, "Frames") == ["1-100", "1-30"]
    assert _posted(route, "FramesPerTask") == ["100", "30"]


# -- host save failures -------------------------------------------------------


class _SaveRaisesAdapter(MiniAdapter):
    """A modified scene whose host save raises, as Maya's ``cmds.file`` can."""

    def __init__(self) -> None:
        super().__init__(modified=True)

    def save_scene(self) -> bool:
        raise RuntimeError("Permission denied: /x/s.mini")


def test_worker_reports_a_host_exception_as_failed(app: object) -> None:
    """An exception that is not a SubmitterError must still reach the artist, not die."""

    def boom() -> list[Any]:
        raise RuntimeError("Permission denied: /x/s.mini")

    worker = qt_dialog._SubmitWorker(boom)
    failed: list[object] = []
    done: list[object] = []
    worker.failed.connect(failed.append)
    worker.done.connect(done.append)
    worker.run()  # synchronously, on this thread: the signals are delivered directly
    assert done == []
    [error] = failed
    assert isinstance(error, SubmitterError)
    assert "Permission denied: /x/s.mini" in error.user_message


@respx.mock
def test_host_save_exception_shows_banner(app: object) -> None:
    route = _mock_server()
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=_SaveRaisesAdapter())
    dialog._save_scene_check.setChecked(True)
    dialog.submit_and_wait()
    assert not route.called
    error_label = dialog.findChild(QtWidgets.QLabel, "errorLabel")
    assert error_label is not None
    assert not error_label.isHidden()
    assert "Permission denied: /x/s.mini" in error_label.text()


# -- host calls stay on the GUI thread ------------------------------------------


class _ThreadLog:
    """One ordered log of host calls and POSTs, each with the thread it ran on."""

    def __init__(self) -> None:
        self.entries: list[tuple[str, threading.Thread]] = []

    def add(self, name: str) -> None:
        self.entries.append((name, threading.current_thread()))

    def names(self) -> list[str]:
        return [name for name, _ in self.entries]

    def threads(self, name: str) -> list[threading.Thread]:
        return [thread for n, thread in self.entries if n == name]

    def post(self, response: httpx.Response) -> Any:
        """A respx side effect that logs each POST, then answers ``response``."""

        def side_effect(_request: httpx.Request) -> httpx.Response:
            self.add("post")
            return response

        return side_effect


class _ThreadRecordingAdapter(PassAdapter):
    """Logs every host call (pymxs, ``maya.cmds`` and the like are main-thread only)."""

    def __init__(self, log: _ThreadLog, scene_path: str = "/x/s.mini", **kwargs: bool) -> None:
        super().__init__(scene_path=scene_path)
        self.log = log
        self.modified = kwargs.get("modified", True)
        self.can_save = kwargs.get("can_save", True)

    def scene_context(self) -> SceneContext:
        self.log.add("scene_context")
        return super().scene_context()

    def render_targets(self) -> list[RenderTarget]:
        self.log.add("render_targets")
        return super().render_targets()

    def is_scene_modified(self) -> bool:
        self.log.add("is_scene_modified")
        return self.modified

    def save_scene(self) -> bool:
        self.log.add("save_scene")
        return self.can_save


def _submit_on_worker(dialog: SubmitDialog) -> None:
    """Press Submit for real: the submit runs on the dialog's worker QThread."""
    dialog._on_submit()
    worker = dialog._worker
    assert worker is not None
    assert worker.wait(10_000)
    deadline = time.monotonic() + 10
    while dialog._submitting and time.monotonic() < deadline:
        QtWidgets.QApplication.processEvents()
    assert not dialog._submitting


def _assert_host_calls_on_main_thread(log: _ThreadLog) -> None:
    main = threading.main_thread()
    host = [(n, t) for n, t in log.entries if n != "post"]
    assert host, "the submit made no host call at all"
    assert all(t is main for _, t in host), [(n, t.name) for n, t in host]


@respx.mock
def test_per_target_submit_makes_no_host_call_on_the_worker(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    log = _ThreadLog()
    route = _mock_pass_server()
    route.mock(side_effect=log.post(httpx.Response(201, json={"id": "j", "name": "n"})))
    _capture_boxes(monkeypatch)
    dialog = _pass_dialog(_ThreadRecordingAdapter(log))
    dialog._save_scene_check.setChecked(True)
    _tick(dialog, "B")
    _tick(dialog, "C")
    log.entries.clear()

    _submit_on_worker(dialog)

    assert route.call_count == 3
    # The POSTs ran on the worker thread, so this really exercised it.
    assert all(t is not threading.main_thread() for t in log.threads("post"))
    _assert_host_calls_on_main_thread(log)
    names = log.names()
    assert names.count("save_scene") == 1
    assert names.count("scene_context") == 1  # one snapshot per submit, not one per target
    assert names.index("save_scene") < names.index("post")


@respx.mock
def test_whole_scene_submit_makes_no_host_call_on_the_worker(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    log = _ThreadLog()
    _mock_pass_server()
    route = respx.post(f"{BASE}/api/v1/products/mini-render/jobs").mock(
        side_effect=log.post(httpx.Response(201, json={"id": "j1", "name": "n"}))
    )
    boxes = _capture_boxes(monkeypatch)
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=_ThreadRecordingAdapter(log))
    _select_product(dialog, "mini-render")
    dialog._save_scene_check.setChecked(True)
    log.entries.clear()

    _submit_on_worker(dialog)

    assert route.call_count == 1
    assert json.loads(route.calls.last.request.content)["parameters"]["SceneFile"] == "/x/s.mini"
    assert all(t is not threading.main_thread() for t in log.threads("post"))
    _assert_host_calls_on_main_thread(log)
    names = log.names()
    assert names.count("save_scene") == 1
    assert names.count("scene_context") == 1
    assert names.index("save_scene") < names.index("post")
    ((_title, text),) = boxes
    assert "job j1" in text


@respx.mock
def test_per_target_failed_save_posts_nothing_and_reports_each_target(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    log = _ThreadLog()
    route = _mock_pass_server()
    boxes = _capture_boxes(monkeypatch)
    dialog = _pass_dialog(_ThreadRecordingAdapter(log, can_save=False))
    dialog._save_scene_check.setChecked(True)
    _tick(dialog, "B")
    _tick(dialog, "C")
    log.entries.clear()

    _submit_on_worker(dialog)

    assert not route.called
    _assert_host_calls_on_main_thread(log)
    assert log.names().count("save_scene") == 1
    ((title, text),) = boxes
    assert title == "No jobs submitted"
    assert text == "\n".join(f"{n}: FAILED — {SCENE_NOT_SAVED_MESSAGE}" for n in ("A", "B", "C"))


@respx.mock
def test_whole_scene_failed_save_posts_nothing_and_shows_the_banner(app: object) -> None:
    log = _ThreadLog()
    route = _mock_server()
    dialog = SubmitDialog(
        SubmitterSession(server_url=BASE), adapter=_ThreadRecordingAdapter(log, can_save=False)
    )
    dialog._save_scene_check.setChecked(True)
    log.entries.clear()

    _submit_on_worker(dialog)

    assert not route.called
    _assert_host_calls_on_main_thread(log)
    assert log.names().count("save_scene") == 1
    error_label = dialog.findChild(QtWidgets.QLabel, "errorLabel")
    assert error_label is not None
    assert error_label.text() == SCENE_NOT_SAVED_MESSAGE


@respx.mock
def test_untitled_scene_still_asks_to_save_first(app: object) -> None:
    """A never-saved scene's save fails too; the banner keeps naming the real cause."""
    route = _mock_server()
    adapter = _ThreadRecordingAdapter(_ThreadLog(), scene_path="", can_save=False)
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=adapter)
    dialog._save_scene_check.setChecked(True)

    _submit_on_worker(dialog)

    assert not route.called
    error_label = dialog.findChild(QtWidgets.QLabel, "errorLabel")
    assert error_label is not None
    assert error_label.text().startswith("Save your scene before submitting")


@respx.mock
def test_unchecked_save_never_saves_a_modified_scene(
    app: object, monkeypatch: pytest.MonkeyPatch
) -> None:
    log = _ThreadLog()
    route = _mock_server()
    _capture_boxes(monkeypatch)
    dialog = SubmitDialog(SubmitterSession(server_url=BASE), adapter=_ThreadRecordingAdapter(log))
    dialog._save_scene_check.setChecked(False)
    log.entries.clear()

    _submit_on_worker(dialog)

    assert route.call_count == 1
    assert "save_scene" not in log.names()
    _assert_host_calls_on_main_thread(log)
