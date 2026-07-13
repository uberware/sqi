# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""The synchronous sqi client and its HTTP transport core.

:class:`SqiClient` owns an :class:`httpx.Client`, applies default headers, joins
request paths under the ``/api/v1`` prefix, maps non-2xx responses to typed
errors, retries idempotent requests with backoff, and exposes health probes.
Resource-specific methods (jobs, tasks, workers, ...) are layered on top in later
sections; this module provides the shared request machinery they call.
"""

from __future__ import annotations

import json
import logging
import random
import re
import time
import warnings
from collections.abc import Callable, Iterator, Mapping
from enum import Enum
from pathlib import Path
from types import TracebackType
from typing import TYPE_CHECKING, Any, Generic, TypeVar, Union
from urllib.parse import quote

import httpx

if TYPE_CHECKING:
    from .events import SqiEventStream

from ._version import __version__
from .errors import (
    SqiConnectionError,
    SqiError,
    SqiTimeoutError,
    _parse_retry_after,
    api_error_from_response,
)
from .models import (
    CancelResult,
    ComputeLocation,
    Farm,
    Job,
    JobStatus,
    LogChunk,
    LogPage,
    Page,
    Product,
    ProductParameter,
    Queue,
    RetryJobResult,
    RetryResult,
    ServerVersion,
    StorageLocation,
    Task,
    TaskAttempt,
    TaskStatus,
    UsagePool,
    Worker,
    WorkerAction,
    WorkerStatus,
    iter_pages,
    parse_page,
)

_T = TypeVar("_T")

__all__ = ["JobTemplate", "SqiClient"]

# Accepted shapes for an OpenJD job template at submission time: a raw string
# (sent verbatim), a filesystem path (read and sent), or a dict (serialized to
# JSON). See :meth:`SqiClient.submit_job`.
JobTemplate = Union[str, Path, "dict[str, Any]"]

logger = logging.getLogger("sqi_client")

_API_PREFIX = "/api/v1"

# The API contract major version this client is written against — the "1" in
# /api/v1, which sqi-server confirms via the X-API-Version response header on
# every API response. It is decoupled from the product release version:
# additive changes (new endpoints, fields, enum values) do not move it; only a
# breaking change does, shipping as /api/v2 with X-API-Version: 2. A server
# advertising a higher major triggers a one-time warning.
_SUPPORTED_API_MAJOR = 1


class _CrudResource(Generic[_T]):
    """Generic CRUD transport for one REST resource family.

    Parameterized by the collection ``path`` (e.g. ``/farms``) and a ``parse``
    function (a model's ``from_dict``). Covers the five standard operations the
    server exposes for each resource — ``POST /``, ``GET /``, ``GET /{id}``,
    ``PUT /{id}``, ``DELETE /{id}`` — so every resource family shares one tested
    code path. ``GET /`` comes in two flavors because the server returns a bare
    array for some resources (:meth:`list_all`) and a paginated wrapper for
    others (:meth:`list_page`).
    """

    def __init__(
        self,
        client: SqiClient,
        path: str,
        parse: Callable[[Mapping[str, Any]], _T],
    ) -> None:
        self._client = client
        self._path = path
        self._parse = parse

    def create(self, body: Mapping[str, Any]) -> _T:
        data = self._client._request_json("POST", self._path, json=body)
        return self._parse(data)

    def get(self, resource_id: str) -> _T:
        data = self._client._request_json("GET", self._item_path(resource_id))
        return self._parse(data)

    def update(self, resource_id: str, body: Mapping[str, Any]) -> _T:
        data = self._client._request_json("PUT", self._item_path(resource_id), json=body)
        return self._parse(data)

    def delete(self, resource_id: str) -> None:
        self._client._request("DELETE", self._item_path(resource_id))

    def list_all(self) -> list[_T]:
        """List via a bare JSON array response (no pagination)."""
        data = self._client._request_json("GET", self._path)
        if not isinstance(data, list):
            return []
        return [self._parse(item) for item in data if isinstance(item, dict)]

    def list_page(self, params: Mapping[str, Any]) -> Page[_T]:
        """List via a paginated ``{items, total, limit, offset}`` response."""
        data = self._client._request_json("GET", self._path, params=params)
        return parse_page(data, self._parse)

    def _item_path(self, resource_id: str) -> str:
        return f"{self._path}/{quote(resource_id, safe='')}"


class SqiClient:
    """A synchronous client for a single ``sqi-server`` instance.

    Args:
        base_url: Server root URL, e.g. ``http://localhost:8080`` (no
            ``/api/v1`` suffix — that prefix is added per request). A path
            component is preserved, so a reverse-proxy subpath works:
            ``http://host/sqi`` sends requests to ``http://host/sqi/api/v1/…``.
        timeout: Per-request timeout in seconds (default 30).
        headers: Extra default headers merged into every request. This is the
            forward-compatible hook for Phase 3 authentication; supplied headers
            take precedence over the client's own defaults on conflict.
        max_attempts: Total attempts for a retried (idempotent GET) request,
            including the first (default 3). ``1`` disables retries.
        retry_backoff: Base delay in seconds for exponential backoff between
            retries (default 0.5).
        retry_backoff_max: Cap on the computed backoff delay in seconds.
        retry_jitter: When true (default), randomize each backoff delay in
            ``[0, computed]`` to avoid thundering-herd retries.

    The client may be used as a context manager (closing the underlying
    connection pool on exit) or closed explicitly via :meth:`close`. The
    underlying ``httpx.Client`` is thread-safe; a single instance may be shared
    across threads. The once-per-instance version-skew warning and the
    once-per-endpoint deprecation warnings are best-effort under concurrency
    (they may, harmlessly, be emitted more than once).
    """

    def __init__(
        self,
        base_url: str,
        *,
        timeout: float = 30.0,
        headers: Mapping[str, str] | None = None,
        max_attempts: int = 3,
        retry_backoff: float = 0.5,
        retry_backoff_max: float = 30.0,
        retry_jitter: bool = True,
    ) -> None:
        self._base_url = base_url.rstrip("/")
        default_headers = {
            "User-Agent": f"sqi-sdk/{__version__}",
            "Accept": "application/json",
        }
        if headers:
            default_headers.update(headers)
        # Retained for the WebSocket upgrade (events()), which uses a separate
        # connection but should carry the same headers (User-Agent, the Phase 3
        # auth hook).
        self._default_headers = default_headers
        self._http = httpx.Client(
            base_url=self._base_url,
            timeout=timeout,
            headers=default_headers,
        )
        self._max_attempts = max(1, max_attempts)
        self._retry_backoff = retry_backoff
        self._retry_backoff_max = retry_backoff_max
        self._retry_jitter = retry_jitter
        self._version_warned = False
        self._deprecation_warned: set[str] = set()

        # Generic CRUD transports shared by the resource methods below.
        self._farms: _CrudResource[Farm] = _CrudResource(self, "/farms", Farm.from_dict)
        self._queues: _CrudResource[Queue] = _CrudResource(self, "/queues", Queue.from_dict)
        self._storage_locations: _CrudResource[StorageLocation] = _CrudResource(
            self, "/storage-locations", StorageLocation.from_dict
        )
        self._compute_locations: _CrudResource[ComputeLocation] = _CrudResource(
            self, "/compute-locations", ComputeLocation.from_dict
        )
        self._usage_pools: _CrudResource[UsagePool] = _CrudResource(
            self, "/usage-pools", UsagePool.from_dict
        )
        self._products: _CrudResource[Product] = _CrudResource(self, "/products", Product.from_dict)

    # ── Lifecycle ─────────────────────────────────────────────────────────────

    def __enter__(self) -> SqiClient:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        self.close()

    def close(self) -> None:
        """Close the underlying HTTP connection pool."""
        self._http.close()

    def __repr__(self) -> str:
        return f"SqiClient(base_url={self._base_url!r})"

    # ── URL / parameter helpers ───────────────────────────────────────────────

    def _build_url(self, path: str, *, api: bool = True) -> str:
        suffix = path if path.startswith("/") else f"/{path}"
        return f"{_API_PREFIX}{suffix}" if api else suffix

    # ── Retry / backoff ───────────────────────────────────────────────────────

    def _backoff_delay(self, attempt: int, retry_after: float | None) -> float:
        if retry_after is not None:
            # Honor the server's Retry-After, clamped to [0, retry_backoff_max]:
            # never sleep a negative duration (time.sleep would raise), and never
            # let a server-supplied value stall a request beyond the caller's
            # configured ceiling.
            return min(max(0.0, retry_after), self._retry_backoff_max)
        # 2.0** keeps this float; int**int is typed Any (negative-exponent overload).
        capped = min(self._retry_backoff * (2.0 ** (attempt - 1)), self._retry_backoff_max)
        if self._retry_jitter:
            return random.uniform(0, capped)
        return capped

    def _log_retry(self, method: str, url: str, attempt: int, reason: str) -> None:
        logger.warning(
            "sqi-sdk retry %d/%d: %s %s (%s)",
            attempt,
            self._max_attempts - 1,
            method,
            url,
            reason,
        )

    # ── Version negotiation ───────────────────────────────────────────────────

    def _check_version_headers(self, response: httpx.Response) -> None:
        # Endpoint deprecation and version skew are independent signals; each
        # warns on its own.
        self._warn_deprecated_endpoint(response)
        self._warn_version_skew(response)

    def _warn_deprecated_endpoint(self, response: httpx.Response) -> None:
        """Warn once per endpoint marked deprecated via the RFC 8594 headers.

        sqi-server wraps retiring endpoints with middleware that sets
        ``Deprecation`` (the declaration date, or ``true``), optionally
        ``Sunset`` (planned removal date) and a ``Link`` with
        ``rel="deprecation"`` pointing at migration documentation.
        """
        if "Deprecation" not in response.headers:
            return
        path = response.request.url.path
        # Dedup on the endpoint template, not the concrete path: a deprecated
        # /jobs/{id} hit while iterating thousands of jobs must warn once. The
        # size cap is a backstop for path shapes the ID collapsing misses, so
        # the set cannot grow unboundedly over a long-lived client.
        key = _endpoint_key(path)
        if (
            key in self._deprecation_warned
            or len(self._deprecation_warned) >= _MAX_DEPRECATION_KEYS
        ):
            return
        self._deprecation_warned.add(key)

        message = f"sqi-server marks {path} as deprecated"
        sunset = response.headers.get("Sunset")
        if sunset:
            message += f" (sunset {sunset})"
        link = _deprecation_link(response.headers.get("Link"))
        if link:
            message += f"; see {link}"
        warnings.warn(message, stacklevel=2)

    def _warn_version_skew(self, response: httpx.Response) -> None:
        """Warn once per client instance when the server's API contract major
        (X-API-Version) is newer than this client supports, or unparseable."""
        if self._version_warned:
            return
        version = response.headers.get("X-API-Version")
        if not version:
            return

        message: str | None = None
        major = _major_version(version)
        if major is None:
            message = (
                f"sqi-server reported an unrecognized API version {version!r}; "
                "this sqi-sdk may be incompatible"
            )
        elif major > _SUPPORTED_API_MAJOR:
            message = (
                f"sqi-server API version {version} is newer than this sqi-sdk "
                f"supports (v{_SUPPORTED_API_MAJOR}.x); behavior may be unreliable "
                "— upgrade sqi-sdk"
            )

        if message is not None:
            warnings.warn(message, stacklevel=2)
            self._version_warned = True

    # ── Core transport ────────────────────────────────────────────────────────

    def _send(
        self,
        method: str,
        url: str,
        *,
        params: Mapping[str, Any] | None = None,
        allow_retry: bool = True,
        **kwargs: Any,
    ) -> httpx.Response:
        """Send a request with retry/backoff, returning the raw response.

        Does not raise for HTTP error statuses — only for transport failures
        (wrapped as :class:`SqiConnectionError` / :class:`SqiTimeoutError`).
        Retries (when ``allow_retry``) cover connect/timeout failures, 5xx, and
        429 responses; 429 honors ``Retry-After``.
        """
        method = method.upper()
        for attempt in range(1, self._max_attempts + 1):
            is_last = attempt == self._max_attempts
            start = time.monotonic()
            try:
                response = self._http.request(method, url, params=params, **kwargs)
            except httpx.TimeoutException as exc:
                if allow_retry and not is_last:
                    self._log_retry(method, url, attempt, "timeout")
                    time.sleep(self._backoff_delay(attempt, None))
                    continue
                raise SqiTimeoutError(f"request to {method} {url} timed out") from exc
            except httpx.TransportError as exc:
                if allow_retry and not is_last:
                    self._log_retry(method, url, attempt, str(exc) or "connection error")
                    time.sleep(self._backoff_delay(attempt, None))
                    continue
                raise SqiConnectionError(f"could not connect to {method} {url}: {exc}") from exc

            duration_ms = (time.monotonic() - start) * 1000
            self._check_version_headers(response)
            logger.debug("%s %s -> %d (%.1f ms)", method, url, response.status_code, duration_ms)

            if allow_retry and not is_last and _is_retryable_status(response.status_code):
                retry_after = (
                    _parse_retry_after(response.headers.get("Retry-After"))
                    if response.status_code == 429
                    else None
                )
                self._log_retry(method, url, attempt, f"status {response.status_code}")
                time.sleep(self._backoff_delay(attempt, retry_after))
                continue

            return response

        # range(1, n+1) with n >= 1 always yields at least once and every path
        # above returns or raises, so this is unreachable; it satisfies the type
        # checker that the method never implicitly returns None.
        raise SqiError("request retry loop exhausted without returning")

    def _request(
        self,
        method: str,
        path: str,
        *,
        api: bool = True,
        params: Mapping[str, Any] | None = None,
        **kwargs: Any,
    ) -> httpx.Response:
        """Send a request under the API prefix and raise on any non-2xx status.

        ``None``-valued query parameters are dropped so optional filters never
        appear in the URL. Only GET requests are retried (idempotent).
        """
        url = self._build_url(path, api=api)
        clean = _drop_none(params) if params is not None else None
        allow_retry = method.upper() == "GET"
        response = self._send(method, url, params=clean, allow_retry=allow_retry, **kwargs)
        if response.is_success:
            return response
        raise api_error_from_response(response)

    def _request_json(self, method: str, path: str, **kwargs: Any) -> Any:
        """Like :meth:`_request` but return the decoded JSON body (or ``None``)."""
        response = self._request(method, path, **kwargs)
        if response.status_code == 204 or not response.content:
            return None
        return response.json()

    # ── Jobs ──────────────────────────────────────────────────────────────────

    def submit_job(
        self,
        template: JobTemplate,
        *,
        farm_id: str,
        queue_id: str,
        owner: str | None = None,
        priority: int | None = None,
        project: str | None = None,
        max_attempts: int | None = None,
        retry_delay_seconds: int | None = None,
        failure_limit: int | None = None,
        depends_on: list[str] | None = None,
    ) -> Job:
        """Submit a raw OpenJD job template and return the created :class:`Job`.

        Args:
            template: The OpenJD job template, as one of:

                * a ``str`` — sent to the server verbatim (YAML or JSON text);
                * a :class:`pathlib.Path` — the file is read and its contents
                  sent;
                * a ``dict`` — serialized to JSON before sending.

                The ``Content-Type`` is selected automatically: ``application/json``
                for dicts and for string/file content that parses as JSON,
                ``application/x-yaml`` otherwise (and always for ``.yaml``/``.yml``
                file paths). Serializing a dict needs only the standard library —
                the optional ``yaml`` extra is not required to submit.
            farm_id: Target farm (required query parameter).
            queue_id: Target queue (required query parameter).
            owner: Optional human-readable owner label.
            priority: Optional scheduling priority (higher runs sooner); the
                server defaults to 50 when omitted.
            project: Optional project label for later filtering.
            max_attempts: Per-job override for the maximum attempts per task.
                Omitted means inherit (queue -> farm -> server default). Not to
                be confused with the client constructor's transport-level
                ``max_attempts`` (HTTP retry count) or :meth:`retry_job`/
                :meth:`retry_task` (manual one-off retries) — this is the
                server-side automatic retry policy for the job's tasks.
            retry_delay_seconds: Per-job override for the delay before retrying
                a failed task. Omitted means inherit (queue -> farm -> server
                default).
            failure_limit: Per-job override for the number of task failures
                that auto-parks the job. Omitted means inherit (queue -> farm
                -> server default).
            depends_on: IDs of upstream jobs this job must wait for (whole-job
                cross-job dependencies). Each must already exist and be in the
                same farm; none may already be failed or canceled. When any is
                not yet completed the created job is returned in the ``blocked``
                status and starts only once every upstream completes.

        Returns:
            The created :class:`Job`, parsed from the ``201 Created`` body.

        Raises:
            ValidationError: The template failed server-side OpenJD validation
                (HTTP 422); the server's ``detail`` (e.g. ``"step 'Render'
                references undefined parameter 'Frames'"``) is preserved verbatim
                on the exception.
            TypeError: ``template`` is not a ``str``, ``Path``, or ``dict``.
            ValueError: ``template`` is a ``dict`` containing values that cannot
                be serialized to JSON.
        """
        body, content_type = _prepare_template(template)
        params: dict[str, Any] = {
            "farm_id": farm_id,
            "queue_id": queue_id,
            "owner": owner,
            "priority": priority,
            "project": project,
            "max_attempts": max_attempts,
            "retry_delay_seconds": retry_delay_seconds,
            "failure_limit": failure_limit,
            "depends_on": depends_on,
        }
        data = self._request_json(
            "POST",
            "/jobs",
            params=params,
            content=body,
            headers={"Content-Type": content_type},
        )
        return Job.from_dict(data)

    def list_jobs(
        self,
        *,
        status: JobStatus | str | None = None,
        farm_id: str | None = None,
        queue_id: str | None = None,
        owner: str | None = None,
        project: str | None = None,
        sort_by: str | None = None,
        sort_dir: str | None = None,
        limit: int = 50,
        offset: int = 0,
    ) -> Page[Job]:
        """List jobs, returning one page of results.

        Args:
            status: Filter by job status; accepts a :class:`JobStatus` or its
                plain wire string (e.g. ``"running"``).
            farm_id: Filter by farm.
            queue_id: Filter by queue.
            owner: Filter by owner label.
            project: Filter by project label.
            sort_by: Sort field — one of ``created_at``, ``priority``,
                ``status``, ``updated_at``, ``name`` (server default
                ``created_at``).
            sort_dir: Sort direction, ``asc`` or ``desc`` (server default
                ``asc``).
            limit: Page size, 1-1000 (default 50).
            offset: Zero-based offset of the page (default 0).

        Returns:
            A :class:`Page` of :class:`Job` for the requested window. Use
            :meth:`iter_jobs` to walk every page automatically.
        """
        data = self._request_json(
            "GET",
            "/jobs",
            params=self._job_list_params(
                status=status,
                farm_id=farm_id,
                queue_id=queue_id,
                owner=owner,
                project=project,
                sort_by=sort_by,
                sort_dir=sort_dir,
                limit=limit,
                offset=offset,
            ),
        )
        return parse_page(data, Job.from_dict)

    def iter_jobs(
        self,
        *,
        status: JobStatus | str | None = None,
        farm_id: str | None = None,
        queue_id: str | None = None,
        owner: str | None = None,
        project: str | None = None,
        sort_by: str | None = None,
        sort_dir: str | None = None,
        limit: int = 50,
    ) -> Iterator[Job]:
        """Iterate every job matching the filters, fetching pages lazily.

        Accepts the same filters as :meth:`list_jobs`; ``limit`` is the page
        size fetched per request. Pages are walked from the start of the result
        set until it is exhausted.

        Yields:
            Each :class:`Job`, in the server's order, across page boundaries.
        """

        def fetch(page_offset: int, page_limit: int) -> Page[Job]:
            return self.list_jobs(
                status=status,
                farm_id=farm_id,
                queue_id=queue_id,
                owner=owner,
                project=project,
                sort_by=sort_by,
                sort_dir=sort_dir,
                limit=page_limit,
                offset=page_offset,
            )

        return iter_pages(fetch, limit=limit)

    def get_job(self, job_id: str) -> Job:
        """Fetch one job by ID, with ``steps`` and ``task_counts`` populated.

        Raises:
            NotFoundError: No job with that ID exists (HTTP 404).
        """
        data = self._request_json("GET", f"/jobs/{quote(job_id, safe='')}")
        return Job.from_dict(data)

    def pause_job(self, job_id: str) -> Job:
        """Pause a job and return its updated state.

        Raises:
            NotFoundError: No job with that ID exists (HTTP 404).
            ConflictError: The job is in a state that cannot be paused (HTTP 409).
        """
        return self._patch_job(job_id, {"action": "pause"})

    def resume_job(self, job_id: str) -> Job:
        """Resume a paused job and return its updated state.

        Raises:
            NotFoundError: No job with that ID exists (HTTP 404).
            ConflictError: The job is in a state that cannot be resumed (HTTP 409).
        """
        return self._patch_job(job_id, {"action": "resume"})

    def set_job_priority(self, job_id: str, priority: int) -> Job:
        """Set a job's scheduling priority and return its updated state.

        ``priority`` is validated client-side against the minimum the server
        accepts (``>= 1``) before any request is sent.

        Raises:
            ValueError: ``priority`` is below the minimum (1).
            NotFoundError: No job with that ID exists (HTTP 404).
            ConflictError: The job is terminal and cannot be modified (HTTP 409).
        """
        if priority < _MIN_JOB_PRIORITY:
            raise ValueError(f"priority must be >= {_MIN_JOB_PRIORITY}, got {priority}")
        return self._patch_job(job_id, {"priority": priority})

    def cancel_job(self, job_id: str) -> None:
        """Cancel a job. Returns ``None`` on success (HTTP 204).

        Raises:
            NotFoundError: No job with that ID exists (HTTP 404).
            ConflictError: The job has already completed or failed and cannot be
                canceled (HTTP 409). Canceling an already-canceled job succeeds.
        """
        self._request("POST", f"/jobs/{quote(job_id, safe='')}/cancel")

    def retry_job(self, job_id: str) -> RetryJobResult:
        """Retry a job's failed and canceled tasks, reviving them for re-dispatch.

        Resets every ``failed``/``canceled`` task in the job — together with the
        job and the affected steps — back to ``pending`` so the scheduler
        re-runs them in dependency order. Idempotent: a job with no eligible
        tasks returns ``retried=0``.

        Returns:
            A :class:`RetryJobResult` with the job ID and the number of tasks
            revived.

        Raises:
            NotFoundError: No job with that ID exists (HTTP 404).
        """
        data = self._request_json("POST", f"/jobs/{quote(job_id, safe='')}/retry")
        return RetryJobResult.from_dict(data)

    def delete_job(self, job_id: str) -> None:
        """Hard-delete a job and all of its data. Returns ``None`` on success
        (HTTP 204).

        If the job is still active its tasks are canceled immediately before the
        job and all associated data (steps, tasks, attempts, logs) are removed.
        Deleting a job is always permitted regardless of state.

        Raises:
            NotFoundError: No job with that ID exists (HTTP 404).
        """
        self._request("DELETE", f"/jobs/{quote(job_id, safe='')}")

    def _patch_job(self, job_id: str, body: Mapping[str, Any]) -> Job:
        data = self._request_json("PATCH", f"/jobs/{quote(job_id, safe='')}", json=body)
        return Job.from_dict(data)

    @staticmethod
    def _job_list_params(
        *,
        status: JobStatus | str | None,
        farm_id: str | None,
        queue_id: str | None,
        owner: str | None,
        project: str | None,
        sort_by: str | None,
        sort_dir: str | None,
        limit: int,
        offset: int,
    ) -> dict[str, Any]:
        return {
            "status": _enum_value(status),
            "farm_id": farm_id,
            "queue_id": queue_id,
            "owner": owner,
            "project": project,
            "sort_by": sort_by,
            "sort_dir": sort_dir,
            "limit": limit,
            "offset": offset,
        }

    # ── Tasks ─────────────────────────────────────────────────────────────────

    def list_job_tasks(
        self,
        job_id: str,
        *,
        status: TaskStatus | str | None = None,
        sort_by: str | None = None,
        sort_dir: str | None = None,
        limit: int = 50,
        offset: int = 0,
    ) -> Page[Task]:
        """List the tasks of a job, returning one page of results.

        Args:
            job_id: The job whose tasks to list.
            status: Filter by task status; accepts a :class:`TaskStatus` or its
                plain wire string.
            sort_by: Sort field — ``created_at``, ``status``, ``updated_at``, or
                ``name`` (server default ``created_at``).
            sort_dir: Sort direction, ``asc`` or ``desc``.
            limit: Page size, 1-1000 (default 50).
            offset: Zero-based offset of the page (default 0).

        Returns:
            A :class:`Page` of :class:`Task`. Use :meth:`iter_job_tasks` to walk
            every page automatically.

        Raises:
            NotFoundError: No job with that ID exists (HTTP 404).
        """
        data = self._request_json(
            "GET",
            f"/jobs/{quote(job_id, safe='')}/tasks",
            params={
                "status": _enum_value(status),
                "sort_by": sort_by,
                "sort_dir": sort_dir,
                "limit": limit,
                "offset": offset,
            },
        )
        return parse_page(data, Task.from_dict)

    def iter_job_tasks(
        self,
        job_id: str,
        *,
        status: TaskStatus | str | None = None,
        sort_by: str | None = None,
        sort_dir: str | None = None,
        limit: int = 50,
    ) -> Iterator[Task]:
        """Iterate every task of a job, fetching pages lazily.

        Accepts the same filters as :meth:`list_job_tasks`; ``limit`` is the page
        size fetched per request.

        Yields:
            Each :class:`Task`, in the server's order, across page boundaries.
        """

        def fetch(page_offset: int, page_limit: int) -> Page[Task]:
            return self.list_job_tasks(
                job_id,
                status=status,
                sort_by=sort_by,
                sort_dir=sort_dir,
                limit=page_limit,
                offset=page_offset,
            )

        return iter_pages(fetch, limit=limit)

    def get_task(self, task_id: str) -> Task:
        """Fetch one task by ID.

        Raises:
            NotFoundError: No task with that ID exists (HTTP 404).
        """
        data = self._request_json("GET", f"/tasks/{quote(task_id, safe='')}")
        return Task.from_dict(data)

    def retry_task(self, task_id: str) -> RetryResult:
        """Retry a failed or canceled task, resetting it for re-dispatch.

        Returns:
            A :class:`RetryResult` describing the retried task and its new status
            (``ready`` when the task's step dependencies are satisfied, otherwise
            ``pending``).

        Raises:
            NotFoundError: No task with that ID exists (HTTP 404).
            ConflictError: The task is not in a ``failed``/``canceled`` state and
                so cannot be retried (HTTP 409).
        """
        data = self._request_json("POST", f"/tasks/{quote(task_id, safe='')}/retry")
        return RetryResult.from_dict(data)

    def cancel_task(self, task_id: str) -> CancelResult:
        """Cancel a single non-terminal task, signaling its assigned worker.

        Returns:
            A :class:`CancelResult` describing the canceled task and its new
            status (``canceled``).

        Raises:
            NotFoundError: No task with that ID exists (HTTP 404).
            ConflictError: The task is already terminal (succeeded/failed/
                canceled) and so cannot be canceled (HTTP 409).
        """
        data = self._request_json("POST", f"/tasks/{quote(task_id, safe='')}/cancel")
        return CancelResult.from_dict(data)

    def get_task_logs(
        self,
        task_id: str,
        limit: int = 100,
        after_nats_seq: int = 0,
    ) -> LogPage:
        """Fetch one page of a task's log chunks.

        Args:
            task_id: The task whose logs to fetch (latest attempt).
            limit: Maximum chunks per page, 1-1000 (default 100).
            after_nats_seq: Return only chunks with ``nats_seq`` greater than
                this cursor (default 0 — from the beginning).

        Returns:
            A :class:`LogPage` whose ``after_nats_seq`` is the cursor to pass on
            the next call to fetch newer chunks. When a task has no attempt yet
            the page is empty rather than a 404, so pollers need no special case.
        """
        data = self._request_json(
            "GET",
            f"/tasks/{quote(task_id, safe='')}/logs",
            params={"limit": limit, "after_nats_seq": after_nats_seq},
        )
        return LogPage.from_dict(data)

    def tail_task_logs(
        self,
        task_id: str,
        poll_interval: float = 1.0,
        from_seq: int = 0,
        follow: bool = True,
    ) -> Iterator[LogChunk]:
        """Tail a task's logs, yielding each chunk in order as it appears.

        Repeatedly polls :meth:`get_task_logs`, advancing an internal cursor by
        the ``nats_seq`` of the chunks it yields so no chunk is repeated or
        skipped. Between empty polls it sleeps ``poll_interval`` seconds.

        Args:
            task_id: The task whose logs to tail.
            poll_interval: Seconds to wait between polls once caught up to the
                end of the log (default 1.0).
            from_seq: Cursor to start after — ``0`` tails from the beginning.
            follow: When ``True`` (default), keep polling until the task reaches
                a terminal state (``succeeded``/``failed``/``canceled``) and the
                final chunks have been drained, then stop — never spinning on a
                finished task. When ``False``, stop as soon as the current end of
                the log is reached.

        Yields:
            Each :class:`LogChunk`, ordered by ``nats_seq``.
        """
        cursor = from_seq
        # Set once a terminal status is observed after an empty poll; the next
        # empty poll then confirms everything is drained and stops the loop.
        pending_terminal = False
        while True:
            page = self.get_task_logs(task_id, after_nats_seq=cursor)
            for chunk in page.items:
                yield chunk
                # Advance by the chunk's own seq, not the response's echoed
                # cursor, so tailing is correct regardless of the server's
                # after_nats_seq semantics.
                cursor = max(cursor, chunk.nats_seq)
            if page.items:
                pending_terminal = False
                continue
            # Empty poll: caught up to the current end of the log.
            if not follow:
                return
            if pending_terminal:
                return
            if self._task_is_terminal(task_id):
                # Poll once more to drain anything written between this empty
                # poll and the terminal transition, then stop.
                pending_terminal = True
                continue
            time.sleep(poll_interval)

    def _task_is_terminal(self, task_id: str) -> bool:
        return self.get_task(task_id).status in _TERMINAL_TASK_STATUSES

    def list_task_attempts(self, task_id: str) -> list[TaskAttempt]:
        """List a task's execution attempts, oldest first
        (``GET /tasks/{id}/attempts``).

        Raises:
            NotFoundError: No task with that ID exists (HTTP 404).
        """
        data = self._request_json("GET", f"/tasks/{quote(task_id, safe='')}/attempts")
        return [TaskAttempt.from_dict(item) for item in data.get("items", [])]

    # ── Workers ───────────────────────────────────────────────────────────────

    def list_workers(
        self,
        *,
        farm_id: str | None = None,
        queue_id: str | None = None,
        compute_location: str | None = None,
        status: WorkerStatus | str | None = None,
        sort_by: str | None = None,
        sort_dir: str | None = None,
        limit: int = 50,
        offset: int = 0,
    ) -> Page[Worker]:
        """List workers, returning one page of results.

        Args:
            farm_id: Filter by farm.
            queue_id: Filter by queue.
            compute_location: Filter by named compute location (e.g.
                ``on-prem``, ``aws-us-east-1``).
            status: Filter by worker status; accepts a :class:`WorkerStatus` or
                its plain wire string (``online``/``offline``/``disabled``).
            sort_by: Sort field — ``hostname``, ``status``, ``registered_at``, or
                ``last_heartbeat_at`` (server default ``hostname``).
            sort_dir: Sort direction, ``asc`` or ``desc``.
            limit: Page size, 1-1000 (default 50).
            offset: Zero-based offset of the page (default 0).

        Returns:
            A :class:`Page` of :class:`Worker`. Use :meth:`iter_workers` to walk
            every page automatically.
        """
        data = self._request_json(
            "GET",
            "/workers",
            params={
                "farm_id": farm_id,
                "queue_id": queue_id,
                "compute_location": compute_location,
                "status": _enum_value(status),
                "sort_by": sort_by,
                "sort_dir": sort_dir,
                "limit": limit,
                "offset": offset,
            },
        )
        return parse_page(data, Worker.from_dict)

    def iter_workers(
        self,
        *,
        farm_id: str | None = None,
        queue_id: str | None = None,
        compute_location: str | None = None,
        status: WorkerStatus | str | None = None,
        sort_by: str | None = None,
        sort_dir: str | None = None,
        limit: int = 50,
    ) -> Iterator[Worker]:
        """Iterate every worker matching the filters, fetching pages lazily.

        Accepts the same filters as :meth:`list_workers`; ``limit`` is the page
        size fetched per request.

        Yields:
            Each :class:`Worker`, in the server's order, across page boundaries.
        """

        def fetch(page_offset: int, page_limit: int) -> Page[Worker]:
            return self.list_workers(
                farm_id=farm_id,
                queue_id=queue_id,
                compute_location=compute_location,
                status=status,
                sort_by=sort_by,
                sort_dir=sort_dir,
                limit=page_limit,
                offset=page_offset,
            )

        return iter_pages(fetch, limit=limit)

    def get_worker(self, worker_id: str) -> Worker:
        """Fetch one worker by ID, including its currently-executing task if any.

        Raises:
            NotFoundError: No worker with that ID exists (HTTP 404).
        """
        data = self._request_json("GET", f"/workers/{quote(worker_id, safe='')}")
        return Worker.from_dict(data)

    def disable_worker(self, worker_id: str) -> WorkerAction | None:
        """Disable a worker so it receives no new task assignments.

        Idempotent: a worker already disabled is accepted unchanged.

        Returns:
            A :class:`WorkerAction` with the worker's status after the operation
            when the server returns a body, or ``None`` on a bare success status.
            The server currently always returns a body, so ``None`` is a
            defensive should-not-happen path.

        Raises:
            NotFoundError: No worker with that ID exists (HTTP 404).
        """
        return self._worker_action(worker_id, "disable")

    def enable_worker(self, worker_id: str) -> WorkerAction | None:
        """Re-enable a disabled worker.

        Idempotent: an online or offline worker is accepted unchanged.

        Returns:
            A :class:`WorkerAction` with the worker's status after the operation
            when the server returns a body, or ``None`` on a bare success status.
            The server currently always returns a body, so ``None`` is a
            defensive should-not-happen path.

        Raises:
            NotFoundError: No worker with that ID exists (HTTP 404).
        """
        return self._worker_action(worker_id, "enable")

    def _worker_action(self, worker_id: str, action: str) -> WorkerAction | None:
        data = self._request_json("POST", f"/workers/{quote(worker_id, safe='')}/{action}")
        return WorkerAction.from_dict(data) if data is not None else None

    def remove_worker(self, worker_id: str) -> None:
        """Hard-delete a worker. Returns ``None`` on success (HTTP 204).

        Only removable workers are accepted: offline workers, or disabled
        workers whose last heartbeat is older than the heartbeat-timeout window
        (the machine is gone). Check :attr:`Worker.removable` first to avoid a
        ``ConflictError``. Task and attempt history referencing the worker is
        preserved by ID; a removed worker that reconnects simply re-registers.

        Raises:
            NotFoundError: No worker with that ID exists (HTTP 404).
            ConflictError: The worker is not removable — it is online or a
                still-heartbeating disabled worker (HTTP 409).
        """
        self._request("DELETE", f"/workers/{quote(worker_id, safe='')}")

    # ── Farms ─────────────────────────────────────────────────────────────────

    def create_farm(
        self,
        *,
        name: str,
        description: str | None = None,
        max_concurrent_tasks: int = 0,
        max_attempts: int | None = None,
        retry_delay_seconds: int | None = None,
        failure_limit: int | None = None,
    ) -> Farm:
        """Create a farm and return it.

        Args:
            name: Farm name.
            description: Optional human-readable description.
            max_concurrent_tasks: Cap on concurrently-running tasks; ``0`` means
                unlimited (the default).
            max_attempts: Farm-level max-attempts retry-policy override.
                Omitted means inherit (server default). Distinct from the
                client constructor's transport-level ``max_attempts`` and from
                :meth:`retry_job`/:meth:`retry_task`.
            retry_delay_seconds: Farm-level retry-delay retry-policy override.
                Omitted means inherit (server default).
            failure_limit: Farm-level failure-limit retry-policy override.
                Omitted means inherit (server default).
        """
        return self._farms.create(
            _farm_body(
                name,
                description,
                max_concurrent_tasks,
                max_attempts,
                retry_delay_seconds,
                failure_limit,
            )
        )

    def list_farms(self) -> list[Farm]:
        """Return all farms. The server returns a bare array (no pagination)."""
        return self._farms.list_all()

    def iter_farms(self) -> Iterator[Farm]:
        """Iterate all farms (the farms endpoint is not paginated)."""
        return iter(self._farms.list_all())

    def get_farm(self, farm_id: str) -> Farm:
        """Fetch one farm by ID. Raises :class:`NotFoundError` if it is missing."""
        return self._farms.get(farm_id)

    def update_farm(
        self,
        farm_id: str,
        *,
        name: str,
        description: str | None = None,
        max_concurrent_tasks: int = 0,
        max_attempts: int | None = None,
        retry_delay_seconds: int | None = None,
        failure_limit: int | None = None,
    ) -> Farm:
        """Replace a farm's mutable fields (PUT, full replacement) and return it.

        Per the server's PUT semantics this is a full replace: a field left at
        its default (e.g. an omitted ``description``) is cleared, not preserved.
        This applies to the retry-policy overrides too: an omitted
        ``max_attempts``/``retry_delay_seconds``/``failure_limit`` is cleared
        back to "inherit", not left unchanged.
        """
        return self._farms.update(
            farm_id,
            _farm_body(
                name,
                description,
                max_concurrent_tasks,
                max_attempts,
                retry_delay_seconds,
                failure_limit,
            ),
        )

    def delete_farm(self, farm_id: str) -> None:
        """Delete a farm. Raises :class:`NotFoundError` if it does not exist."""
        self._farms.delete(farm_id)

    # ── Queues ────────────────────────────────────────────────────────────────

    def create_queue(
        self,
        *,
        farm_id: str,
        name: str,
        description: str | None = None,
        priority: int = 0,
        max_concurrent_tasks: int = 0,
        paused: bool = False,
        max_attempts: int | None = None,
        retry_delay_seconds: int | None = None,
        failure_limit: int | None = None,
    ) -> Queue:
        """Create a queue within a farm and return it.

        Args:
            farm_id: The farm to create the queue in (required).
            name: Queue name.
            description: Optional description.
            priority: Scheduling priority within the farm (higher runs sooner).
            max_concurrent_tasks: Cap on concurrent tasks; ``0`` means unlimited.
            paused: Whether the queue starts paused.
            max_attempts: Queue-level max-attempts retry-policy override.
                Omitted means inherit (farm -> server default). Distinct from
                the client constructor's transport-level ``max_attempts`` and
                from :meth:`retry_job`/:meth:`retry_task`.
            retry_delay_seconds: Queue-level retry-delay retry-policy override.
                Omitted means inherit (farm -> server default).
            failure_limit: Queue-level failure-limit retry-policy override.
                Omitted means inherit (farm -> server default).
        """
        return self._queues.create(
            _queue_body(
                farm_id,
                name,
                description,
                priority,
                max_concurrent_tasks,
                paused,
                max_attempts,
                retry_delay_seconds,
                failure_limit,
            )
        )

    def list_queues(
        self,
        *,
        farm_id: str | None = None,
        paused: bool | None = None,
        sort_by: str | None = None,
        sort_dir: str | None = None,
        limit: int = 50,
        offset: int = 0,
    ) -> Page[Queue]:
        """List queues (paginated), optionally filtered.

        Args:
            farm_id: Filter by farm.
            paused: Filter by paused state.
            sort_by: Sort field — ``name``, ``priority``, or ``created_at``
                (server default ``name``).
            sort_dir: Sort direction, ``asc`` or ``desc``.
            limit: Page size, 1-1000 (default 50).
            offset: Zero-based offset of the page (default 0).
        """
        return self._queues.list_page(
            {
                "farm_id": farm_id,
                "paused": paused,
                "sort_by": sort_by,
                "sort_dir": sort_dir,
                "limit": limit,
                "offset": offset,
            }
        )

    def iter_queues(
        self,
        *,
        farm_id: str | None = None,
        paused: bool | None = None,
        sort_by: str | None = None,
        sort_dir: str | None = None,
        limit: int = 50,
    ) -> Iterator[Queue]:
        """Iterate every queue matching the filters, fetching pages lazily."""

        def fetch(page_offset: int, page_limit: int) -> Page[Queue]:
            return self.list_queues(
                farm_id=farm_id,
                paused=paused,
                sort_by=sort_by,
                sort_dir=sort_dir,
                limit=page_limit,
                offset=page_offset,
            )

        return iter_pages(fetch, limit=limit)

    def get_queue(self, queue_id: str) -> Queue:
        """Fetch one queue by ID. Raises :class:`NotFoundError` if it is missing."""
        return self._queues.get(queue_id)

    def update_queue(
        self,
        queue_id: str,
        *,
        farm_id: str,
        name: str,
        description: str | None = None,
        priority: int = 0,
        max_concurrent_tasks: int = 0,
        paused: bool = False,
        max_attempts: int | None = None,
        retry_delay_seconds: int | None = None,
        failure_limit: int | None = None,
    ) -> Queue:
        """Replace a queue's mutable fields (PUT, full replacement) and return it.

        Full replace: any field left at its default (e.g. an omitted
        ``description``, or ``priority``/``max_concurrent_tasks``/``paused`` not
        passed) is reset, not preserved — pass every field you want to keep.
        This applies to the retry-policy overrides too: an omitted
        ``max_attempts``/``retry_delay_seconds``/``failure_limit`` is cleared
        back to "inherit", not left unchanged.
        """
        return self._queues.update(
            queue_id,
            _queue_body(
                farm_id,
                name,
                description,
                priority,
                max_concurrent_tasks,
                paused,
                max_attempts,
                retry_delay_seconds,
                failure_limit,
            ),
        )

    def delete_queue(self, queue_id: str) -> None:
        """Delete a queue. Raises :class:`NotFoundError` if it does not exist."""
        self._queues.delete(queue_id)

    # ── Storage locations ─────────────────────────────────────────────────────

    def create_storage_location(
        self,
        *,
        name: str,
        description: str | None = None,
        roots: Mapping[str, str] | None = None,
    ) -> StorageLocation:
        """Create a storage location and return it.

        Args:
            name: Storage-location name.
            description: Optional description.
            roots: Map of compute-location name to absolute root path for that
                location (e.g. ``{"on-prem": "/mnt/farm"}``). The storage
                ``type`` is derived by the server from the roots.
        """
        return self._storage_locations.create(_storage_body(name, description, roots))

    def list_storage_locations(self) -> list[StorageLocation]:
        """Return all storage locations (bare array, no pagination)."""
        return self._storage_locations.list_all()

    def iter_storage_locations(self) -> Iterator[StorageLocation]:
        """Iterate all storage locations (endpoint is not paginated)."""
        return iter(self._storage_locations.list_all())

    def get_storage_location(self, location_id: str) -> StorageLocation:
        """Fetch one storage location by ID. Raises :class:`NotFoundError` if missing."""
        return self._storage_locations.get(location_id)

    def update_storage_location(
        self,
        location_id: str,
        *,
        name: str,
        description: str | None = None,
        roots: Mapping[str, str] | None = None,
    ) -> StorageLocation:
        """Replace a storage location's fields (PUT, full replacement) and return it.

        Full replace: an omitted ``description`` or ``roots`` is cleared, not
        preserved — pass every field you want to keep.
        """
        return self._storage_locations.update(location_id, _storage_body(name, description, roots))

    def delete_storage_location(self, location_id: str) -> None:
        """Delete a storage location. Raises :class:`NotFoundError` if it is missing."""
        self._storage_locations.delete(location_id)

    # ── Compute locations ─────────────────────────────────────────────────────

    def create_compute_location(
        self,
        *,
        name: str,
        description: str | None = None,
    ) -> ComputeLocation:
        """Create a compute location and return it.

        Args:
            name: Compute-location name.
            description: Optional description.
        """
        return self._compute_locations.create(_compute_location_body(name, description))

    def list_compute_locations(self) -> list[ComputeLocation]:
        """Return all compute locations (bare array, no pagination)."""
        return self._compute_locations.list_all()

    def iter_compute_locations(self) -> Iterator[ComputeLocation]:
        """Iterate all compute locations (endpoint is not paginated)."""
        return iter(self._compute_locations.list_all())

    def get_compute_location(self, location_id: str) -> ComputeLocation:
        """Fetch one compute location by ID. Raises :class:`NotFoundError` if missing."""
        return self._compute_locations.get(location_id)

    def update_compute_location(
        self,
        location_id: str,
        *,
        name: str,
        description: str | None = None,
    ) -> ComputeLocation:
        """Replace a compute location's fields (PUT, full replacement) and return it.

        Args:
            location_id: The compute location to update.
            name: New name for the compute location (required — the server
                rejects a PUT with an empty or missing name).
            description: Optional description; omit to clear it.
        """
        return self._compute_locations.update(
            location_id, _compute_location_body(name, description)
        )

    def delete_compute_location(self, location_id: str) -> None:
        """Delete a compute location. Raises :class:`NotFoundError` if it is missing."""
        self._compute_locations.delete(location_id)

    # ── Usage pools ───────────────────────────────────────────────────────────

    def create_usage_pool(
        self,
        *,
        name: str,
        max_concurrent: int,
        server_hint: str | None = None,
    ) -> UsagePool:
        """Create a usage pool and return it.

        Args:
            name: Pool name (referenced from job templates as
                ``amount.worker.usagepool.<name>``).
            max_concurrent: Maximum simultaneous claims from this pool
                (must be at least 1; validated client-side before sending).
            server_hint: Optional license-server address hint (informational).

        Raises:
            ValueError: ``max_concurrent`` is below the minimum (1).
        """
        return self._usage_pools.create(_usage_body(name, max_concurrent, server_hint))

    def list_usage_pools(self) -> list[UsagePool]:
        """Return all usage pools (bare array, no pagination)."""
        return self._usage_pools.list_all()

    def iter_usage_pools(self) -> Iterator[UsagePool]:
        """Iterate all usage pools (endpoint is not paginated)."""
        return iter(self._usage_pools.list_all())

    def get_usage_pool(self, pool_id: str) -> UsagePool:
        """Fetch one usage pool by ID. Raises :class:`NotFoundError` if missing."""
        return self._usage_pools.get(pool_id)

    def update_usage_pool(
        self,
        pool_id: str,
        *,
        name: str,
        max_concurrent: int,
        server_hint: str | None = None,
    ) -> UsagePool:
        """Replace a usage pool's fields (PUT, full replacement) and return it.

        Full replace: an omitted ``server_hint`` is cleared, not preserved.

        Raises:
            ValueError: ``max_concurrent`` is below the minimum (1).
        """
        return self._usage_pools.update(pool_id, _usage_body(name, max_concurrent, server_hint))

    def delete_usage_pool(self, pool_id: str) -> None:
        """Delete a usage pool. Raises :class:`NotFoundError` if it is missing."""
        self._usage_pools.delete(pool_id)

    # ── Products ──────────────────────────────────────────────────────────────

    def list_products(self) -> list[Product]:
        """Return all products (built-ins + stored), bare array, no pagination."""
        return self._products.list_all()

    def iter_products(self) -> Iterator[Product]:
        """Iterate all products (endpoint is not paginated)."""
        return iter(self._products.list_all())

    def get_product(self, name: str) -> Product:
        """Fetch one product by name. Raises :class:`NotFoundError` if missing."""
        return self._products.get(name)

    def create_product(
        self,
        *,
        name: str,
        template: str,
        format: str,
        title: str | None = None,
        description: str | None = None,
        category: str | None = None,
        version: str | None = None,
    ) -> Product:
        """Create a custom product from a raw OpenJD template and return it.

        Args:
            name: Stable product name (slug).
            template: Raw OpenJD template text.
            format: ``yaml`` or ``json``.
            title, description, category, version: Optional metadata.
        """
        return self._products.create(
            _product_body(name, title, description, category, version, template, format)
        )

    def update_product(
        self,
        name: str,
        *,
        template: str,
        format: str,
        title: str | None = None,
        description: str | None = None,
        category: str | None = None,
        version: str | None = None,
    ) -> Product:
        """Replace a custom product's fields (PUT, full replacement) and return it."""
        return self._products.update(
            name, _product_body(name, title, description, category, version, template, format)
        )

    def delete_product(self, name: str) -> None:
        """Delete a custom product. Raises :class:`NotFoundError` if missing."""
        self._products.delete(name)

    def get_product_parameters(self, name: str) -> list[ProductParameter]:
        """Return the parsed job parameters (with UI hints) for a product.

        Raises:
            NotFoundError: No such product (HTTP 404).
            ValidationError: The product's stored template is unparseable (HTTP 422).
        """
        data = self._request_json("GET", f"/products/{name}/parameters")
        return [ProductParameter.from_dict(item) for item in data]

    def submit_product_job(
        self,
        name: str,
        *,
        farm_id: str,
        queue_id: str,
        job_name: str | None = None,
        owner: str | None = None,
        submitter: str | None = None,
        priority: int | None = None,
        project: str | None = None,
        parameters: Mapping[str, str] | None = None,
        max_attempts: int | None = None,
        retry_delay_seconds: int | None = None,
        failure_limit: int | None = None,
    ) -> Job:
        """Submit a job from a product and return the created :class:`Job`.

        Args:
            name: Product name to submit.
            farm_id, queue_id: Target farm and queue (required).
            job_name: Optional job name; overrides the template's name when set.
                The wire field is ``name``; this parameter is named ``job_name``
                to avoid shadowing the positional product ``name`` argument.
            owner, submitter, priority, project: Optional job metadata.
            parameters: Job-parameter values (name → string).
            max_attempts, retry_delay_seconds, failure_limit: Per-job retry
                overrides; ``None`` means inherit.
        """
        body: dict[str, Any] = {"farm_id": farm_id, "queue_id": queue_id}
        if job_name is not None:
            body["name"] = job_name
        if owner is not None:
            body["owner"] = owner
        if submitter is not None:
            body["submitter"] = submitter
        if priority is not None:
            body["priority"] = priority
        if project is not None:
            body["project"] = project
        if parameters is not None:
            body["parameters"] = dict(parameters)
        _add_retry_policy(body, max_attempts, retry_delay_seconds, failure_limit)
        data = self._request_json("POST", f"/products/{name}/jobs", json=body)
        return Job.from_dict(data)

    # ── Live events (optional ws extra) ───────────────────────────────────────

    def events(self, *, reconnect: bool = True) -> SqiEventStream:
        """Open a live WebSocket event stream for this server.

        Requires the optional ``ws`` extra (``pip install 'sqi-sdk[ws]'``);
        the import happens when the returned stream connects, so calling this
        without the extra installed only fails on connection, with an actionable
        :class:`ImportError`.

        The stream is not yet connected — use it as a context manager::

            with client.events() as stream:
                stream.subscribe("workers")
                for event in stream:
                    ...

        Args:
            reconnect: Auto-reconnect and resubscribe on disconnect (default true).

        Returns:
            An unconnected :class:`~sqi_client.events.SqiEventStream` pointed at
            this client's server, carrying the same default headers.
        """
        from .events import SqiEventStream

        return SqiEventStream(
            _derive_ws_url(self._base_url),
            headers=self._default_headers,
            reconnect=reconnect,
        )

    def tail_task_logs_live(self, task_id: str, from_seq: int = 0) -> Iterator[LogChunk]:
        """Live-tail a task's logs over WebSocket — the push-based counterpart to
        :meth:`tail_task_logs`.

        Subscribes to ``tasks/{task_id}/logs`` and yields each :class:`LogChunk`
        as the server pushes it. Streams until the connection closes or the
        caller stops iterating; on an idle disconnect or server restart the
        underlying stream reconnects and resumes from the last seen event.

        Requires the optional ``ws`` extra; without it the first iteration
        raises :class:`ImportError` naming the ``pip install 'sqi-sdk[ws]'``
        remedy.

        Args:
            task_id: The task whose logs to tail.
            from_seq: Resume cursor — replay buffered events with ``seq`` greater
                than this before streaming live ones. ``0`` (the default) streams
                only new events going forward; pass the last seen ``seq`` to
                resume without missing chunks.

        Yields:
            Each :class:`LogChunk` pushed for the task. Note these come from the
            live ``TaskLogPush`` payload, which omits ``id``/``nats_seq``/
            ``received_at`` (those are zero-valued); ``seq_num`` and the content
            fields are present.
        """
        subject = f"tasks/{task_id}/logs"
        with self.events() as stream:
            stream.subscribe(subject, since_seq=from_seq)
            for event in stream:
                if event.subject == subject:
                    yield LogChunk.from_dict(event.payload)

    # ── High-level conveniences ───────────────────────────────────────────────

    def wait_for_job(
        self,
        job_id: str,
        poll_interval: float = 2.0,
        timeout: float | None = None,
    ) -> Job:
        """Poll a job until it finishes, returning the final :class:`Job`.

        Repeatedly calls :meth:`get_job` until the job reaches a terminal status
        (``completed``, ``failed``, or ``canceled``), sleeping ``poll_interval``
        seconds between polls.

        Note that ``paused`` is *not* terminal: waiting on a paused job that is
        never resumed blocks until ``timeout`` (or forever when ``timeout`` is
        ``None``). Pass a ``timeout`` when that is possible. A very small
        ``poll_interval`` polls the server aggressively.

        Args:
            job_id: The job to wait on.
            poll_interval: Seconds between polls (default 2.0).
            timeout: Maximum seconds to wait before giving up. ``None`` (the
                default) waits indefinitely.

        Returns:
            The :class:`Job` in its terminal state.

        Raises:
            SqiTimeoutError: ``timeout`` elapsed before the job finished.
            NotFoundError: No job with that ID exists (HTTP 404).
        """
        deadline = None if timeout is None else time.monotonic() + timeout
        while True:
            job = self.get_job(job_id)
            if job.status in _TERMINAL_JOB_STATUSES:
                return job
            now = time.monotonic()
            if deadline is not None and now >= deadline:
                raise SqiTimeoutError(
                    f"job {job_id} did not reach a terminal status within {timeout}s"
                )
            # Cap the sleep at the remaining time so the final poll lands on the
            # deadline rather than overshooting it.
            delay = poll_interval if deadline is None else min(poll_interval, deadline - now)
            time.sleep(delay)

    def submit_and_wait(
        self,
        template: JobTemplate,
        *,
        farm_id: str,
        queue_id: str,
        owner: str | None = None,
        priority: int | None = None,
        project: str | None = None,
        max_attempts: int | None = None,
        retry_delay_seconds: int | None = None,
        failure_limit: int | None = None,
        depends_on: list[str] | None = None,
        poll_interval: float = 2.0,
        timeout: float | None = None,
    ) -> Job:
        """Submit a job and block until it finishes — the one-call pipeline path.

        Composes :meth:`submit_job` and :meth:`wait_for_job`: accepts the same
        submission arguments as the former (including the ``max_attempts`` /
        ``retry_delay_seconds`` / ``failure_limit`` retry-policy overrides and
        the ``depends_on`` cross-job dependencies) plus the
        ``poll_interval``/``timeout`` of the latter.

        Returns:
            The finished :class:`Job` in its terminal state.

        Raises:
            ValidationError: The template failed server-side validation (422).
            SqiTimeoutError: ``timeout`` elapsed before the job finished.
        """
        job = self.submit_job(
            template,
            farm_id=farm_id,
            queue_id=queue_id,
            owner=owner,
            priority=priority,
            project=project,
            max_attempts=max_attempts,
            retry_delay_seconds=retry_delay_seconds,
            failure_limit=failure_limit,
            depends_on=depends_on,
        )
        return self.wait_for_job(job.id, poll_interval=poll_interval, timeout=timeout)

    # ── Health probes ─────────────────────────────────────────────────────────

    def server_version(self) -> ServerVersion:
        """Return the running server's build metadata (``GET /api/v1/version``).

        Reports the server's semantic version, git commit, build date, and Go
        runtime version. Unlike :meth:`ping`/:meth:`ready`, this raises on
        transport or HTTP errors rather than swallowing them.
        """
        return ServerVersion.from_dict(self._request_json("GET", "/version"))

    def ping(self) -> bool:
        """Return ``True`` if the server's liveness probe (``/healthz``) is OK.

        Swallows connection/timeout errors into ``False`` — this is a probe, not
        an assertion.
        """
        return self._probe("/healthz")

    def ready(self) -> bool:
        """Return ``True`` if the server's readiness probe (``/readyz``) is OK.

        Swallows connection/timeout errors into ``False``.
        """
        return self._probe("/readyz")

    def _probe(self, path: str) -> bool:
        try:
            response = self._send("GET", self._build_url(path, api=False), allow_retry=False)
        except SqiError:
            return False
        return response.status_code == 200


