# SPDX-License-Identifier: AGPL-3.0-or-later
"""Generic Qt submit dialog: product → target → params → farm/queue → submit."""

from __future__ import annotations

import argparse
from collections.abc import Mapping
from typing import Any

from sqi_client.models import Farm, Job, Product, ProductParameter, Queue
from sqi_submitter.core import (
    FormInvalidError,
    FormModel,
    HostAdapter,
    RenderTarget,
    SubmitterError,
    SubmitterSession,
    group_products,
    prefill,
    submit_form,
)
from sqi_submitter.qt._compat import QtCore, QtGui, QtWidgets, require_qt
from sqi_submitter.qt.widgets import build_form

__all__ = ["SubmitDialog", "main", "open_for_adapter"]

_SUGGESTED_LABEL = "Suggested —"
_ALL_PRODUCTS_LABEL = "All —"
_SCENE_SETTINGS_LABEL = "Scene settings"
_ERROR_STYLE = "border: 1px solid red;"
_STALE_MODEL_MESSAGE = "Product parameters failed to load — reload and try again."
_EMPTY_CATALOG_MESSAGE = (
    "No products are available on this server. Ask your admin to install a "
    "preset from the preset library (Admin → Preset Library), then Reload."
)
_CLOSE_WAIT_MS = 10_000


class _SubmitWorker(QtCore.QThread):
    """Runs :func:`submit_form` off the GUI thread; reports back via signals."""

    done = QtCore.Signal(object)
    failed = QtCore.Signal(object)

    def __init__(
        self,
        session: SubmitterSession,
        product_name: str,
        model: FormModel,
        *,
        farm_id: str,
        queue_id: str,
        job_name: str | None,
        adapter: HostAdapter | None,
        save_scene: bool,
    ) -> None:
        super().__init__()
        self._session = session
        self._product_name = product_name
        self._model = model
        self._farm_id = farm_id
        self._queue_id = queue_id
        self._job_name = job_name
        self._adapter = adapter
        self._save_scene = save_scene

    def run(self) -> None:
        try:
            job = submit_form(
                self._session,
                self._product_name,
                self._model,
                farm_id=self._farm_id,
                queue_id=self._queue_id,
                job_name=self._job_name,
                adapter=self._adapter,
                save_scene=self._save_scene,
            )
        except SubmitterError as exc:
            self.failed.emit(exc)
        else:
            self.done.emit(job)


