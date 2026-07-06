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

from sqi_submitter.core import SubmitterSession  # noqa: E402
from sqi_submitter.qt.dialog import SubmitDialog, main  # noqa: E402
from tests.test_adapter_contract import BASE, MiniAdapter  # noqa: E402

QtWidgets = compat.QtWidgets

pytestmark = pytest.mark.usefixtures("_clean_settings")


@pytest.fixture(scope="module")
def app() -> object:
    return QtWidgets.QApplication.instance() or QtWidgets.QApplication([])


def _mock_server() -> respx.Route:
    respx.get(f"{BASE}/api/v1/products").mock(
        return_value=httpx.Response(200, json=[{"name": "mini-render", "title": "Mini Render"}])
    )
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


def test_main_is_importable_entry_point() -> None:
    assert callable(main)