# Minimum job priority the server accepts (OpenAPI `Job.priority` /
# `PatchJobRequest.priority`, both `minimum: 1`). There is no declared maximum.
_MIN_JOB_PRIORITY = 1

# Minimum usage-pool concurrency the server accepts (OpenAPI `UsagePool` /
# `CreateUsagePoolRequest.max_concurrent`, `minimum: 1`).
_MIN_USAGE_MAX_CONCURRENT = 1

# Task states from which no further log output or transitions occur; once a task
# reaches one, a follow-tail can stop after draining (see tail_task_logs).
_TERMINAL_TASK_STATUSES = frozenset({TaskStatus.SUCCEEDED, TaskStatus.FAILED, TaskStatus.CANCELED})

# Job states past which no further transitions occur; wait_for_job returns once a
# job reaches one of these.
_TERMINAL_JOB_STATUSES = frozenset({JobStatus.COMPLETED, JobStatus.FAILED, JobStatus.CANCELED})


def _enum_value(value: Any) -> Any:
    """Return an enum's wire value, passing through plain values unchanged.

    Status filters accept either a :class:`~sqi_client.models.JobStatus` (a
    str-enum) or a plain string; this normalizes the enum to its serialized
    value so the query string carries ``running``, not ``JobStatus.RUNNING``.
    """
    return value.value if isinstance(value, Enum) else value