class SubmitDialog(QtWidgets.QDialog):
    """Product → target → parameters → farm/queue → submit, one dialog."""

    def __init__(
        self,
        session: SubmitterSession,
        adapter: HostAdapter | None = None,
        parent: QtWidgets.QWidget | None = None,
    ) -> None:
        require_qt()
        super().__init__(parent)
        self.session = session
        self.adapter = adapter

        self._products: list[Product] = []
        self._model: FormModel | None = None
        self._model_product: str | None = None
        self._form_root: QtWidgets.QWidget | None = None
        self._worker: _SubmitWorker | None = None

        self.setWindowTitle("Submit to sqi")
        self._build_ui()
        self._wire_signals()
        self._load()

    # -- construction -----------------------------------------------------

    def _build_ui(self) -> None:
        self._error_label = QtWidgets.QLabel(self)
        self._error_label.setObjectName("errorLabel")
        self._error_label.setWordWrap(True)
        self._error_label.hide()

        self._reload_button = QtWidgets.QPushButton("Reload", self)
        self._reload_button.setObjectName("reloadButton")
        self._reload_button.hide()

        self._product_combo = QtWidgets.QComboBox(self)
        self._product_combo.setObjectName("productCombo")

        self._target_combo = QtWidgets.QComboBox(self)
        self._target_combo.setObjectName("targetCombo")
        self._target_combo.hide()

        self._form_scroll = QtWidgets.QScrollArea(self)
        self._form_scroll.setWidgetResizable(True)

        self._job_name_edit = QtWidgets.QLineEdit(self)
        self._job_name_edit.setObjectName("jobNameEdit")

        self._farm_combo = QtWidgets.QComboBox(self)
        self._farm_combo.setObjectName("farmCombo")
        self._queue_combo = QtWidgets.QComboBox(self)
        self._queue_combo.setObjectName("queueCombo")

        self._save_scene_check = QtWidgets.QCheckBox("Save scene before submit", self)
        self._save_scene_check.setObjectName("saveSceneCheck")
        self._save_scene_check.setVisible(self.adapter is not None)
        if self.adapter is not None:
            self._save_scene_check.setChecked(
                bool(self.session.settings.get("save_before_submit", True))
            )

        self._submit_button = QtWidgets.QPushButton("Submit", self)
        self._submit_button.setObjectName("submitButton")
        self._close_button = QtWidgets.QPushButton("Close", self)
        self._close_button.setObjectName("closeButton")

        error_row = QtWidgets.QHBoxLayout()
        error_row.addWidget(self._error_label, 1)
        error_row.addWidget(self._reload_button)

        farm_row = QtWidgets.QHBoxLayout()
        farm_row.addWidget(self._farm_combo, 1)
        farm_row.addWidget(self._queue_combo, 1)

        button_row = QtWidgets.QHBoxLayout()
        button_row.addStretch(1)
        button_row.addWidget(self._submit_button)
        button_row.addWidget(self._close_button)

        layout = QtWidgets.QVBoxLayout(self)
        layout.addLayout(error_row)
        layout.addWidget(self._product_combo)
        layout.addWidget(self._target_combo)
        layout.addWidget(self._form_scroll, 1)
        layout.addWidget(QtWidgets.QLabel("Job name", self))
        layout.addWidget(self._job_name_edit)
        layout.addLayout(farm_row)
        layout.addWidget(self._save_scene_check)
        layout.addLayout(button_row)

    def _wire_signals(self) -> None:
        self._product_combo.currentIndexChanged.connect(self._on_product_changed)
        self._target_combo.currentIndexChanged.connect(self._on_target_changed)
        self._farm_combo.currentIndexChanged.connect(self._on_farm_changed)
        self._reload_button.clicked.connect(self._load)
        self._submit_button.clicked.connect(self._on_submit)
        self._close_button.clicked.connect(self.close)

    # -- loading ------------------------------------------------------------

    def _load(self) -> None:
        self._clear_error()

        try:
            products = self.session.products()
        except SubmitterError as exc:
            self._show_error(exc.user_message)
        else:
            self._populate_products(products)
            self._populate_targets()
            self._rebuild_form()
            if not products:
                self._show_error(_EMPTY_CATALOG_MESSAGE)
                self._submit_button.setEnabled(False)

        try:
            farms = self.session.farms()
        except SubmitterError as exc:
            self._show_error(exc.user_message)
        else:
            self._populate_farms(farms)
            self._on_farm_changed(self._farm_combo.currentIndex())

    def _populate_products(self, products: list[Product]) -> None:
        self._products = products
        host_token = self.adapter.host_name if self.adapter is not None else ""
        suggested, rest = group_products(products, host_token)

        combo = self._product_combo
        combo.blockSignals(True)
        combo.clear()
        self._add_product_group(_SUGGESTED_LABEL, suggested)
        self._add_product_group(_ALL_PRODUCTS_LABEL, rest)

        remembered = self.session.settings.get(f"last_product.{host_token}")
        self._select_product(remembered)
        combo.blockSignals(False)

    def _add_product_group(self, label: str, products: list[Product]) -> None:
        if not products:
            return
        combo = self._product_combo
        if combo.count():
            combo.insertSeparator(combo.count())
        header_index = combo.count()
        combo.addItem(label)
        self._disable_item(header_index)
        for product in products:
            combo.addItem(product.title or product.name, product)

    def _disable_item(self, index: int) -> None:
        model = self._product_combo.model()
        if isinstance(model, QtGui.QStandardItemModel):
            item = model.item(index)
            if item is not None:
                item.setFlags(item.flags() & ~QtCore.Qt.ItemFlag.ItemIsEnabled)

    def _select_product(self, remembered: object) -> None:
        combo = self._product_combo
        for i in range(combo.count()):
            data = combo.itemData(i)
            if isinstance(data, Product) and data.name == remembered:
                combo.setCurrentIndex(i)
                return
        for i in range(combo.count()):
            if isinstance(combo.itemData(i), Product):
                combo.setCurrentIndex(i)
                return

    def _populate_targets(self) -> None:
        combo = self._target_combo
        combo.blockSignals(True)
        combo.clear()
        targets = self.adapter.render_targets() if self.adapter is not None else []
        if targets:
            combo.addItem(_SCENE_SETTINGS_LABEL, None)
            for target in targets:
                combo.addItem(target.name, target)
            combo.setCurrentIndex(0)
        combo.setVisible(bool(targets))
        combo.blockSignals(False)

    def _populate_farms(self, farms: list[Farm]) -> None:
        combo = self._farm_combo
        combo.blockSignals(True)
        combo.clear()
        for farm in farms:
            combo.addItem(farm.name, farm)
        combo.blockSignals(False)

    def _populate_queues(self, queues: list[Queue]) -> None:
        combo = self._queue_combo
        combo.blockSignals(True)
        combo.clear()
        for queue in queues:
            combo.addItem(queue.name, queue)
        combo.blockSignals(False)

    def _on_farm_changed(self, _index: int) -> None:
        farm = self._farm_combo.currentData()
        if not isinstance(farm, Farm):
            self._populate_queues([])
            return
        try:
            queues = self.session.queues(farm.id)
        except SubmitterError as exc:
            self._show_error(exc.user_message)
            return
        self._populate_queues(queues)

    # -- form -----------------------------------------------------------

    def _current_product(self) -> Product | None:
        data = self._product_combo.currentData()
        return data if isinstance(data, Product) else None

    def _current_target(self) -> RenderTarget | None:
        data = self._target_combo.currentData()
        return data if isinstance(data, RenderTarget) else None

    def _on_product_changed(self, _index: int) -> None:
        self._rebuild_form()

    def _on_target_changed(self, _index: int) -> None:
        self._rebuild_form()

    def _rebuild_form(self) -> None:
        product = self._current_product()
        if product is None:
            return
        try:
            parameters: list[ProductParameter] = self.session.parameters(product.name)
        except SubmitterError as exc:
            # The combo already moved: whatever model we hold belongs to the
            # previous product. Mark it stale so submit refuses to post the
            # old fields under the new product's name.
            self._model_product = None
            self._submit_button.setEnabled(False)
            self._show_error(exc.user_message)
            return

        model = FormModel.from_parameters(parameters)
        if self.adapter is not None:
            context = self.adapter.scene_context()
            model.apply_prefill(prefill(parameters, context, self._current_target()))
        self._model = model
        self._model_product = product.name
        self._submit_button.setEnabled(True)

        old = self._form_scroll.takeWidget()
        self._form_scroll.setWidget(build_form(model))
        self._form_root = self._form_scroll.widget()
        if old is not None:
            old.deleteLater()

        self._job_name_edit.setText(product.title or product.name)

    # -- submit -----------------------------------------------------------

    def _submit_kwargs(self) -> dict[str, Any]:
        farm = self._farm_combo.currentData()
        queue = self._queue_combo.currentData()
        return {
            "farm_id": farm.id if isinstance(farm, Farm) else "",
            "queue_id": queue.id if isinstance(queue, Queue) else "",
            "job_name": self._job_name_edit.text() or None,
            "adapter": self.adapter,
            "save_scene": (
                self._save_scene_check.isChecked() if self.adapter is not None else True
            ),
        }

    def _ready_product(self) -> Product | None:
        """The selected product, only if the current form model belongs to it.

        Guards against a stale model: when a product change's parameters fetch
        failed, ``self._model`` still holds the previous product's fields and
        must never be posted under the newly selected product's name.
        """
        product = self._current_product()
        if product is None:
            return None
        if self._model is None or self._model_product != product.name:
            self._show_error(_STALE_MODEL_MESSAGE)
            return None
        return product

    def _on_submit(self) -> None:
        product = self._ready_product()
        if product is None or self._model is None:
            return
        kwargs = self._submit_kwargs()
        # Capture at submit-start: the async submit must persist what was
        # actually submitted, not whatever the widgets say when it finishes.
        product_name = product.name
        save_scene = bool(kwargs["save_scene"])
        self._submit_button.setEnabled(False)
        worker = _SubmitWorker(self.session, product_name, self._model, **kwargs)
        worker.done.connect(lambda job: self._on_submit_success(job, product_name, save_scene))
        worker.failed.connect(self._on_submit_error)
        worker.finished.connect(lambda: self._submit_button.setEnabled(True))
        self._worker = worker
        worker.start()

    def submit_and_wait(self) -> None:
        """Test/QA hook: runs the submit body synchronously, no worker thread."""
        product = self._ready_product()
        if product is None or self._model is None:
            return
        kwargs = self._submit_kwargs()
        self._submit_button.setEnabled(False)
        try:
            job = submit_form(self.session, product.name, self._model, **kwargs)
        except SubmitterError as exc:
            self._on_submit_error(exc)
        else:
            self._on_submit_success(job, product.name, bool(kwargs["save_scene"]))
        finally:
            self._submit_button.setEnabled(True)

    def _on_submit_success(self, job: Job, product_name: str, save_scene: bool) -> None:
        self._clear_field_errors()
        self._clear_error()
        url = f"{self.session.server_url}/jobs/{job.id}"
        QtWidgets.QMessageBox.information(self, "Job submitted", f"Submitted job {job.id}\n{url}")

        host = self.adapter.host_name if self.adapter is not None else ""
        self.session.settings.set(f"last_product.{host}", product_name)
        if self.adapter is not None:
            self.session.settings.set("save_before_submit", save_scene)

    def _on_submit_error(self, exc: SubmitterError) -> None:
        if isinstance(exc, FormInvalidError):
            self._apply_field_errors(exc.errors)
        self._show_error(exc.user_message)

    def _apply_field_errors(self, errors: Mapping[str, str]) -> None:
        self._clear_field_errors()
        if self._form_root is None:
            return
        for name, message in errors.items():
            editor = self._form_root.findChild(QtWidgets.QWidget, f"field_{name}")
            if editor is None:
                continue
            editor.setToolTip(message)
            editor.setStyleSheet(_ERROR_STYLE)

    def _clear_field_errors(self) -> None:
        if self._form_root is None or self._model is None:
            return
        for f in self._model.fields:
            editor = self._form_root.findChild(QtWidgets.QWidget, f"field_{f.parameter.name}")
            if editor is None:
                continue
            editor.setStyleSheet("")
            editor.setToolTip("Pre-filled from the scene" if f.prefilled else "")

    # -- error banner -------------------------------------------------------

    def _show_error(self, message: str) -> None:
        self._error_label.setText(message)
        self._error_label.show()
        self._reload_button.show()

    def _clear_error(self) -> None:
        self._error_label.clear()
        self._error_label.hide()
        self._reload_button.hide()

    # -- lifecycle ------------------------------------------------------------

    def closeEvent(self, arg__1: QtGui.QCloseEvent) -> None:
        """Never destroy a running submit thread: wait briefly, else block close."""
        worker = self._worker
        if worker is not None and worker.isRunning() and not worker.wait(_CLOSE_WAIT_MS):
            self._show_error("A submit is still in progress — please wait, then close.")
            arg__1.ignore()
            return
        super().closeEvent(arg__1)


_dialog_ref: SubmitDialog | None = None


def open_for_adapter(
    adapter: HostAdapter,
    server_url: str | None = None,
    parent: QtWidgets.QWidget | None = None,
) -> SubmitDialog:
    """Build a session, show the dialog non-modally, and return it.

    Keeps a module-level reference so the dialog stays alive after this call
    returns — hosts call this from a menu item and hold nothing else onto it.
    """
    require_qt()
    global _dialog_ref
    session = SubmitterSession(server_url=server_url)
    dialog = SubmitDialog(session, adapter=adapter, parent=parent)
    dialog.show()
    _dialog_ref = dialog
    return dialog


def main() -> int:
    """Console-script entry point (``sqi-submit``): standalone dialog, no DCC."""
    parser = argparse.ArgumentParser(prog="sqi-submit", description="Submit a job to sqi.")
    parser.add_argument("--server", default=None, help="sqi server URL (default: configured)")
    args = parser.parse_args()
    require_qt()

    app = QtWidgets.QApplication.instance() or QtWidgets.QApplication([])
    dialog = SubmitDialog(SubmitterSession(server_url=args.server))
    dialog.show()
    run = getattr(app, "exec", None) or app.exec_
    return int(run())
