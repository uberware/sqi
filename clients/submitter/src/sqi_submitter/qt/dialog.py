# SPDX-License-Identifier: AGPL-3.0-or-later
"""Generic Qt submit dialog: product → targets → params → farm/queue → submit."""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass

from sqi_client.models import Farm, Product, ProductParameter, Queue
from sqi_submitter.core import (
    FormInvalidError,
    FormModel,
    HostAdapter,
    RenderTarget,
    SubmitterError,
    SubmitterSession,
    TargetResult,
    apply_edits,
    group_products,
    normalize_name,
    per_target_models,
    prefill,
    submit_form,
    submit_targets,
    target_parameters,
)
from sqi_submitter.core.handoff import ContextAdapter
from sqi_submitter.core.joboptions import JobOptions
from sqi_submitter.qt._compat import QtCore, QtGui, QtWidgets, require_qt
from sqi_submitter.qt.widgets import PREFILL_TIP, build_form

__all__ = ["SubmitDialog", "open_for_adapter"]

_SUGGESTED_LABEL = "Suggested —"
_ALL_PRODUCTS_LABEL = "All —"
_ERROR_STYLE = "border: 1px solid red;"
_STALE_MODEL_MESSAGE = "Product parameters failed to load — reload and try again."
_EMPTY_CATALOG_MESSAGE = (
    "No products are available on this server. Ask your admin to install a "
    "preset from the preset library (Admin → Preset Library), then Reload."
)
_PER_TARGET_MODE = "One job per ticked target"
_WHOLE_SCENE_MODE = "One job for the whole scene, as configured"
_TICK_HINT = "Tick at least one target."
_FROM_EACH_TARGET_TIP = "From each target"
_LOCKED_TIP = "Kept per target (e.g. a movie renders as one task)"
_CLOSE_WAIT_MS = 10_000

_SubmitCall = Callable[[], list[TargetResult]]


class _SubmitWorker(QtCore.QThread):
    """Runs one prepared submit off the GUI thread; reports back via signals.

    ``done`` carries the ``list[TargetResult]``; ``failed`` carries a
    :class:`SubmitterError` the call raised (the whole-scene path only, so the
    single job keeps its banner and highlighted fields).
    """

    done = QtCore.Signal(object)
    failed = QtCore.Signal(object)

    def __init__(self, call: _SubmitCall) -> None:
        super().__init__()
        self._call = call

    def run(self) -> None:
        try:
            results = _run_submit(self._call)
        except SubmitterError as exc:
            self.failed.emit(exc)
        else:
            self.done.emit(results)


def _run_submit(call: _SubmitCall) -> list[TargetResult]:
    """Run ``call``, turning any exception into a :class:`SubmitterError`.

    A host's ``save_scene`` (Maya's ``cmds.file``, say) can raise anything; on the
    worker thread an unhandled exception just ends the thread and the artist sees
    nothing, so it is reported through the banner like any other failure.
    """
    try:
        return call()
    except SubmitterError:
        raise
    except Exception as exc:
        raise SubmitterError(str(exc) or type(exc).__name__) from exc


@dataclass(frozen=True)
class _Submission:
    """Everything one submit needs, captured on the GUI thread at submit-start."""

    call: _SubmitCall
    product_name: str
    save_scene: bool
    # (settings key, ticked names) to remember; None for a whole-scene product.
    remember_ticks: tuple[str, list[str]] | None