def _derive_ws_url(base_url: str) -> str:
    """Derive the ``ws(s)://…/api/v1/ws`` endpoint from an ``http(s)`` base URL.

    Any path component is preserved (so a reverse-proxy subpath works), matching
    how :meth:`SqiClient._build_url` treats the base URL.
    """
    if base_url.startswith("https://"):
        return f"wss://{base_url[len('https://') :]}/api/v1/ws"
    if base_url.startswith("http://"):
        return f"ws://{base_url[len('http://') :]}/api/v1/ws"
    # Already a ws/wss or scheme-less base — best effort.
    return f"{base_url}/api/v1/ws"


# ── CRUD request-body builders ────────────────────────────────────────────────
#
# Each builds the create/update request body for a resource family (the create
# and update schemas are identical per resource — both are full representations).
# Required and defaulted fields are always sent; optional fields are included
# only when supplied, so a full-replace PUT that omits one clears it server-side.


def _farm_body(
    name: str,
    description: str | None,
    max_concurrent_tasks: int,
    max_attempts: int | None = None,
    retry_delay_seconds: int | None = None,
    failure_limit: int | None = None,
) -> dict[str, Any]:
    body: dict[str, Any] = {"name": name, "max_concurrent_tasks": max_concurrent_tasks}
    if description is not None:
        body["description"] = description
    _add_retry_policy(body, max_attempts, retry_delay_seconds, failure_limit)
    return body


