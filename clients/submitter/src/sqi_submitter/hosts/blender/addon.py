# SPDX-License-Identifier: AGPL-3.0-or-later
"""Native Blender add-on: N-panel submit UI (bl_info + register()/unregister()).

Every ``bpy`` touch — including ``import bpy`` itself and the operator/panel
*class definitions*, which subclass ``bpy.types.Operator``/``bpy.types.Panel``
and therefore cannot exist without ``bpy`` installed — lives inside a function
body. ``_make_classes()`` builds those classes lazily and is only ever called
from :func:`register`, so importing this module never requires ``bpy`` (the
test suite imports :func:`field_layout` directly with no ``bpy`` on
``sys.path``). The per-field scene properties are a second lazy construct:
each product's parameters differ, so the ``PropertyGroup`` that backs them is
rebuilt with ``type()`` and re-registered every time :class:`SQI_OT_refresh`
loads a new model — plain class syntax would only capture the fields known at
``register()`` time.
"""

from __future__ import annotations

import queue
import threading
from collections.abc import Callable
from typing import Any

from sqi_submitter.core import (
    FormField,
    FormInvalidError,
    FormModel,
    HostAdapter,
    SubmitterError,
    SubmitterSession,
    prefill,
    submit_form,
)

bl_info = {
    "name": "sqi Submitter",
    "author": "Uberware Inc.",
    "version": (0, 1, 0),
    "blender": (3, 0, 0),
    "location": "View3D > Sidebar > sqi",
    "description": "Submit render jobs to a sqi farm",
    "category": "Render",
}

__all__ = ["bl_info", "field_layout", "register", "unregister"]

# Module-level UI/session state. A Blender add-on module is a singleton (one
# import per running Blender session), so this plays the role an instance
# attribute would in a Qt dialog.
_state: dict[str, Any] = {
    "session": None,  # SubmitterSession | None
    "products": [],  # list[Product]
    "targets": [],  # list[RenderTarget]
    "model": None,  # FormModel | None
    "adapter": None,  # HostAdapter | None
    # EnumProperty items callbacks must keep a Python reference to the last
    # returned sequence (documented Blender pitfall: the returned strings can
    # be garbage-collected while Blender still holds pointers into them).
    "_product_items": [],  # list[tuple[str, str, str]]
    "_target_items": [],  # list[tuple[str, str, str]]
    "field_errors": {},  # dict[str, str]
}

_classes: list[Any] = []  # registered bpy.types classes, in registration order
_field_group_cls: list[Any] = []  # current per-field PropertyGroup, [cls] or []


def _visible_fields(model: FormModel) -> list[FormField]:
    """Fields the panel draws: not hidden, and not the host-managed scene path."""
    return [f for f in model.fields if not f.hidden and not f.is_scene_path]


def field_layout(model: FormModel) -> list[tuple[str, str, str]]:
    """(name, label, widget) rows for every drawable field, in order.

    Pure and bpy-free so it is directly unit-testable; the panel's draw loop
    calls this to decide which scene property + widget to draw per field.
    """
    return [(f.parameter.name, f.label, f.widget) for f in _visible_fields(model)]


def field_rows(
    model: FormModel, errors: dict[str, str] | None = None
) -> list[tuple[str, str, str, str | None]]:
    """(name, label, widget, error) for every drawable field, in order."""
    errs = errors or {}
    return [
        (f.parameter.name, f.label, f.widget, errs.get(f.parameter.name))
        for f in _visible_fields(model)
    ]


def _prop_name(field_name: str) -> str:
    return f"sqi_field_{field_name}"


def _run_async(fn: Callable[[], Any], on_done: Callable[[Any, BaseException | None], None]) -> None:
    """Run ``fn`` on a worker thread; deliver its result on Blender's main thread.

    The ``bpy`` API is not thread-safe, so the worker thread only computes a
    plain-Python result and hands it back through a queue; a
    ``bpy.app.timers`` poller (invoked on the main thread) drains the queue
    and calls ``on_done``.
    """
    import bpy

    result_queue: queue.Queue[tuple[Any, BaseException | None]] = queue.Queue()

    def _worker() -> None:
        try:
            result = fn()
        except Exception as exc:  # forwarded to on_done, not swallowed
            result_queue.put((None, exc))
        else:
            result_queue.put((result, None))

    def _poll() -> float | None:
        try:
            result, error = result_queue.get_nowait()
        except queue.Empty:
            return 0.2  # not ready yet; reschedule
        on_done(result, error)
        return None  # unregister this timer

    threading.Thread(target=_worker, daemon=True).start()
    bpy.app.timers.register(_poll)