class SubmitDialog(QtWidgets.QDialog):
    """Product → targets → parameters → farm/queue → submit, one dialog."""

    def __init__(
        self,
        session: SubmitterSession,
        adapter: HostAdapter | None = None,
        parent: QtWidgets.QWidget | None = None,
        initial_error: str | None = None,
    ) -> None:
        require_qt()
        super().__init__(parent)
        self.session = session
        self.adapter = adapter

        self._products: list[Product] = []
        self._targets: list[RenderTarget] = []
        self._parameters: list[ProductParameter] = []
        self._target_params: frozenset[str] = frozenset()
        self._model: FormModel | None = None
        self._model_product: str | None = None
        # The first ticked target when the form was last built (None: none ticked).
        self._displayed_target: RenderTarget | None = None
        self._form_root: QtWidgets.QWidget | None = None
        self._worker: _SubmitWorker | None = None
        self._submitting = False
        # Fields the artist changed, by parameter name. Each job takes these
        # over its target's own pre-fill (except target parameters and keys a
        # target locks); reset when the product changes.
        self.edited: dict[str, str] = {}

        self.setWindowTitle("Submit to sqi")
        self._build_ui()
        self._wire_signals()
        self._load()
        if initial_error:
            self._show_error(initial_error)

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

        self._mode_label = QtWidgets.QLabel(self)
        self._mode_label.setObjectName("modeLabel")

        self._target_list = QtWidgets.QListWidget(self)
        self._target_list.setObjectName("targetList")
        self._target_list.hide()

        self._target_hint = QtWidgets.QLabel(_TICK_HINT, self)
        self._target_hint.setObjectName("targetHint")
        self._target_hint.hide()

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
        self._save_scene_check.setVisible(self._saves_scene_in_dialog)
        if self.adapter is not None:
            self._save_scene_check.setChecked(
                bool(self.session.settings.get("save_before_submit", True))
            )

        self._advanced_group = self._build_advanced_group()

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
        layout.addWidget(self._mode_label)
        layout.addWidget(self._target_list)
        layout.addWidget(self._target_hint)
        layout.addWidget(self._form_scroll, 1)
        layout.addWidget(QtWidgets.QLabel("Job name", self))
        layout.addWidget(self._job_name_edit)
        layout.addLayout(farm_row)
        layout.addWidget(self._save_scene_check)
        layout.addWidget(self._advanced_group)
        layout.addLayout(button_row)

    def _build_advanced_group(self) -> QtWidgets.QGroupBox:
        """Collapsible, initially-unchecked per-job override editors.

        Each spin box uses ``0`` as the "unset" sentinel (real minimums, e.g.
        priority, start at 1 server-side) so a field left untouched maps to
        ``None`` and is omitted from the submit payload — see
        :meth:`_job_options`.
        """
        group = QtWidgets.QGroupBox("Advanced (job overrides)", self)
        group.setObjectName("advancedGroup")
        group.setCheckable(True)
        group.setChecked(False)

        self._owner_label = QtWidgets.QLabel("Owner", group)
        self._owner_edit = QtWidgets.QLineEdit(group)
        self._owner_edit.setObjectName("ownerEdit")

        self._priority_spin = QtWidgets.QSpinBox(group)
        self._priority_spin.setObjectName("prioritySpin")
        self._priority_spin.setRange(0, 100)
        self._priority_spin.setSpecialValueText("Inherit")

        self._project_edit = QtWidgets.QLineEdit(group)
        self._project_edit.setObjectName("projectEdit")

        self._max_attempts_spin = QtWidgets.QSpinBox(group)
        self._max_attempts_spin.setObjectName("maxAttemptsSpin")
        self._max_attempts_spin.setRange(0, 1000)
        self._max_attempts_spin.setSpecialValueText("Inherit")

        self._retry_delay_spin = QtWidgets.QSpinBox(group)
        self._retry_delay_spin.setObjectName("retryDelaySpin")
        self._retry_delay_spin.setRange(0, 86_400)
        self._retry_delay_spin.setSpecialValueText("Inherit")

        self._failure_limit_spin = QtWidgets.QSpinBox(group)
        self._failure_limit_spin.setObjectName("failureLimitSpin")
        self._failure_limit_spin.setRange(0, 1000)
        self._failure_limit_spin.setSpecialValueText("Inherit")

        form = QtWidgets.QFormLayout(group)
        form.addRow(self._owner_label, self._owner_edit)
        form.addRow("Priority", self._priority_spin)
        form.addRow("Project", self._project_edit)
        form.addRow("Max attempts", self._max_attempts_spin)
        form.addRow("Retry delay (s)", self._retry_delay_spin)
        form.addRow("Failure limit", self._failure_limit_spin)

        # The Owner override is only meaningful (and only accepted by the
        # server) for a credential holding jobs.submit_as; hide it otherwise
        # so an artist never fills it in only to be 403'd at submit time.
        # session.may_submit_as fails OPEN when unknown (see its docstring) —
        # this is a UI affordance, the server is the real enforcement point.
        owner_visible = self.session.may_submit_as
        self._owner_label.setVisible(owner_visible)
        self._owner_edit.setVisible(owner_visible)
        return group

    def _wire_signals(self) -> None:
        self._product_combo.currentIndexChanged.connect(self._on_product_changed)
        self._target_list.itemChanged.connect(self._on_ticks_changed)
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
        host_token = self._host_token()
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

    @property
    def _saves_scene_in_dialog(self) -> bool:
        """Whether the dialog offers (and remembers) "Save scene before submit".

        A handoff host saves the scene before launching sqi-submit, so the box
        would offer something the dialog cannot do.
        """
        return self.adapter is not None and not isinstance(self.adapter, ContextAdapter)

    def _host_token(self) -> str:
        return self.adapter.host_name if self.adapter is not None else ""

    def _ticks_key(self) -> str:
        scene = self.adapter.scene_context().scene_path if self.adapter is not None else None
        return f"last_targets.{self._host_token()}.{scene or ''}"

    def _remembered_ticks(self, names: Sequence[str]) -> set[str]:
        """Remembered ticked names that still exist; else just the first target."""
        stored = self.session.settings.get(self._ticks_key())
        kept = {n for n in stored if n in names} if isinstance(stored, list) else set()
        if not kept and names:
            kept = {names[0]}
        return kept

    def _populate_targets(self) -> None:
        self._targets = self.adapter.render_targets() if self.adapter is not None else []
        ticked = self._remembered_ticks([t.name for t in self._targets])
        widget = self._target_list
        widget.blockSignals(True)
        widget.clear()
        # Row i is self._targets[i]: _ticked_targets relies on that order.
        for target in self._targets:
            item = QtWidgets.QListWidgetItem(target.name, widget)
            item.setFlags(QtCore.Qt.ItemFlag.ItemIsEnabled | QtCore.Qt.ItemFlag.ItemIsUserCheckable)
            item.setCheckState(
                QtCore.Qt.CheckState.Checked
                if target.name in ticked
                else QtCore.Qt.CheckState.Unchecked
            )
        widget.blockSignals(False)

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

    # -- targets ----------------------------------------------------------

    @property
    def _per_target(self) -> bool:
        return bool(self._target_params)

    def _ticked_targets(self) -> list[RenderTarget]:
        """Ticked targets in list order (the first one drives the form)."""
        widget = self._target_list
        ticked: list[RenderTarget] = []
        for i, target in enumerate(self._targets):
            item = widget.item(i)
            if item is not None and item.checkState() == QtCore.Qt.CheckState.Checked:
                ticked.append(target)
        return ticked

    def _on_ticks_changed(self, _item: object) -> None:
        ticked = self._ticked_targets() if self._per_target else []
        # An edit to a target parameter belonged to the old selection: with
        # several ticked the field is "from each target", so drop it.
        dropped = False
        for name in self._target_params:
            if self.edited.pop(name, None) is not None:
                dropped = True
        product = self._current_product()
        first = ticked[0] if ticked else None
        if product is None or self._model_product != product.name:
            self._update_submit_state(ticked)
        elif dropped or first is not self._displayed_target:
            self._rebuild_model(product.name, ticked)
        else:
            # The form shows the first ticked target's model, which ticking or
            # unticking a later target cannot change: restyle it, don't rebuild.
            self._clear_field_errors(ticked)
            self._update_submit_state(ticked)

    # -- form -----------------------------------------------------------

    def _current_product(self) -> Product | None:
        data = self._product_combo.currentData()
        return data if isinstance(data, Product) else None

    def _on_product_changed(self, _index: int) -> None:
        self._rebuild_form()

    def _hidden_field_names(self, model: FormModel) -> frozenset[str]:
        """Scene-path fields are host-managed when a DCC adapter is present."""
        if self.adapter is None:
            return frozenset()
        return frozenset(f.parameter.name for f in model.fields if f.is_scene_path)

    def _rebuild_form(self) -> None:
        """Fetch the selected product's parameters, then rebuild the form."""
        product = self._current_product()
        if product is None:
            return
        self.edited = {}
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

        self._parameters = parameters
        self._target_params = target_parameters(parameters, self._targets)
        self._target_list.setVisible(self._per_target)
        self._mode_label.setText(_PER_TARGET_MODE if self._per_target else _WHOLE_SCENE_MODE)
        self._job_name_edit.setText(product.title or product.name)
        self._rebuild_model(product.name)

    def _display_model(self, ticked: Sequence[RenderTarget]) -> FormModel:
        """The model the form shows: the first ticked target's pre-fill + edits."""
        if self.adapter is not None and ticked:
            return per_target_models(
                self._parameters,
                self.adapter.scene_context(),
                ticked[:1],
                self.edited,
                target_params=self._target_params,
            )[0][1]
        model = FormModel.from_parameters(self._parameters)
        if self.adapter is not None:
            model.apply_prefill(prefill(self._parameters, self.adapter.scene_context()))
        apply_edits(model, self.edited)
        return model

    def _rebuild_model(
        self, product_name: str, ticked: Sequence[RenderTarget] | None = None
    ) -> None:
        """Rebuild the model and editors from ``product_name``'s cached parameters (no fetch)."""
        if ticked is None:
            ticked = self._ticked_targets() if self._per_target else []
        model = self._display_model(ticked)
        self._model = model
        self._model_product = product_name
        self._displayed_target = ticked[0] if ticked else None

        old = self._form_scroll.takeWidget()
        self._form_scroll.setWidget(
            build_form(
                model,
                hidden_names=self._hidden_field_names(model),
                on_change=self._on_field_edited,
            )
        )
        self._form_root = self._form_scroll.widget()
        if old is not None:
            old.deleteLater()
        self._apply_target_states(ticked)
        self._update_submit_state(ticked)

    def _on_field_edited(self, name: str, value: str) -> None:
        self.edited[name] = value

    def _field_editor(self, name: str) -> QtWidgets.QWidget | None:
        if self._form_root is None:
            return None
        return self._form_root.findChild(QtWidgets.QWidget, f"field_{name}")

    def _apply_target_states(self, ticked: Sequence[RenderTarget] | None = None) -> None:
        """Lock target parameters while several are ticked; flag locked keys.

        Sets every field's tooltip, as a rebuild would, so the form is right
        after the ticks change without one.
        """
        if self._form_root is None or self._model is None or not self._per_target:
            return
        if ticked is None:
            ticked = self._ticked_targets()
        locked = {normalize_name(k) for t in ticked for k in t.locked}
        several = len(ticked) > 1
        for f in self._model.fields:
            name = f.parameter.name
            editor = self._field_editor(name)
            if editor is None:
                continue
            is_target_param = name in self._target_params
            if is_target_param:
                row = self._form_root.findChild(QtWidgets.QWidget, f"row_{name}")
                (row or editor).setEnabled(not several)
            if several and is_target_param:
                editor.setToolTip(_FROM_EACH_TARGET_TIP)
            elif normalize_name(name) in locked:
                editor.setToolTip(_LOCKED_TIP)
            else:
                editor.setToolTip(PREFILL_TIP if f.prefilled else "")

    def _update_submit_state(self, ticked: Sequence[RenderTarget] | None = None) -> None:
        """Submit needs a current model, no submit running, and (per-target) a tick."""
        product = self._current_product()
        ready = (
            product is not None and self._model is not None and self._model_product == product.name
        )
        if self._per_target and ticked is None:
            ticked = self._ticked_targets()
        needs_tick = self._per_target and not ticked
        self._target_hint.setVisible(needs_tick)
        self._submit_button.setEnabled(ready and not needs_tick and not self._submitting)

    # -- submit -----------------------------------------------------------

    def _job_options(self) -> JobOptions | None:
        """Advanced-group editors -> ``JobOptions``, or ``None`` when inactive.

        The group is opt-in: collapsed (unchecked) means "no overrides" even
        if widgets hold stale values from a prior expand. When checked, a
        spin box left at its ``0`` sentinel or a blank line edit maps to
        ``None`` (inherit) rather than an explicit override.
        """
        if not self._advanced_group.isChecked():
            return None
        opts = JobOptions(
            owner=(self._owner_edit.text() or None) if self.session.may_submit_as else None,
            priority=self._priority_spin.value() or None,
            project=self._project_edit.text() or None,
            max_attempts=self._max_attempts_spin.value() or None,
            retry_delay_seconds=self._retry_delay_spin.value() or None,
            failure_limit=self._failure_limit_spin.value() or None,
        )
        return None if opts.is_empty() else opts

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

    def _submission_models(
        self, ticked: Sequence[RenderTarget]
    ) -> list[tuple[RenderTarget, FormModel]]:
        assert self.adapter is not None  # per-target implies targets, so an adapter
        # The all-targets set, never one derived from the ticked subset: an
        # edit must not be honoured or dropped depending on what else is ticked.
        models = per_target_models(
            self._parameters,
            self.adapter.scene_context(),
            ticked,
            self.edited,
            target_params=self._target_params,
        )
        if len(models) == 1:
            # One ticked target behaves like a single-target pre-fill: an edit
            # to its target parameter is the artist's to make (per_target_models
            # always keeps a target parameter the target's own).
            _, model = models[0]
            for name in self._target_params:
                if name in self.edited:
                    model.set_value(name, self.edited[name])
        return models

    def _prepare_submit(self) -> _Submission | None:
        """Capture the whole submit at submit-start, or ``None`` if not ready.

        The async submit must use (and persist) what was on screen when the
        artist pressed Submit, not whatever the widgets say when it finishes.
        """
        product = self._ready_product()
        model = self._model
        if product is None or model is None:
            return None
        ticked = self._ticked_targets() if self._per_target else []
        if self._per_target and not ticked:
            self._update_submit_state()
            return None

        session = self.session
        product_name = product.name
        farm = self._farm_combo.currentData()
        queue = self._queue_combo.currentData()
        farm_id = farm.id if isinstance(farm, Farm) else ""
        queue_id = queue.id if isinstance(queue, Queue) else ""
        job_name = self._job_name_edit.text()
        adapter = self.adapter
        save_scene = self._save_scene_check.isChecked() if adapter is not None else True
        job_options = self._job_options()

        if self._per_target:
            models = self._submission_models(ticked)
            prefix = job_name or product.title or product.name

            def call() -> list[TargetResult]:
                return submit_targets(
                    session,
                    product_name,
                    models,
                    farm_id=farm_id,
                    queue_id=queue_id,
                    job_name_prefix=prefix,
                    adapter=adapter,
                    save_scene=save_scene,
                    job_options=job_options,
                )

            remember = (self._ticks_key(), [t.name for t in ticked])
            return _Submission(call, product_name, save_scene, remember)

        scene = RenderTarget(name=job_name or product.title or product.name, kind="scene")

        def call_whole_scene() -> list[TargetResult]:
            job = submit_form(
                session,
                product_name,
                model,
                farm_id=farm_id,
                queue_id=queue_id,
                job_name=job_name or None,
                adapter=adapter,
                save_scene=save_scene,
                job_options=job_options,
            )
            return [TargetResult(target=scene, job=job, error=None)]

        return _Submission(call_whole_scene, product_name, save_scene, None)

    def _on_submit(self) -> None:
        submission = self._prepare_submit()
        if submission is None:
            return
        self._set_submitting(True)
        worker = _SubmitWorker(submission.call)
        worker.done.connect(lambda results: self._on_submit_done(results, submission))
        worker.failed.connect(self._on_submit_error)
        worker.finished.connect(lambda: self._set_submitting(False))
        self._worker = worker
        worker.start()

    def submit_and_wait(self) -> None:
        """Test/QA hook: runs the submit body synchronously, no worker thread."""
        submission = self._prepare_submit()
        if submission is None:
            return
        self._set_submitting(True)
        try:
            results = _run_submit(submission.call)
        except SubmitterError as exc:
            self._on_submit_error(exc)
        else:
            self._on_submit_done(results, submission)
        finally:
            self._set_submitting(False)

    def _set_submitting(self, submitting: bool) -> None:
        self._submitting = submitting
        self._update_submit_state()

    def _result_text(self, results: Sequence[TargetResult]) -> str:
        lines: list[str] = []
        for result in results:
            if result.job is not None:
                lines.append(f"{result.target.name}: job {result.job.id}")
                lines.append(f"    {self.session.server_url}/jobs/{result.job.id}")
            else:
                lines.append(f"{result.target.name}: FAILED — {result.error}")
        return "\n".join(lines)

    def _on_submit_done(self, results: list[TargetResult], submission: _Submission) -> None:
        self._clear_field_errors()
        self._clear_error()
        submitted = [r for r in results if r.job is not None]
        text = self._result_text(results)
        if len(submitted) == len(results):
            title = "Job submitted" if len(results) == 1 else "Jobs submitted"
            QtWidgets.QMessageBox.information(self, title, text)
        else:
            title = "Some jobs failed" if submitted else "No jobs submitted"
            QtWidgets.QMessageBox.warning(self, title, text)
        if not submitted:
            return

        self.session.settings.set(f"last_product.{self._host_token()}", submission.product_name)
        if self._saves_scene_in_dialog:
            self.session.settings.set("save_before_submit", submission.save_scene)
        if submission.remember_ticks is not None:
            self.session.settings.set(*submission.remember_ticks)

    def _on_submit_error(self, exc: SubmitterError) -> None:
        if isinstance(exc, FormInvalidError):
            self._apply_field_errors(exc.errors)
        self._show_error(exc.user_message)

    def _apply_field_errors(self, errors: Mapping[str, str]) -> None:
        self._clear_field_errors()
        if self._form_root is None:
            return
        for name, message in errors.items():
            editor = self._field_editor(name)
            if editor is None:
                continue
            editor.setToolTip(message)
            editor.setStyleSheet(_ERROR_STYLE)

    def _clear_field_errors(self, ticked: Sequence[RenderTarget] | None = None) -> None:
        if self._form_root is None or self._model is None:
            return
        for f in self._model.fields:
            editor = self._field_editor(f.parameter.name)
            if editor is None:
                continue
            editor.setStyleSheet("")
            editor.setToolTip(PREFILL_TIP if f.prefilled else "")
        self._apply_target_states(ticked)

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
    dialog.raise_()
    dialog.activateWindow()
    _dialog_ref = dialog
    return dialog