def _queue_body(
    farm_id: str,
    name: str,
    description: str | None,
    priority: int,
    max_concurrent_tasks: int,
    paused: bool,
    max_attempts: int | None = None,
    retry_delay_seconds: int | None = None,
    failure_limit: int | None = None,
) -> dict[str, Any]:
    body: dict[str, Any] = {
        "farm_id": farm_id,
        "name": name,
        "priority": priority,
        "max_concurrent_tasks": max_concurrent_tasks,
        "paused": paused,
    }
    if description is not None:
        body["description"] = description
    _add_retry_policy(body, max_attempts, retry_delay_seconds, failure_limit)
    return body


def _add_retry_policy(
    body: dict[str, Any],
    max_attempts: int | None,
    retry_delay_seconds: int | None,
    failure_limit: int | None,
) -> None:
    """Add the retry-policy override keys to a farm/queue request body.

    Each key is included only when its value is supplied, matching the
    "included only when not ``None``" convention the other optional body
    fields in this module follow.
    """
    if max_attempts is not None:
        body["max_attempts"] = max_attempts
    if retry_delay_seconds is not None:
        body["retry_delay_seconds"] = retry_delay_seconds
    if failure_limit is not None:
        body["failure_limit"] = failure_limit


def _storage_body(
    name: str,
    description: str | None,
    roots: Mapping[str, str] | None,
) -> dict[str, Any]:
    body: dict[str, Any] = {"name": name}
    if description is not None:
        body["description"] = description
    if roots is not None:
        body["roots"] = dict(roots)
    return body