def _widget_property(bpy_props: Any, field: FormField) -> Any:
    """Map a form field to a Blender scene-property, honoring the value type.

    Mirrors ``qt/widgets.py``: SPIN_BOX means Int for INT parameters and Float
    for FLOAT parameters; CHECK_BOX is a Bool; everything else edits a string.
    """
    widget = field.widget
    if widget == "SPIN_BOX" and field.parameter.type == "INT":
        return bpy_props.IntProperty(name="")
    if widget == "SPIN_BOX":
        return bpy_props.FloatProperty(name="")
    if widget == "CHECK_BOX":
        return bpy_props.BoolProperty(name="")
    return bpy_props.StringProperty(name="")


def _build_field_property_group(model: FormModel | None) -> Any:
    """A fresh ``PropertyGroup`` subclass with one property per form field."""
    import bpy

    annotations: dict[str, Any] = {}
    if model is not None:
        for field in _visible_fields(model):
            annotations[_prop_name(field.parameter.name)] = _widget_property(bpy.props, field)
    namespace: dict[str, Any] = {"__annotations__": annotations}
    return type("SQI_FieldPropertyGroup", (bpy.types.PropertyGroup,), namespace)


def _apply_model(model: FormModel | None, adapter: HostAdapter | None) -> None:
    """Swap in a freshly-fetched model: rebuild + re-register the field group."""
    import bpy

    _state["model"] = model
    _state["adapter"] = adapter

    if hasattr(bpy.types.Scene, "sqi_fields"):
        del bpy.types.Scene.sqi_fields
    if _field_group_cls:
        bpy.utils.unregister_class(_field_group_cls.pop())

    new_cls = _build_field_property_group(model)
    bpy.utils.register_class(new_cls)
    _field_group_cls.append(new_cls)
    bpy.types.Scene.sqi_fields = bpy.props.PointerProperty(type=new_cls)

    if model is not None:
        scene = getattr(bpy.context, "scene", None)
        fields = getattr(scene, "sqi_fields", None) if scene is not None else None
        if fields is not None:
            for field in _visible_fields(model):
                setattr(fields, _prop_name(field.parameter.name), _scene_prop_value(field))


def _bool_field_value(checked: bool, field: FormField) -> str:
    """Map a CHECK_BOX bool through the same (off, on) rule as qt/widgets.py.

    ``allowed_values[0]`` is the unchecked value, ``allowed_values[1]`` is
    checked (mirrors the web form's ``const [off, on] = allowed_values`` in
    ``web/src/components/ProductParamField.tsx``). Never ``str(bool)`` —
    that would write Python's capitalized "True"/"False", a third,
    incompatible convention.
    """
    allowed = field.parameter.allowed_values or []
    off, on = (allowed[0], allowed[1]) if len(allowed) >= 2 else ("false", "true")
    return on if checked else off


def _scene_prop_value(field: FormField) -> str | int | float | bool:
    """Coerce a field's model value to the scene property's Python type.

    Inverse of _copy_scene_values_into_model / mirror of _widget_property:
    SPIN_BOX(INT)->int, SPIN_BOX(FLOAT)->float, CHECK_BOX->bool, else str.
    """
    value = field.value or ""
    widget = field.widget
    if widget == "SPIN_BOX" and field.parameter.type == "INT":
        try:
            return int(float(value))
        except ValueError:
            return 0
    if widget == "SPIN_BOX":
        try:
            return float(value)
        except ValueError:
            return 0.0
    if widget == "CHECK_BOX":
        allowed = field.parameter.allowed_values or []
        on = allowed[1] if len(allowed) >= 2 else "true"
        return value == on
    return value


