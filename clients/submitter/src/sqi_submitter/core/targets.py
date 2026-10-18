# SPDX-License-Identifier: AGPL-3.0-or-later
"""Multi-target submission: one job per ticked render target (no Qt, no DCC).

A product parameter is a *target parameter* when its normalized name matches a
key present in the ``extra`` of every target; those are the keys that identify
a target (``RenderLayer``, ``Take``, ``RenderQueueItem``). A product with at
least one is a *per-target product*. Keys only some targets carry (After
Effects' ``FramesPerTask`` on movie targets) are per-target pre-fill values and
never make a product per-target.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from dataclasses import dataclass

from sqi_client.models import Job, ProductParameter
from sqi_submitter.core.adapter import HostAdapter
from sqi_submitter.core.context import RenderTarget, SceneContext
from sqi_submitter.core.errors import FormInvalidError, SubmitterError
from sqi_submitter.core.joboptions import JobOptions
from sqi_submitter.core.mapping import normalize_name, prefill
from sqi_submitter.core.schema import FormModel
from sqi_submitter.core.session import SubmitterSession
from sqi_submitter.core.submit import SCENE_NOT_SAVED_MESSAGE, scene_save_failed, submit_form


def target_parameters(
    parameters: Sequence[ProductParameter], targets: Sequence[RenderTarget]
) -> frozenset[str]:
    """Names of the product parameters every target supplies through ``extra``."""
    if not targets:
        return frozenset()
    shared = set.intersection(*({normalize_name(k) for k in t.extra} for t in targets))
    return frozenset(p.name for p in parameters if normalize_name(p.name) in shared)


def is_per_target(parameters: Sequence[ProductParameter], targets: Sequence[RenderTarget]) -> bool:
    """True when the product has at least one target parameter."""
    return bool(target_parameters(parameters, targets))


def per_target_models(
    parameters: Sequence[ProductParameter],
    context: SceneContext,
    targets: Sequence[RenderTarget],
    edited: Mapping[str, str],
    *,
    target_params: frozenset[str] | None = None,
) -> list[tuple[RenderTarget, FormModel]]:
    """Build one pre-filled form model per target.

    Each model starts from the parameter defaults plus ``prefill`` for that
    target, then takes the artist's ``edited`` values, except for target
    parameters (always the target's own) and for keys the target ``locked``.

    ``target_params`` defaults to :func:`target_parameters` over ``targets``.
    A caller building models for a *subset* of the host's targets (the ticked
    ones) passes the set computed over all of them: otherwise a key every
    ticked target happens to carry would count as a target parameter, and an
    edit to it would be honoured or dropped depending on what else is ticked.
    """
    if target_params is None:
        target_params = target_parameters(parameters, targets)
    models: list[tuple[RenderTarget, FormModel]] = []
    for target in targets:
        model = FormModel.from_parameters(parameters)
        model.apply_prefill(prefill(parameters, context, target))
        locked = {normalize_name(k) for k in target.locked}
        locked_names = {p.name for p in parameters if normalize_name(p.name) in locked}
        apply_edits(model, edited, skip=target_params | locked_names)
        models.append((target, model))
    return models


def apply_edits(
    model: FormModel, edited: Mapping[str, str], *, skip: frozenset[str] = frozenset()
) -> None:
    """Set each of the artist's ``edited`` values the model has a field for, except ``skip``."""
    known = {f.parameter.name for f in model.fields}
    for name, value in edited.items():
        if name in known and name not in skip:
            model.set_value(name, value)


def job_name_for(prefix: str, target: RenderTarget) -> str:
    return f"{prefix} - {target.name}"


@dataclass(frozen=True)
class TargetResult:
    """Outcome of submitting one target: exactly one of ``job``/``error`` is set."""

    target: RenderTarget
    job: Job | None
    error: str | None


def _error_text(exc: SubmitterError) -> str:
    """One line fit for a per-target result list.

    A ``FormInvalidError``'s own message points at highlighted fields, which a
    result list has none of, so it names each invalid field instead.
    """
    if isinstance(exc, FormInvalidError) and exc.errors:
        details = "; ".join(f"{name}: {message}" for name, message in exc.errors.items())
        return f"Invalid parameters — {details}"
    return exc.user_message


def submit_targets(
    session: SubmitterSession,
    product_name: str,
    models: Sequence[tuple[RenderTarget, FormModel]],
    *,
    farm_id: str,
    queue_id: str,
    job_name_prefix: str,
    adapter: HostAdapter | None,
    save_scene: bool,
    job_options: JobOptions | None,
) -> list[TargetResult]:
    """Submit each model as its own job, in order; a failure does not stop the rest.

    The scene is saved once, up front, when ``save_scene`` is set and the host
    reports it modified. If that save fails nothing is submitted and every
    target reports the failure, since each job would render a stale file.
    """
    if scene_save_failed(adapter, save_scene):
        return [
            TargetResult(target=target, job=None, error=SCENE_NOT_SAVED_MESSAGE)
            for target, _ in models
        ]
    results: list[TargetResult] = []
    for target, model in models:
        try:
            job = submit_form(
                session,
                product_name,
                model,
                farm_id=farm_id,
                queue_id=queue_id,
                job_name=job_name_for(job_name_prefix, target),
                adapter=adapter,
                save_scene=False,
                job_options=job_options,
            )
        except SubmitterError as exc:
            results.append(TargetResult(target=target, job=None, error=_error_text(exc)))
        else:
            results.append(TargetResult(target=target, job=job, error=None))
    return results
