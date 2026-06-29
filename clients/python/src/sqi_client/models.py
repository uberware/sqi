# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Typed models for the sqi client.

This module holds the pagination primitives (:class:`Page`, :func:`iter_pages`)
plus the resource dataclasses — :class:`Job`, :class:`Task`, :class:`Worker`,
:class:`Farm`, :class:`Queue`, :class:`StorageLocation`, :class:`UsagePool`,
and the log types — that mirror the server's OpenAPI component schemas
field-for-field.

Every model exposes a ``from_dict`` classmethod that parses a decoded JSON
response object. Parsing is deliberately *tolerant* for forward compatibility
with newer servers: RFC 3339 timestamps become timezone-aware
:class:`~datetime.datetime` values, nested objects are constructed recursively,
unknown JSON fields are ignored rather than raising, and unknown status enum
values are preserved verbatim instead of failing. This mirrors the Go server's
zero-value JSON semantics — a field the client does not recognize never crashes
a pipeline (or an artist's DCC session).
"""

from __future__ import annotations

import re
from collections.abc import Callable, Iterator, Mapping
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Any, Generic, TypeVar, cast

__all__ = [
    "CancelResult",
    "ComputeLocation",
    "CurrentTask",
    "Farm",
    "GPUInfo",
    "Job",
    "JobStatus",
    "LogChunk",
    "LogPage",
    "Page",
    "ParameterUserInterface",
    "Product",
    "ProductParameter",
    "Queue",
    "RetryJobResult",
    "RetryResult",
    "ServerVersion",
    "Step",
    "StorageLocation",
    "Task",
    "TaskCounts",
    "TaskStatus",
    "UsagePool",
    "Worker",
    "WorkerAction",
    "WorkerStatus",
    "iter_pages",
    "parse_page",
]

T = TypeVar("T")


@dataclass(frozen=True)
class Page(Generic[T]):
    """One page of a list response, mirroring the server's wrapper object.

    Attributes:
        items: The items on this page.
        total: Total number of items across all pages matching the query.
        limit: The page size the server applied.
        offset: The zero-based offset of this page.
    """

    items: list[T]
    total: int
    limit: int
    offset: int


def iter_pages(
    fetch_page: Callable[[int, int], Page[T]],
    *,
    limit: int,
    offset: int = 0,
) -> Iterator[T]:
    """Yield every item across pages, fetching lazily as the caller consumes.

    ``fetch_page`` is called as ``fetch_page(offset, limit)`` and must return the
    corresponding :class:`Page`. Iteration advances ``offset`` by the number of
    items received and stops as soon as the result set is exhausted — when a page
    comes back empty, when the server's ``total`` has been reached, or when a
    short page signals the end. "Short" is judged against the page size the
    server actually applied (``Page.limit``), not the requested ``limit``:
    sqi-server clamps oversized limits (max 1000) rather than rejecting them, and
    a clamped full page must not end the iteration early.

    Args:
        fetch_page: Callable returning the page at a given ``(offset, limit)``.
        limit: Page size to request.
        offset: Starting offset (defaults to the beginning of the result set).

    Yields:
        Each item, in the server's order, across page boundaries.
    """
    current = offset
    while True:
        page = fetch_page(current, limit)
        yield from page.items
        count = len(page.items)
        # The server may clamp the requested page size (sqi-server caps limit
        # at 1000), so a page is only "short" relative to the size the server
        # actually applied — judging by the requested limit would silently
        # truncate a clamped result set after its first page.
        applied_limit = page.limit if 0 < page.limit < limit else limit
        # Stop on an empty page, once the absolute position reaches the server's
        # reported total, or on a short page — all of which mark the end of the
        # result set. Using absolute position rather than a running count keeps
        # termination correct when `offset > 0`.
        if count == 0 or current + count >= page.total or count < applied_limit:
            return
        current += count


def parse_page(
    data: Mapping[str, Any],
    parse_item: Callable[[Mapping[str, Any]], T],
) -> Page[T]:
    """Build a :class:`Page` from the server's ``{items, total, limit, offset}``.

    ``parse_item`` constructs each item from its JSON object (typically a model's
    ``from_dict``). Non-object entries in ``items`` are skipped, and a missing or
    malformed wrapper degrades to an empty page rather than raising — consistent
    with the tolerant parsing the models use.
    """
    if not isinstance(data, Mapping):
        # A pathological 200 body (null, a bare array, empty) must degrade to an
        # empty page rather than raising on the .get() calls below.
        return Page(items=[], total=0, limit=0, offset=0)
    raw_items = data.get("items")
    items = (
        [parse_item(item) for item in raw_items if isinstance(item, dict)]
        if isinstance(raw_items, list)
        else []
    )
    return Page(
        items=items,
        total=_as_int(data.get("total")),
        limit=_as_int(data.get("limit")),
        offset=_as_int(data.get("offset")),
    )


# ── Tolerant field parsers ────────────────────────────────────────────────────
#
# Each helper takes the raw JSON value (which the decoder types as ``Any``) and
# coerces it to the declared field type, falling back to a type-appropriate
# zero value rather than raising on an absent or mistyped field. The optional
# variants return ``None`` for absent/``null``/mistyped values so nullable spec
# fields round-trip faithfully. Timestamps follow the same rule: a required
# timestamp that is absent or unparseable falls back to the zero-time sentinel
# (matching the Go server's zero ``time.Time`` wire form, ``0001-01-01T00:00:00Z``)
# and an optional one to ``None`` — parsing never raises, so a malformed field
# from a server or proxy cannot crash a pipeline.

_FRACTION_RE = re.compile(r"\.(\d+)")

# Tolerant fallback for a required timestamp that cannot be parsed. Mirrors the
# Go server's zero time.Time so an unset/garbled value reads as an obvious
# sentinel rather than raising mid-pipeline.
_ZERO_TIME = datetime.min.replace(tzinfo=timezone.utc)


def _normalize_fraction(match: re.Match[str]) -> str:
    """Pad or truncate a fractional-seconds run to exactly six digits.

    Python's ``datetime.fromisoformat`` before 3.11 accepts only 3- or 6-digit
    fractions, while Go's RFC 3339 output trims trailing zeros and can emit
    nanosecond precision. Normalizing to microseconds keeps a perfectly valid
    timestamp from raising on a 3.9 interpreter.
    """
    return "." + match.group(1)[:6].ljust(6, "0")


def _parse_datetime(value: str) -> datetime:
    """Parse an RFC 3339 timestamp string into a timezone-aware datetime."""
    text = value.strip()
    if text and text[-1] in ("Z", "z"):
        # 'Z' (the Zulu/UTC designator) is rejected by fromisoformat before
        # 3.11; swap it for an explicit zero offset.
        text = text[:-1] + "+00:00"
    text = _FRACTION_RE.sub(_normalize_fraction, text)
    parsed = datetime.fromisoformat(text)
    if parsed.tzinfo is None:
        # RFC 3339 always carries an offset; treat a stray naive value as UTC so
        # callers are guaranteed an aware datetime.
        parsed = parsed.replace(tzinfo=timezone.utc)
    return parsed


def _as_str(value: Any) -> str:
    return value if isinstance(value, str) else ""


def _opt_str(value: Any) -> str | None:
    return value if isinstance(value, str) else None


def _as_int(value: Any) -> int:
    # bool is a subclass of int in Python; never let True/False masquerade as 1/0.
    if isinstance(value, bool):
        return 0
    return value if isinstance(value, int) else 0


def _opt_int(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    return value if isinstance(value, int) else None


def _as_bool(value: Any) -> bool:
    return value if isinstance(value, bool) else False


def _opt_bool(value: Any) -> bool | None:
    return value if isinstance(value, bool) else None


def _str_dict(value: Any) -> dict[str, str]:
    if not isinstance(value, dict):
        return {}
    return {str(k): str(v) for k, v in value.items()}


def _str_list(value: Any) -> list[str]:
    if not isinstance(value, list):
        return []
    return [str(item) for item in value]


def _as_datetime(value: Any) -> datetime:
    if isinstance(value, str):
        try:
            return _parse_datetime(value)
        except ValueError:
            pass
    return _ZERO_TIME


def _opt_datetime(value: Any) -> datetime | None:
    if not isinstance(value, str):
        return None
    try:
        return _parse_datetime(value)
    except ValueError:
        return None


def _as_mapping(value: Any) -> Mapping[str, Any]:
    """Return ``value`` if it is a JSON object, else an empty mapping.

    Used for required nested objects (e.g. a worker's ``gpu``) so a missing or
    mistyped sub-object yields an all-default model rather than raising.
    """
    return value if isinstance(value, dict) else {}


# ── Status enums (tolerant) ───────────────────────────────────────────────────

_E = TypeVar("_E", bound="_OpenStrEnum")


class _OpenStrEnum(str, Enum):
    """A string enum that tolerates values newer servers may introduce.

    Subclasses declare the members this client version knows. An unrecognized
    value is *preserved* — wrapped in a pseudo-member that compares equal to its
    wire string — rather than raising, so a status added in a future server
    release never breaks parsing. Unknown values are not registered in the
    canonical member map; iterating the enum still yields only the declared
    members.

    Compare statuses by value (``status == JobStatus.RUNNING`` or
    ``status == "running"``), not identity: declared members are singletons, but
    each unknown value yields a fresh pseudo-member, so ``is`` is unreliable for
    a status that might be one a newer server introduced.
    """

    @classmethod
    def _missing_(cls, value: object) -> _OpenStrEnum | None:
        if not isinstance(value, str):
            return None
        pseudo: Any = str.__new__(cls, value)
        pseudo._name_ = value
        pseudo._value_ = value
        return cast("_OpenStrEnum", pseudo)

    @classmethod
    def parse(cls: type[_E], value: str) -> _E:
        """Return the member for ``value``, preserving unknown values verbatim."""
        return cls(value)


class JobStatus(_OpenStrEnum):
    """Lifecycle status of a job (OpenAPI ``Job.status``)."""

    PENDING = "pending"
    RUNNING = "running"
    COMPLETED = "completed"
    FAILED = "failed"
    CANCELED = "canceled"
    PAUSED = "paused"


class TaskStatus(_OpenStrEnum):
    """Lifecycle status of a task (OpenAPI ``Task.status``)."""

    PENDING = "pending"
    READY = "ready"
    ASSIGNED = "assigned"
    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELED = "canceled"


class WorkerStatus(_OpenStrEnum):
    """Connectivity/availability status of a worker (OpenAPI ``Worker.status``)."""

    ONLINE = "online"
    OFFLINE = "offline"
    DISABLED = "disabled"


# ── Job, Step, TaskCounts ─────────────────────────────────────────────────────


@dataclass(frozen=True)
class Step:
    """A single step within a job's OpenJD template."""

    id: str
    name: str
    step_order: int
    status: str
    created_at: datetime
    updated_at: datetime
    depends_on: list[str] = field(default_factory=list)

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Step:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            name=_as_str(data.get("name")),
            step_order=_as_int(data.get("step_order")),
            status=_as_str(data.get("status")),
            created_at=_as_datetime(data.get("created_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            depends_on=_str_list(data.get("depends_on")),
        )


@dataclass(frozen=True)
class TaskCounts:
    """Per-status task tallies for a job (OpenAPI ``TaskCounts``)."""

    total: int
    pending: int
    ready: int
    assigned: int
    running: int
    succeeded: int
    failed: int
    canceled: int

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> TaskCounts:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            total=_as_int(data.get("total")),
            pending=_as_int(data.get("pending")),
            ready=_as_int(data.get("ready")),
            assigned=_as_int(data.get("assigned")),
            running=_as_int(data.get("running")),
            succeeded=_as_int(data.get("succeeded")),
            failed=_as_int(data.get("failed")),
            canceled=_as_int(data.get("canceled")),
        )


@dataclass(frozen=True)
class Job:
    """A submitted job.

    The ``steps`` and ``task_counts`` fields are populated only by
    :meth:`~sqi_client.client.SqiClient.get_job` (the ``JobDetail`` response);
    on a list-level job they are ``None``.
    """

    id: str
    farm_id: str
    queue_id: str
    name: str
    owner: str
    submitter: str
    priority: int
    status: JobStatus
    template_format: str
    created_at: datetime
    updated_at: datetime
    project: str | None = None
    started_at: datetime | None = None
    completed_at: datetime | None = None
    steps: list[Step] | None = None
    task_counts: TaskCounts | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Job:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        raw_steps = data.get("steps")
        raw_counts = data.get("task_counts")
        steps = (
            [Step.from_dict(s) for s in raw_steps if isinstance(s, dict)]
            if isinstance(raw_steps, list)
            else None
        )
        return cls(
            id=_as_str(data.get("id")),
            farm_id=_as_str(data.get("farm_id")),
            queue_id=_as_str(data.get("queue_id")),
            name=_as_str(data.get("name")),
            owner=_as_str(data.get("owner")),
            submitter=_as_str(data.get("submitter")),
            priority=_as_int(data.get("priority")),
            status=JobStatus.parse(_as_str(data.get("status"))),
            template_format=_as_str(data.get("template_format")),
            created_at=_as_datetime(data.get("created_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            project=_opt_str(data.get("project")),
            started_at=_opt_datetime(data.get("started_at")),
            completed_at=_opt_datetime(data.get("completed_at")),
            steps=steps,
            task_counts=(
                TaskCounts.from_dict(raw_counts) if isinstance(raw_counts, dict) else None
            ),
        )


# ── Tasks ─────────────────────────────────────────────────────────────────────


@dataclass(frozen=True)
class Task:
    """A single unit of work within a job."""

    id: str
    job_id: str
    step_id: str
    name: str
    status: TaskStatus
    created_at: datetime
    updated_at: datetime
    parameters: dict[str, str] = field(default_factory=dict)
    assigned_worker_id: str | None = None
    assigned_at: datetime | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Task:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            job_id=_as_str(data.get("job_id")),
            step_id=_as_str(data.get("step_id")),
            name=_as_str(data.get("name")),
            status=TaskStatus.parse(_as_str(data.get("status"))),
            created_at=_as_datetime(data.get("created_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            parameters=_str_dict(data.get("parameters")),
            assigned_worker_id=_opt_str(data.get("assigned_worker_id")),
            assigned_at=_opt_datetime(data.get("assigned_at")),
        )


@dataclass(frozen=True)
class RetryResult:
    """Outcome of retrying a task (OpenAPI ``RetryResponse``).

    Returned by :meth:`~sqi_client.client.SqiClient.retry_task`: the task that
    was retried and the status it was reset to (typically ``ready``).
    """

    task_id: str
    status: TaskStatus

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> RetryResult:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            task_id=_as_str(data.get("task_id")),
            status=TaskStatus.parse(_as_str(data.get("status"))),
        )


@dataclass(frozen=True)
class CancelResult:
    """Outcome of canceling a task (OpenAPI ``CancelResponse``).

    Returned by :meth:`~sqi_client.client.SqiClient.cancel_task`: the task that
    was canceled and the status it was set to (``canceled``).
    """

    task_id: str
    status: TaskStatus

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> CancelResult:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            task_id=_as_str(data.get("task_id")),
            status=TaskStatus.parse(_as_str(data.get("status"))),
        )


@dataclass(frozen=True)
class RetryJobResult:
    """Outcome of retrying a job's failed/canceled tasks (OpenAPI ``RetryJobResponse``).

    Returned by :meth:`~sqi_client.client.SqiClient.retry_job`: the job and the
    number of tasks revived (``0`` when none were eligible).
    """

    job_id: str
    retried: int

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> RetryJobResult:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            job_id=_as_str(data.get("job_id")),
            retried=_as_int(data.get("retried")),
        )


# ── Workers ───────────────────────────────────────────────────────────────────


@dataclass(frozen=True)
class GPUInfo:
    """GPU hardware description reported by a worker (all fields optional)."""

    vendor: str | None = None
    model: str | None = None
    vram_mb: int | None = None
    count: int | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> GPUInfo:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            vendor=_opt_str(data.get("vendor")),
            model=_opt_str(data.get("model")),
            vram_mb=_opt_int(data.get("vram_mb")),
            count=_opt_int(data.get("count")),
        )


@dataclass(frozen=True)
class CurrentTask:
    """A task a worker is currently executing (an entry in
    ``WorkerDetail.current_tasks``)."""

    id: str
    job_id: str
    name: str
    status: TaskStatus
    assigned_at: datetime | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> CurrentTask:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            job_id=_as_str(data.get("job_id")),
            name=_as_str(data.get("name")),
            status=TaskStatus.parse(_as_str(data.get("status"))),
            assigned_at=_opt_datetime(data.get("assigned_at")),
        )