def _compute_location_body(
    name: str,
    description: str | None,
) -> dict[str, Any]:
    body: dict[str, Any] = {"name": name}
    if description is not None:
        body["description"] = description
    return body


def _product_body(
    name: str,
    title: str | None,
    description: str | None,
    category: str | None,
    version: str | None,
    template: str,
    template_format: str,
) -> dict[str, Any]:
    body: dict[str, Any] = {"name": name, "template": template, "format": template_format}
    if title is not None:
        body["title"] = title
    if description is not None:
        body["description"] = description
    if category is not None:
        body["category"] = category
    if version is not None:
        body["version"] = version
    return body


def _usage_body(
    name: str,
    max_concurrent: int,
    server_hint: str | None,
) -> dict[str, Any]:
    if max_concurrent < _MIN_USAGE_MAX_CONCURRENT:
        raise ValueError(
            f"max_concurrent must be >= {_MIN_USAGE_MAX_CONCURRENT}, got {max_concurrent}"
        )
    body: dict[str, Any] = {
        "name": name,
        "max_concurrent": max_concurrent,
    }
    if server_hint is not None:
        body["server_hint"] = server_hint
    return body


_CONTENT_TYPE_JSON = "application/json"
# The server recognizes application/x-yaml (alongside application/yaml and
# text/yaml) as the YAML format; anything else is treated as JSON.
_CONTENT_TYPE_YAML = "application/x-yaml"
_YAML_SUFFIXES = (".yaml", ".yml")