def _copy_scene_values_into_model(context: Any, model: FormModel) -> None:
    """Read the artist's edits out of the scene properties before submit."""
    fields = getattr(context.scene, "sqi_fields", None)
    if fields is None:
        return
    for f in _visible_fields(model):
        value = getattr(fields, _prop_name(f.parameter.name), None)
        if value is None:
            continue
        if f.widget == "CHECK_BOX":
            model.set_value(f.parameter.name, _bool_field_value(bool(value), f))
        else:
            model.set_value(f.parameter.name, str(value))


def _product_enum_items(_self: Any, _context: Any) -> list[tuple[str, str, str]]:
    products = _state["products"]
    if not products:
        items = [("NONE", "(refresh to load products)", "")]
    else:
        items = [(str(i), p.title or p.name, p.description) for i, p in enumerate(products)]
    _state["_product_items"] = items  # keep a reference: Blender only borrows it
    return items


def _target_enum_items(_self: Any, _context: Any) -> list[tuple[str, str, str]]:
    items = [("SCENE", "Scene settings", "")]
    items += [(str(i), t.name, "") for i, t in enumerate(_state["targets"])]
    _state["_target_items"] = items  # keep a reference: Blender only borrows it
    return items


def _selected_product(props: Any) -> Any | None:
    products = _state["products"]
    if not products:
        return None
    try:
        index = int(props.product)
    except ValueError:
        index = 0
    return products[index] if 0 <= index < len(products) else products[0]


def _selected_target(props: Any) -> Any | None:
    targets = _state["targets"]
    if props.target == "SCENE" or not targets:
        return None
    try:
        index = int(props.target)
    except ValueError:
        return None
    return targets[index] if 0 <= index < len(targets) else None


def _fetch_products_and_model(
    selected_product_name: str | None, target: Any | None
) -> tuple[list[Any], Any]:
    """Runs off the main thread: session/products/parameters fetch only."""
    from sqi_submitter.hosts.blender.adapter import BlenderAdapter

    session = _state["session"] or SubmitterSession()
    _state["session"] = session
    adapter = BlenderAdapter()
    products = session.products()

    model = None
    if products:
        product = next((p for p in products if p.name == selected_product_name), products[0])
        parameters = session.parameters(product.name)
        model = FormModel.from_parameters(parameters)
        model.apply_prefill(prefill(parameters, adapter.scene_context(), target))
    return products, (model, adapter)


