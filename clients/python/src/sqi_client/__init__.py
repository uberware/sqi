# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""sqi-client — a pure-Python client for the sqi distributed task farm manager.

This package wraps the sqi-server REST and WebSocket API for scripted job
submission, querying, and management. It is the foundation for the Phase 2 DCC
submitters and for pipeline automation scripts (see the project ``ROADMAP.md``).

The curated public API is re-exported here and listed in ``__all__``: the
client, its typed models, the exception hierarchy, and — when importable — the
optional WebSocket event stream. Everything else is underscore-private and not
part of the supported interface.
"""

from __future__ import annotations

from ._version import __version__
from .client import JobTemplate, SqiClient
from .errors import (
    APIError,
    BadRequestError,
    ConflictError,
    NotFoundError,
    RateLimitError,
    ServerError,
    SqiConnectionError,
    SqiError,
    SqiTimeoutError,
    ValidationError,
)
from .models import (
    CancelResult,
    CurrentTask,
    Farm,
    GPUInfo,
    Job,
    JobStatus,
    LogChunk,
    LogPage,
    Page,
    Queue,
    RetryResult,
    Step,
    StorageLocation,
    Task,
    TaskCounts,
    TaskStatus,
    UsagePool,
    Worker,
    WorkerAction,
    WorkerStatus,
    iter_pages,
    parse_page,
)

__all__ = [
    "APIError",
    "BadRequestError",
    "CancelResult",
    "ConflictError",
    "CurrentTask",
    "Farm",
    "GPUInfo",
    "Job",
    "JobStatus",
    "JobTemplate",
    "LogChunk",
    "LogPage",
    "NotFoundError",
    "Page",
    "Queue",
    "RateLimitError",
    "RetryResult",
    "ServerError",
    "SqiClient",
    "SqiConnectionError",
    "SqiError",
    "SqiTimeoutError",
    "Step",
    "StorageLocation",
    "Task",
    "TaskCounts",
    "TaskStatus",
    "UsagePool",
    "ValidationError",
    "Worker",
    "WorkerAction",
    "WorkerStatus",
    "__version__",
    "iter_pages",
    "parse_page",
]

# The optional WebSocket surface lives in ``events`` (importable without the
# ``ws`` extra — ``websockets`` is loaded lazily on connect). Guarded so a
# missing or broken events module never breaks importing the core package.
try:
    from .events import Event, SqiEventStream
except ImportError:  # pragma: no cover - events imports cleanly in supported setups
    pass
else:
    __all__ += ["Event", "SqiEventStream"]