def _prepare_template(template: JobTemplate) -> tuple[str, str]:
    """Resolve a template input to its request body text and ``Content-Type``.

    A ``dict`` is serialized to JSON; a ``Path`` is read from disk (always YAML
    for ``.yaml``/``.yml``); a ``str`` is sent verbatim. For string and file
    content, the type is inferred by attempting a JSON parse.
    """
    if isinstance(template, dict):
        try:
            return json.dumps(template), _CONTENT_TYPE_JSON
        except (TypeError, ValueError) as exc:
            # Distinguish an unserializable *value* inside the dict from the
            # wrong-type case below: both would otherwise surface as a bare
            # TypeError, which a caller cannot tell apart.
            raise ValueError(f"template dict is not JSON-serializable: {exc}") from exc
    if isinstance(template, Path):
        text = template.read_text(encoding="utf-8")
        if template.suffix.lower() in _YAML_SUFFIXES:
            return text, _CONTENT_TYPE_YAML
        return text, _content_type_for_text(text)
    if isinstance(template, str):
        return template, _content_type_for_text(template)
    raise TypeError(f"template must be a str, pathlib.Path, or dict, not {type(template).__name__}")


def _content_type_for_text(text: str) -> str:
    """Return the JSON content type if ``text`` parses as JSON, else YAML."""
    try:
        json.loads(text)
    except ValueError:
        return _CONTENT_TYPE_YAML
    return _CONTENT_TYPE_JSON


