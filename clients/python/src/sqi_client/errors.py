# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Exception hierarchy for the sqi client.

All errors raised by :class:`sqi_client.client.SqiClient` derive from
:class:`SqiError`, so a caller can catch that single base to handle any client
failure. Transport failures map to :class:`SqiConnectionError` /
:class:`SqiTimeoutError`; any non-2xx HTTP response maps to :class:`APIError` or
one of its status-specific subclasses, populated from the server's RFC 7807
``application/problem+json`` body (see ``docs/api.md``).
"""

from __future__ import annotations

import math

import httpx

__all__ = [
    "APIError",
    "BadRequestError",
    "ConflictError",
    "NotFoundError",
    "RateLimitError",
    "ServerError",
    "SqiAuthError",
    "SqiConnectionError",
    "SqiError",
    "SqiTimeoutError",
    "ValidationError",
]


class SqiError(Exception):
    """Base class for every error raised by sqi-sdk."""


class SqiConnectionError(SqiError):
    """The server could not be reached (DNS, connection refused, reset, ...).

    Wraps the underlying :class:`httpx.TransportError`, available via
    ``__cause__``.
    """


class SqiTimeoutError(SqiError):
    """A request did not complete within the configured timeout.

    Wraps the underlying :class:`httpx.TimeoutException`, available via
    ``__cause__``.
    """


class APIError(SqiError):
    """The server returned a non-2xx HTTP response.

    Fields are populated from the RFC 7807 problem-details body when present,
    falling back to the raw response text otherwise.

    Attributes:
        status: The HTTP status code.
        title: The problem ``title`` (or the HTTP reason phrase as a fallback).
        detail: The human-readable problem ``detail`` (or raw body text).
        request_id: The request correlation id, taken from the problem
            ``instance`` field or the ``X-Request-Id`` response header. Include
            this when reporting a server-side failure.
        retry_after: For 429 responses, the ``Retry-After`` value in seconds if
            the server supplied one; otherwise ``None``.
    """

    def __init__(
        self,
        *,
        status: int,
        title: str | None = None,
        detail: str | None = None,
        request_id: str | None = None,
        retry_after: float | None = None,
    ) -> None:
        self.status = status
        self.title = title
        self.detail = detail
        self.request_id = request_id
        self.retry_after = retry_after
        super().__init__(self._summary())

    def _summary(self) -> str:
        message = self.detail or self.title or "API request failed"
        text = f"[HTTP {self.status}] {message}"
        if self.request_id:
            text += f" (request_id={self.request_id})"
        return text


class BadRequestError(APIError):
    """HTTP 400 — the request was malformed."""


class NotFoundError(APIError):
    """HTTP 404 — the requested resource does not exist."""


class ConflictError(APIError):
    """HTTP 409 — the request conflicts with the resource's current state."""


class ValidationError(APIError):
    """HTTP 422 — the request body failed validation (e.g. bad OpenJD template)."""


class RateLimitError(APIError):
    """HTTP 429 — the client is being rate limited. See :attr:`APIError.retry_after`."""


class ServerError(APIError):
    """HTTP 5xx — the server failed to process a valid request."""


class SqiAuthError(APIError):
    """HTTP 401/403 — authentication is required or the credential is rejected."""


# Exact status-code → subclass mapping. 5xx falls through to ServerError and any
# other unmapped status to the APIError base (see _class_for_status).
_STATUS_TO_CLASS = {
    400: BadRequestError,
    401: SqiAuthError,
    403: SqiAuthError,
    404: NotFoundError,
    409: ConflictError,
    422: ValidationError,
    429: RateLimitError,
}


def _class_for_status(status: int) -> type[APIError]:
    cls = _STATUS_TO_CLASS.get(status)
    if cls is not None:
        return cls
    if 500 <= status <= 599:
        return ServerError
    return APIError


def _parse_retry_after(value: str | None) -> float | None:
    """Parse a ``Retry-After`` header value expressed in delta-seconds.

    sqi-server's rate limiter emits the delta-seconds form on 429 responses.
    The HTTP-date form is intentionally not supported, so a non-numeric header
    yields ``None`` — as does a non-finite value like ``inf``, which would
    otherwise translate into an unbounded sleep in the retry path.
    """
    if not value:
        return None
    try:
        parsed = float(value)
    except ValueError:
        return None
    if not math.isfinite(parsed):
        return None
    return parsed


def api_error_from_response(response: httpx.Response) -> APIError:
    """Build the appropriate :class:`APIError` subclass from a non-2xx response.

    Parses an ``application/problem+json`` body into ``title``/``detail``/
    ``request_id``, falling back to the raw response text and the HTTP reason
    phrase when the body is missing or is not valid problem-details JSON.
    """
    status = response.status_code
    header_request_id = response.headers.get("X-Request-Id")
    request_id: str | None = None
    title: str | None = None
    detail: str | None = None

    content_type = response.headers.get("Content-Type", "").lower()
    parsed: object = None
    if "json" in content_type:
        try:
            parsed = response.json()
        except ValueError:
            parsed = None

    if isinstance(parsed, dict):
        raw_title = parsed.get("title")
        raw_detail = parsed.get("detail")
        instance = parsed.get("instance")
        if isinstance(raw_title, str):
            title = raw_title
        if isinstance(raw_detail, str):
            detail = raw_detail
        if isinstance(instance, str) and instance:
            request_id = instance

    # Prefer the body's `instance`; fall back to the X-Request-Id header.
    if request_id is None:
        request_id = header_request_id

    if detail is None:
        body = response.text.strip()
        if body:
            detail = body
    if title is None and response.reason_phrase:
        title = response.reason_phrase

    retry_after = _parse_retry_after(response.headers.get("Retry-After")) if status == 429 else None

    cls = _class_for_status(status)
    return cls(
        status=status,
        title=title,
        detail=detail,
        request_id=request_id,
        retry_after=retry_after,
    )
