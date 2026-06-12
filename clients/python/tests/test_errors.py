# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for HTTP error mapping and RFC 7807 problem-details parsing (task 19)."""

from __future__ import annotations

import httpx
import pytest
import respx

from sqi_client.errors import (
    APIError,
    BadRequestError,
    ConflictError,
    NotFoundError,
    RateLimitError,
    ServerError,
    SqiConnectionError,
    SqiTimeoutError,
    ValidationError,
)
from tests.conftest import BASE_URL, ClientFactory, ProblemFactory

_URL = f"{BASE_URL}/api/v1/probe"


@pytest.mark.parametrize(
    ("status", "expected"),
    [
        (400, BadRequestError),
        (404, NotFoundError),
        (409, ConflictError),
        (422, ValidationError),
        (429, RateLimitError),
        (500, ServerError),
        (502, ServerError),
        (503, ServerError),
        (418, APIError),  # unmapped 4xx falls back to the base APIError
    ],
)
@respx.mock
def test_status_maps_to_exception_class(
    status: int,
    expected: type[APIError],
    make_client: ClientFactory,
    problem: ProblemFactory,
) -> None:
    respx.get(_URL).mock(return_value=problem(status, detail="boom"))
    client = make_client()

    with pytest.raises(expected) as exc_info:
        client._request("GET", "/probe")

    assert type(exc_info.value) is expected
    assert exc_info.value.status == status


@respx.mock
def test_problem_fields_are_extracted(make_client: ClientFactory, problem: ProblemFactory) -> None:
    respx.get(_URL).mock(
        return_value=problem(
            404,
            title="Not Found",
            detail="job not found",
            instance="a1b2c3d4e5f60708",
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError) as exc_info:
        client._request("GET", "/probe")

    err = exc_info.value
    assert err.status == 404
    assert err.title == "Not Found"
    assert err.detail == "job not found"
    assert err.request_id == "a1b2c3d4e5f60708"


@respx.mock
def test_request_id_falls_back_to_header(
    make_client: ClientFactory, problem: ProblemFactory
) -> None:
    # No `instance` in the body, but an X-Request-Id header is present.
    respx.get(_URL).mock(
        return_value=problem(409, detail="conflict", request_id_header="hdr-req-99")
    )
    client = make_client()

    with pytest.raises(ConflictError) as exc_info:
        client._request("GET", "/probe")

    assert exc_info.value.request_id == "hdr-req-99"


@respx.mock
def test_instance_preferred_over_header_is_not_required_but_header_used_when_absent(
    make_client: ClientFactory,
) -> None:
    # Body has neither instance nor parseable JSON; header supplies the id.
    respx.get(_URL).mock(
        return_value=httpx.Response(
            500,
            content=b"upstream exploded",
            headers={"Content-Type": "text/plain", "X-Request-Id": "hdr-1"},
        )
    )
    client = make_client()

    with pytest.raises(ServerError) as exc_info:
        client._request("GET", "/probe")

    err = exc_info.value
    # Falls back to raw body text for detail and HTTP reason phrase for title.
    assert err.detail == "upstream exploded"
    assert err.title == "Internal Server Error"
    assert err.request_id == "hdr-1"


@respx.mock
def test_malformed_problem_body_falls_back_cleanly(make_client: ClientFactory) -> None:
    respx.get(_URL).mock(
        return_value=httpx.Response(
            404,
            content=b"<html>not json</html>",
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(NotFoundError) as exc_info:
        client._request("GET", "/probe")

    err = exc_info.value
    assert err.detail == "<html>not json</html>"
    assert err.title == "Not Found"


@respx.mock
def test_empty_body_uses_reason_phrase(make_client: ClientFactory) -> None:
    respx.get(_URL).mock(return_value=httpx.Response(404))
    client = make_client()

    with pytest.raises(NotFoundError) as exc_info:
        client._request("GET", "/probe")

    err = exc_info.value
    assert err.detail is None
    assert err.title == "Not Found"


@respx.mock
def test_str_includes_status_detail_and_request_id(
    make_client: ClientFactory, problem: ProblemFactory
) -> None:
    respx.get(_URL).mock(return_value=problem(404, detail="job not found", instance="req-123"))
    client = make_client()

    with pytest.raises(NotFoundError) as exc_info:
        client._request("GET", "/probe")

    rendered = str(exc_info.value)
    assert "404" in rendered
    assert "job not found" in rendered
    assert "req-123" in rendered


@respx.mock
def test_retry_after_attached_to_rate_limit_error(
    make_client: ClientFactory, problem: ProblemFactory
) -> None:
    response = problem(429, detail="slow down")
    response.headers["Retry-After"] = "12"
    respx.get(_URL).mock(return_value=response)
    client = make_client()

    with pytest.raises(RateLimitError) as exc_info:
        client._request("GET", "/probe")

    assert exc_info.value.retry_after == 12.0


@respx.mock
def test_non_finite_retry_after_is_dropped(
    make_client: ClientFactory, problem: ProblemFactory
) -> None:
    response = problem(429, detail="slow down")
    response.headers["Retry-After"] = "inf"
    respx.get(_URL).mock(return_value=response)
    client = make_client()

    with pytest.raises(RateLimitError) as exc_info:
        client._request("GET", "/probe")

    assert exc_info.value.retry_after is None


@respx.mock
def test_connect_error_becomes_connection_error(make_client: ClientFactory) -> None:
    respx.get(_URL).mock(side_effect=httpx.ConnectError("connection refused"))
    client = make_client()

    with pytest.raises(SqiConnectionError) as exc_info:
        client._request("GET", "/probe")

    assert isinstance(exc_info.value.__cause__, httpx.ConnectError)


@respx.mock
def test_timeout_becomes_timeout_error(make_client: ClientFactory) -> None:
    respx.get(_URL).mock(side_effect=httpx.ConnectTimeout("too slow"))
    client = make_client()

    with pytest.raises(SqiTimeoutError) as exc_info:
        client._request("GET", "/probe")

    assert isinstance(exc_info.value.__cause__, httpx.TimeoutException)