@dataclass(frozen=True)
class Worker:
    """A registered worker node.

    The ``current_tasks`` field is populated only by
    :meth:`~sqi_client.client.SqiClient.get_worker` (the ``WorkerDetail``
    response); on a list-level worker it is an empty list.
    """

    id: str
    farm_id: str
    hostname: str
    gpu: GPUInfo
    status: WorkerStatus
    registered_at: datetime
    updated_at: datetime
    queue_id: str | None = None
    ip_address: str | None = None
    compute_location: str | None = None
    os: str | None = None
    os_version: str | None = None
    version: str | None = None
    """The sqi-worker build version the worker self-reports at registration;
    ``None`` if the worker did not report one."""
    cpu_count: int | None = None
    ram_mb: int | None = None
    tags: dict[str, str] = field(default_factory=dict)
    last_heartbeat_at: datetime | None = None
    current_tasks: list[CurrentTask] = field(default_factory=list)
    removable: bool = False
    """Whether the worker may be removed via
    :meth:`~sqi_client.client.SqiClient.remove_worker`. Server-authoritative:
    true for offline workers and for disabled workers whose last heartbeat is
    older than the heartbeat-timeout window; online and live-disabled workers
    are never removable."""

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Worker:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        raw_current = data.get("current_tasks")
        current_tasks = (
            [CurrentTask.from_dict(t) for t in raw_current if isinstance(t, dict)]
            if isinstance(raw_current, list)
            else []
        )
        return cls(
            id=_as_str(data.get("id")),
            farm_id=_as_str(data.get("farm_id")),
            hostname=_as_str(data.get("hostname")),
            gpu=GPUInfo.from_dict(_as_mapping(data.get("gpu"))),
            status=WorkerStatus.parse(_as_str(data.get("status"))),
            registered_at=_as_datetime(data.get("registered_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            queue_id=_opt_str(data.get("queue_id")),
            ip_address=_opt_str(data.get("ip_address")),
            compute_location=_opt_str(data.get("compute_location")),
            os=_opt_str(data.get("os")),
            os_version=_opt_str(data.get("os_version")),
            version=_opt_str(data.get("version")),
            cpu_count=_opt_int(data.get("cpu_count")),
            ram_mb=_opt_int(data.get("ram_mb")),
            tags=_str_dict(data.get("tags")),
            last_heartbeat_at=_opt_datetime(data.get("last_heartbeat_at")),
            current_tasks=current_tasks,
            removable=_as_bool(data.get("removable")),
        )


@dataclass(frozen=True)
class WorkerAction:
    """Outcome of an enable/disable worker call (OpenAPI ``WorkerActionResponse``).

    Returned by :meth:`~sqi_client.client.SqiClient.enable_worker` and
    :meth:`~sqi_client.client.SqiClient.disable_worker`: the worker's ID and its
    status after the operation. This is the lightweight body those endpoints
    return — not a full :class:`Worker`.
    """

    id: str
    status: WorkerStatus

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> WorkerAction:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            status=WorkerStatus.parse(_as_str(data.get("status"))),
        )


# ── Farms, queues, and resources ──────────────────────────────────────────────


@dataclass(frozen=True)
class Farm:
    """A render farm — the top-level grouping of queues and workers."""

    id: str
    name: str
    max_concurrent_tasks: int
    created_at: datetime
    updated_at: datetime
    description: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Farm:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            name=_as_str(data.get("name")),
            max_concurrent_tasks=_as_int(data.get("max_concurrent_tasks")),
            created_at=_as_datetime(data.get("created_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            description=_opt_str(data.get("description")),
        )


@dataclass(frozen=True)
class Queue:
    """A scheduling queue within a farm."""

    id: str
    farm_id: str
    name: str
    priority: int
    max_concurrent_tasks: int
    paused: bool
    created_at: datetime
    updated_at: datetime
    description: str | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Queue:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            farm_id=_as_str(data.get("farm_id")),
            name=_as_str(data.get("name")),
            priority=_as_int(data.get("priority")),
            max_concurrent_tasks=_as_int(data.get("max_concurrent_tasks")),
            paused=_as_bool(data.get("paused")),
            created_at=_as_datetime(data.get("created_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            description=_opt_str(data.get("description")),
        )


@dataclass(frozen=True)
class ComputeLocation:
    """A named, registered compute location."""

    id: str
    name: str
    created_at: datetime
    updated_at: datetime
    description: str | None = None
    worker_count: int = 0

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> ComputeLocation:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            name=_as_str(data.get("name")),
            created_at=_as_datetime(data.get("created_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            description=_opt_str(data.get("description")),
            worker_count=_as_int(data.get("worker_count")),
        )


@dataclass(frozen=True)
class StorageLocation:
    """A named storage location with per-compute-location root paths."""

    id: str
    name: str
    type: str
    created_at: datetime
    updated_at: datetime
    description: str | None = None
    roots: dict[str, str] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> StorageLocation:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            name=_as_str(data.get("name")),
            type=_as_str(data.get("type")),
            created_at=_as_datetime(data.get("created_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            description=_opt_str(data.get("description")),
            roots=_str_dict(data.get("roots")),
        )


@dataclass(frozen=True)
class UsagePool:
    """A pool that caps the number of concurrent usage claims."""

    id: str
    name: str
    max_concurrent: int
    created_at: datetime
    updated_at: datetime
    server_hint: str | None = None
    #: Slots currently claimed (active, unreleased). Read-only; server-computed.
    in_use: int = 0
    #: Free slots: max(max_concurrent - in_use, 0). Read-only; server-computed.
    available: int = 0

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> UsagePool:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            name=_as_str(data.get("name")),
            max_concurrent=_as_int(data.get("max_concurrent")),
            created_at=_as_datetime(data.get("created_at")),
            updated_at=_as_datetime(data.get("updated_at")),
            server_hint=_opt_str(data.get("server_hint")),
            in_use=_as_int(data.get("in_use")),
            available=_as_int(data.get("available")),
        )


# ── Logs ──────────────────────────────────────────────────────────────────────


@dataclass(frozen=True)
class LogChunk:
    """One chunk of a task attempt's captured output (OpenAPI ``TaskLog``)."""

    id: str
    task_id: str
    attempt_id: str
    seq_num: int
    nats_seq: int
    stream: str
    data: str
    at: datetime
    received_at: datetime

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> LogChunk:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            id=_as_str(data.get("id")),
            task_id=_as_str(data.get("task_id")),
            attempt_id=_as_str(data.get("attempt_id")),
            seq_num=_as_int(data.get("seq_num")),
            nats_seq=_as_int(data.get("nats_seq")),
            stream=_as_str(data.get("stream")),
            data=_as_str(data.get("data")),
            at=_as_datetime(data.get("at")),
            received_at=_as_datetime(data.get("received_at")),
        )


@dataclass(frozen=True)
class LogPage:
    """A page of log chunks plus the cursor to resume from.

    ``after_nats_seq`` echoes the highest NATS sequence on this page; pass it as
    the ``after_nats_seq`` argument of the next ``get_task_logs`` call to fetch
    only newer chunks.
    """

    items: list[LogChunk]
    after_nats_seq: int
    limit: int

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> LogPage:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        raw_items = data.get("items")
        items = (
            [LogChunk.from_dict(c) for c in raw_items if isinstance(c, dict)]
            if isinstance(raw_items, list)
            else []
        )
        return cls(
            items=items,
            after_nats_seq=_as_int(data.get("after_nats_seq")),
            limit=_as_int(data.get("limit")),
        )


# ── Server metadata ───────────────────────────────────────────────────────────


@dataclass(frozen=True)
class ServerVersion:
    """The running sqi-server's build metadata (OpenAPI ``VersionInfo``).

    Returned by :meth:`~sqi_client.client.SqiClient.server_version`. Development
    builds report ``"dev"`` for :attr:`version` and ``"unknown"`` for the rest.
    """

    version: str
    """Semantic version tag, e.g. ``"v0.1.0"`` or ``"dev"``."""
    commit: str
    """Short git commit hash, or ``"unknown"``."""
    build_date: str
    """RFC 3339 build timestamp, or ``"unknown"``."""
    go_version: str
    """Go toolchain version, or ``"unknown"``."""

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> ServerVersion:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            version=_as_str(data.get("version")),
            commit=_as_str(data.get("commit")),
            build_date=_as_str(data.get("build_date")),
            go_version=_as_str(data.get("go_version")),
        )


# ── Products ──────────────────────────────────────────────────────────────────


@dataclass(frozen=True)
class ParameterUserInterface:
    """OpenJD base-spec userInterface presentation hints on a job parameter."""

    control: str = ""
    label: str = ""
    group_label: str = ""
    decimals: int | None = None
    single_step_removal: bool | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> ParameterUserInterface:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            control=_as_str(data.get("control")),
            label=_as_str(data.get("label")),
            group_label=_as_str(data.get("group_label")),
            decimals=_opt_int(data.get("decimals")),
            single_step_removal=_opt_bool(data.get("single_step_removal")),
        )


@dataclass(frozen=True)
class ProductParameter:
    """A parsed job parameter from a product's template (with UI hints)."""

    name: str
    type: str
    description: str = ""
    default: str | None = None
    allowed_values: list[str] | None = None
    min_value: str | None = None
    max_value: str | None = None
    min_length: int | None = None
    max_length: int | None = None
    object_type: str = ""
    data_flow: str = ""
    user_interface: ParameterUserInterface | None = None

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> ProductParameter:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        ui = data.get("user_interface")
        return cls(
            name=_as_str(data.get("name")),
            type=_as_str(data.get("type")),
            description=_as_str(data.get("description")),
            default=_opt_str(data.get("default")),
            allowed_values=(
                [_as_str(v) for v in data["allowed_values"]]
                if isinstance(data.get("allowed_values"), list)
                else None
            ),
            min_value=_opt_str(data.get("min_value")),
            max_value=_opt_str(data.get("max_value")),
            min_length=_opt_int(data.get("min_length")),
            max_length=_opt_int(data.get("max_length")),
            object_type=_as_str(data.get("object_type")),
            data_flow=_as_str(data.get("data_flow")),
            user_interface=(
                ParameterUserInterface.from_dict(ui) if isinstance(ui, Mapping) else None
            ),
        )


@dataclass(frozen=True)
class Product:
    """A product: a thin, friendly catalog entry over an OpenJD template."""

    name: str
    title: str = ""
    description: str = ""
    category: str = ""
    version: str = ""
    source: str = ""
    template: str = ""
    format: str = ""

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> Product:
        """Build an instance from a decoded JSON response object.

        Unknown fields are ignored and missing or mistyped fields fall back to
        type-appropriate defaults; see the module docstring for the full
        tolerant-parsing contract.
        """
        return cls(
            name=_as_str(data.get("name")),
            title=_as_str(data.get("title")),
            description=_as_str(data.get("description")),
            category=_as_str(data.get("category")),
            version=_as_str(data.get("version")),
            source=_as_str(data.get("source")),
            template=_as_str(data.get("template")),
            format=_as_str(data.get("format")),
        )