def _make_classes() -> list[Any]:
    """Build the operator/panel classes; called once, from :func:`register`."""
    import bpy

    class SQI_OT_refresh(bpy.types.Operator):  # type: ignore[misc]
        bl_idname = "sqi.refresh"
        bl_label = "Refresh Products"
        bl_description = "Fetch products and parameters from the sqi server"

        def execute(self, context: Any) -> set[str]:
            props = context.scene.sqi_submitter
            current = _selected_product(props)
            selected_name = current.name if current is not None else None
            target = _selected_target(props)

            def _on_done(result: Any, error: BaseException | None) -> None:
                if error is not None:
                    message = (
                        error.user_message if isinstance(error, SubmitterError) else str(error)
                    )
                    self.report({"ERROR"}, message)
                    return
                products, (model, adapter) = result
                _state["products"] = products
                _state["targets"] = adapter.render_targets()
                _apply_model(model, adapter)
                _state["field_errors"] = {}
                self.report({"INFO"}, f"Loaded {len(products)} product(s)")

            _run_async(lambda: _fetch_products_and_model(selected_name, target), _on_done)
            return {"FINISHED"}

    class SQI_OT_submit(bpy.types.Operator):  # type: ignore[misc]
        bl_idname = "sqi.submit"
        bl_label = "Submit"
        bl_description = "Submit the current form to the sqi farm"

        def execute(self, context: Any) -> set[str]:
            model = _state["model"]
            product = _selected_product(context.scene.sqi_submitter)
            if model is None or product is None:
                self.report({"ERROR"}, "Refresh products before submitting.")
                return {"CANCELLED"}

            _copy_scene_values_into_model(context, model)
            props = context.scene.sqi_submitter
            session = _state["session"] or SubmitterSession()
            _state["session"] = session
            try:
                job = submit_form(
                    session,
                    product.name,
                    model,
                    farm_id=props.farm_id,
                    queue_id=props.queue_id,
                    job_name=props.job_name or None,
                    adapter=_state["adapter"],
                    save_scene=props.save_before_submit,
                )
            except FormInvalidError as exc:
                _state["field_errors"] = dict(exc.errors)
                self.report({"ERROR"}, exc.user_message)
                return {"CANCELLED"}
            except SubmitterError as exc:
                self.report({"ERROR"}, exc.user_message)
                return {"CANCELLED"}
            _state["field_errors"] = {}
            self.report({"INFO"}, f"Submitted job {job.id}")
            return {"FINISHED"}

    class SQI_PT_panel(bpy.types.Panel):  # type: ignore[misc]
        bl_idname = "SQI_PT_panel"
        bl_label = "sqi Submitter"
        bl_space_type = "VIEW_3D"
        bl_region_type = "UI"
        bl_category = "sqi"

        def draw(self, context: Any) -> None:
            layout = self.layout
            props = context.scene.sqi_submitter

            layout.operator(SQI_OT_refresh.bl_idname, icon="FILE_REFRESH")

            if _state["products"]:
                layout.prop(props, "product", text="Product")
                if _state["targets"]:
                    layout.prop(props, "target", text="Target")

            model = _state["model"]
            fields = getattr(context.scene, "sqi_fields", None)
            if model is not None and fields is not None:
                box = layout.box()
                errors = _state["field_errors"]
                for name, label, _widget, error in field_rows(model, errors):
                    row = box.row()
                    row.alert = error is not None
                    row.prop(fields, _prop_name(name), text=label)
                    if error is not None:
                        box.label(text=error, icon="ERROR")

            layout.prop(props, "job_name", text="Job name")
            layout.prop(props, "farm_id", text="Farm")
            layout.prop(props, "queue_id", text="Queue")
            layout.prop(props, "save_before_submit", text="Save scene before submit")
            layout.operator(SQI_OT_submit.bl_idname, icon="EXPORT")

    return [SQI_OT_refresh, SQI_OT_submit, SQI_PT_panel]


def _make_settings_class() -> Any:
    """The fixed (non-per-field) scene properties: product/target pick, job name, etc.

    Built with ``type()`` and a *runtime* ``__annotations__`` dict, never with
    class-body annotation syntax: this module has ``from __future__ import
    annotations`` active, which stringifies class-body annotations — Blender's
    ``register_class`` would then see ``"bpy.props.EnumProperty(...)"`` string
    literals instead of property definitions and register no properties at all.
    """
    import bpy

    annotations: dict[str, Any] = {
        "product": bpy.props.EnumProperty(name="Product", items=_product_enum_items),
        "target": bpy.props.EnumProperty(name="Target", items=_target_enum_items),
        "job_name": bpy.props.StringProperty(name="Job name", default=""),
        "farm_id": bpy.props.StringProperty(name="Farm", default=""),
        "queue_id": bpy.props.StringProperty(name="Queue", default=""),
        "save_before_submit": bpy.props.BoolProperty(name="Save scene before submit", default=True),
    }
    namespace: dict[str, Any] = {"__annotations__": annotations}
    return type("SQI_Settings", (bpy.types.PropertyGroup,), namespace)


def register() -> None:
    import bpy

    settings_cls = _make_settings_class()
    classes = [settings_cls, *_make_classes()]
    _classes[:] = classes
    for cls in classes:
        bpy.utils.register_class(cls)
    bpy.types.Scene.sqi_submitter = bpy.props.PointerProperty(type=settings_cls)
    _apply_model(None, None)


def unregister() -> None:
    import bpy

    if hasattr(bpy.types.Scene, "sqi_fields"):
        del bpy.types.Scene.sqi_fields
    if hasattr(bpy.types.Scene, "sqi_submitter"):
        del bpy.types.Scene.sqi_submitter
    if _field_group_cls:
        bpy.utils.unregister_class(_field_group_cls.pop())
    for cls in reversed(_classes):
        bpy.utils.unregister_class(cls)
    _classes.clear()
    _state["session"] = None
    _state["products"] = []
    _state["targets"] = []
    _state["model"] = None
    _state["adapter"] = None
    _state["field_errors"] = {}
