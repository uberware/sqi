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

import logging
import random
import time
import warnings
from collections.abc import Mapping
from types import TracebackType
from typing import Any

import httpx

from ._version import __version__
from .errors import (
    SqiConnectionError,
    SqiError,
    SqiTimeoutError,
    _parse_retry_after,
    api_error_from_response,
)

__all__ = ["SqiClient"]

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
        if path in self._deprecation_warned:
            return
        self._deprecation_warned.add(path)

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


def _deprecation_link(value: str | None) -> str | None:
    """Extract the URL from a ``Link`` header entry with ``rel="deprecation"``."""
    if not value:
        return None
    for part in value.split(","):
        if 'rel="deprecation"' in part:
            start = part.find("<")
            end = part.find(">")
            if 0 <= start < end:
                return part[start + 1 : end]
    return None
