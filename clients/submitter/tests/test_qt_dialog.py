# SPDX-License-Identifier: AGPL-3.0-or-later
"""Offscreen dialog e2e against a respx-mocked server (no DCC required)."""

import json
import os

import httpx
import pytest
import respx

os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
compat = pytest.importorskip("sqi_submitter.qt._compat")
if not compat.QT_BINDING:
    pytest.skip("no Qt binding installed", allow_module_level=True)

from sqi_client import SqiClient  # noqa: E402
from sqi_client.models import Product  # noqa: E402
from sqi_submitter.core import SubmitterSession  # noqa: E402
from sqi_submitter.qt.dialog import SubmitDialog, main  # noqa: E402
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
    # tests/test_session.py::test_farms_and_queues_fetch); the brief's mock
    # used {"items": [...]} which does not match SqiClient.list_farms and
    # would make farmCombo stay empty. Fixed to a bare array here.
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


def test_main_is_importable_entry_point() -> None:
    assert callable(main)


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
    dialog.findChild(QtWidgets.QSpinBox, "prioritySpin").setValue(80)
    dialog.findChild(QtWidgets.QSpinBox, "maxAttemptsSpin").setValue(5)
    dialog.findChild(QtWidgets.QLineEdit, "ownerEdit").setText("alice")
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
    dialog.findChild(QtWidgets.QSpinBox, "prioritySpin").setValue(80)
    dialog.submit_and_wait()
    body = json.loads(route.calls.last.request.content)
    assert "owner" not in body
    assert "priority" not in body
    assert "max_attempts" not in body