def _is_retryable_status(status: int) -> bool:
    return status == 429 or 500 <= status <= 599


def _drop_none(params: Mapping[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in params.items() if value is not None}


def _major_version(version: str) -> int | None:
    head = version.strip().split(".", 1)[0]
    try:
        return int(head)
    except ValueError:
        return None


# A path segment that is almost certainly a resource ID rather than a route
# word: a UUID, a plain number, or a long hex string. Route words ("jobs",
# "storage-locations", the "v1" prefix) match none of these.
_ID_SEGMENT = re.compile(
    r"^(?:"
    r"[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"
    r"|[0-9]+"
    r"|[0-9a-fA-F]{16,}"
    r")$"
)

# Backstop cap on remembered deprecated-endpoint keys (see _warn_deprecated_endpoint).
_MAX_DEPRECATION_KEYS = 100


def _endpoint_key(path: str) -> str:
    """Collapse ID-like path segments to ``{id}``, yielding the endpoint template.

    ``/api/v1/jobs/018f…345`` and ``/api/v1/jobs/119e…7f8`` both key as
    ``/api/v1/jobs/{id}`` so per-endpoint deduplication does not degrade into
    per-resource.
    """
    return "/".join(
        "{id}" if _ID_SEGMENT.match(segment) else segment for segment in path.split("/")
    )


def _deprecation_link(value: str | None) -> str | None:
    """Extract the URL from a ``Link`` header entry with ``rel="deprecation"``.

    Matches the exact formatting sqi-server's middleware emits
    (``<url>; rel="deprecation"``) — not a full RFC 8288 parser; an
    unrecognized form just yields a less informative warning.
    """
    if not value:
        return None
    for part in value.split(","):
        if 'rel="deprecation"' in part:
            start = part.find("<")
            end = part.find(">")
            if 0 <= start < end:
                return part[start + 1 : end]
    return None
