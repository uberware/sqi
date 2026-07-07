# SPDX-License-Identifier: AGPL-3.0-or-later
"""Blender adapter extraction tests against a fake bpy module."""

from types import SimpleNamespace
from typing import Any


def _install_fake_bpy(
    fake_host_module: Any, filepath: str = "/shows/d/shot.blend", dirty: bool = True
) -> tuple[Any, list[bool]]:
    layer_a, layer_b = SimpleNamespace(name="ViewLayer"), SimpleNamespace(name="fx")
    scene = SimpleNamespace(
        name="Scene",
        frame_start=1,
        frame_end=48,
        render=SimpleNamespace(filepath="/renders/d/", engine="CYCLES"),
        view_layers=[layer_a, layer_b],
    )
    saved = []
    bpy = fake_host_module(
        "bpy",
        data=SimpleNamespace(filepath=filepath, is_dirty=dirty, scenes=[scene]),
        context=SimpleNamespace(scene=scene),
        ops=SimpleNamespace(wm=SimpleNamespace(save_mainfile=lambda: saved.append(True))),
    )
    return bpy, saved


def test_scene_context(fake_host_module: Any) -> None:
    _install_fake_bpy(fake_host_module)
    from sqi_submitter.hosts.blender.adapter import BlenderAdapter

    ctx = BlenderAdapter().scene_context()
    assert ctx.scene_path == "/shows/d/shot.blend"
    assert ctx.frame_range == "1-48"
    assert ctx.output_path == "/renders/d/"
    assert ctx.renderer == "CYCLES"


def test_targets_are_scene_by_view_layer(fake_host_module: Any) -> None:
    _install_fake_bpy(fake_host_module)
    from sqi_submitter.hosts.blender.adapter import BlenderAdapter

    targets = BlenderAdapter().render_targets()
    assert [t.name for t in targets] == ["Scene / ViewLayer", "Scene / fx"]
    assert targets[1].extra == {"Scene": "Scene", "ViewLayer": "fx"}


def test_unsaved_blend_cannot_save(fake_host_module: Any) -> None:
    _install_fake_bpy(fake_host_module, filepath="")
    from sqi_submitter.hosts.blender.adapter import BlenderAdapter

    adapter = BlenderAdapter()
    assert adapter.scene_context().scene_path is None
    assert adapter.save_scene() is False


def test_field_layout_rows_follow_form_model() -> None:
    from sqi_client.models import ProductParameter
    from sqi_submitter.core.schema import FormModel
    from sqi_submitter.hosts.blender.addon import field_layout

    model = FormModel.from_parameters(
        [
            ProductParameter(name="Frames", type="STRING"),
            ProductParameter(name="Chunk", type="INT", default="1"),
        ]
    )
    assert field_layout(model) == [
        ("Frames", "Frames", "LINE_EDIT"),
        ("Chunk", "Chunk", "SPIN_BOX"),
    ]


def _install_fake_bpy_props(fake_host_module: Any) -> Any:
    """A fake bpy whose props factories return recording stand-in tuples."""

    def _factory(kind: str) -> Any:
        def make(**kwargs: Any) -> tuple[str, str]:
            return ("prop", kind)

        return make

    return fake_host_module(
        "bpy",
        props=SimpleNamespace(
            IntProperty=_factory("IntProperty"),
            FloatProperty=_factory("FloatProperty"),
            BoolProperty=_factory("BoolProperty"),
            StringProperty=_factory("StringProperty"),
            EnumProperty=_factory("EnumProperty"),
        ),
        types=SimpleNamespace(PropertyGroup=type("PropertyGroup", (), {})),
    )


def test_settings_class_annotations_are_runtime_properties(fake_host_module: Any) -> None:
    # Regression: `from __future__ import annotations` stringifies class-body
    # annotations, so the settings PropertyGroup must be built with a runtime
    # __annotations__ dict of real bpy.props objects — never annotation syntax.
    _install_fake_bpy_props(fake_host_module)
    from sqi_submitter.hosts.blender.addon import _make_settings_class

    cls = _make_settings_class()
    annotations = cls.__annotations__
    assert set(annotations) == {
        "product",
        "target",
        "job_name",
        "farm",
        "queue",
        "save_before_submit",
    }
    for value in annotations.values():
        assert not isinstance(value, str), "annotation was stringified — no property registered"
    assert annotations["product"] == ("prop", "EnumProperty")
    assert annotations["target"] == ("prop", "EnumProperty")
    assert annotations["job_name"] == ("prop", "StringProperty")
    assert annotations["farm"] == ("prop", "EnumProperty")
    assert annotations["queue"] == ("prop", "EnumProperty")
    assert annotations["save_before_submit"] == ("prop", "BoolProperty")


def test_copy_scene_values_maps_bool_checkbox_through_allowed_values() -> None:
    # CHECK_BOX bools must go through the same (off, on) = allowed_values
    # convention as qt/widgets.py and the web form — never str(bool), which
    # would write Python's capitalized "True"/"False".
    from sqi_client.models import ParameterUserInterface, ProductParameter
    from sqi_submitter.core.schema import FormModel
    from sqi_submitter.hosts.blender.addon import _bool_field_value, _copy_scene_values_into_model

    model = FormModel.from_parameters(
        [
            ProductParameter(
                name="Skip",
                type="STRING",
                allowed_values=["off", "on"],
                user_interface=ParameterUserInterface(control="CHECK_BOX"),
            ),
            ProductParameter(
                name="NoAllowedValues",
                type="STRING",
                user_interface=ParameterUserInterface(control="CHECK_BOX"),
            ),
        ]
    )
    fields = SimpleNamespace(sqi_field_Skip=True, sqi_field_NoAllowedValues=False)
    context = SimpleNamespace(scene=SimpleNamespace(sqi_fields=fields))

    _copy_scene_values_into_model(context, model)

    assert model.values()["Skip"] == "on"
    assert model.values()["NoAllowedValues"] == "false"  # <2 allowed_values fallback, lowercase

    # Directly pins the (off, on) mapping function used above.
    skip_field = next(f for f in model.fields if f.parameter.name == "Skip")
    assert _bool_field_value(True, skip_field) == "on"
    assert _bool_field_value(False, skip_field) == "off"


def test_field_property_group_maps_widgets_to_property_types(fake_host_module: Any) -> None:
    from sqi_client.models import ParameterUserInterface, ProductParameter
    from sqi_submitter.core.schema import FormModel
    from sqi_submitter.hosts.blender.addon import _build_field_property_group

    _install_fake_bpy_props(fake_host_module)
    model = FormModel.from_parameters(
        [
            ProductParameter(name="Chunk", type="INT", default="1"),
            ProductParameter(name="Scale", type="FLOAT", default="1.0"),
            ProductParameter(
                name="Skip",
                type="STRING",
                default="false",
                user_interface=ParameterUserInterface(control="CHECK_BOX"),
            ),
            ProductParameter(name="Frames", type="STRING"),
        ]
    )
    cls = _build_field_property_group(model)
    annotations = cls.__annotations__
    assert annotations["sqi_field_Chunk"] == ("prop", "IntProperty")
    assert annotations["sqi_field_Scale"] == ("prop", "FloatProperty")
    assert annotations["sqi_field_Skip"] == ("prop", "BoolProperty")
    assert annotations["sqi_field_Frames"] == ("prop", "StringProperty")
