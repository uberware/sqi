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
from collections.abc import Mapping
from pathlib import Path
from types import TracebackType
from typing import Any, Union

import httpx

from ._version import __version__
from .errors import (
    SqiConnectionError,
    SqiError,
    SqiTimeoutError,
    _parse_retry_after,
    api_error_from_response,
)
from .models import Job

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
            "User-Agent": f"sqi-client/{__version__}",
            "Accept": "application/json",
        }
        if headers:
            default_headers.update(headers)
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
            "sqi-client retry %d/%d: %s %s (%s)",
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
                "this sqi-client may be incompatible"
            )
        elif major > _SUPPORTED_API_MAJOR:
            message = (
                f"sqi-server API version {version} is newer than this sqi-client "
                f"supports (v{_SUPPORTED_API_MAJOR}.x); behavior may be unreliable "
                "— upgrade sqi-client"
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
        params = {
            "farm_id": farm_id,
            "queue_id": queue_id,
            "owner": owner,
            "priority": priority,
            "project": project,
        }
        data = self._request_json(
            "POST",
            "/jobs",
            params=params,
            content=body,
            headers={"Content-Type": content_type},
        )
        return Job.from_dict(data)

    # ── Health probes ─────────────────────────────────────────────────────────

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
