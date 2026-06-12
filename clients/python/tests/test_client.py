# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for transport plumbing: headers, URL building, params, health, versioning.

Covers tasks 9-11 and 16-18.
"""

from __future__ import annotations

import logging
import warnings

import httpx
import pytest
import respx

from sqi_client import __version__
from sqi_client.client import SqiClient
from tests.conftest import BASE_URL, ClientFactory

_PROBE = f"{BASE_URL}/api/v1/probe"


@respx.mock
def test_default_headers_are_set(make_client: ClientFactory) -> None:
    route = respx.get(_PROBE).mock(return_value=httpx.Response(200))
    client = make_client()

    client._request("GET", "/probe")

    request = route.calls.last.request
    assert request.headers["User-Agent"] == f"sqi-client/{__version__}"
    assert request.headers["Accept"] == "application/json"


@respx.mock
def test_extra_headers_are_merged() -> None:
    route = respx.get(_PROBE).mock(return_value=httpx.Response(200))
    # Caller adds an auth-style header (the Phase 3 hook) and overrides Accept.
    with SqiClient(
        BASE_URL,
        max_attempts=1,
        headers={"Authorization": "Bearer t0ken", "Accept": "application/yaml"},
    ) as client:
        client._request("GET", "/probe")

    request = route.calls.last.request
    assert request.headers["Authorization"] == "Bearer t0ken"
    assert request.headers["Accept"] == "application/yaml"  # caller wins on conflict
    assert request.headers["User-Agent"] == f"sqi-client/{__version__}"  # default kept


@respx.mock
def test_api_prefix_is_applied(make_client: ClientFactory) -> None:
    route = respx.get(f"{BASE_URL}/api/v1/jobs").mock(return_value=httpx.Response(200))
    client = make_client()

    client._request("GET", "/jobs")

    assert route.called
    assert str(route.calls.last.request.url) == f"{BASE_URL}/api/v1/jobs"


@respx.mock
def test_health_paths_bypass_api_prefix(make_client: ClientFactory) -> None:
    route = respx.get(f"{BASE_URL}/healthz").mock(return_value=httpx.Response(200))
    client = make_client()

    assert client.ping() is True
    assert route.calls.last.request.url.path == "/healthz"


@respx.mock
def test_none_query_params_are_dropped(make_client: ClientFactory) -> None:
    route = respx.get(f"{BASE_URL}/api/v1/jobs").mock(return_value=httpx.Response(200))
    client = make_client()

    client._request("GET", "/jobs", params={"status": "running", "owner": None, "limit": 50})

    params = route.calls.last.request.url.params
    assert params["status"] == "running"
    assert params["limit"] == "50"
    assert "owner" not in params


@respx.mock
def test_ping_true_on_200(make_client: ClientFactory) -> None:
    respx.get(f"{BASE_URL}/healthz").mock(return_value=httpx.Response(200))
    assert make_client().ping() is True


@respx.mock
def test_ping_false_on_error_status(make_client: ClientFactory) -> None:
    respx.get(f"{BASE_URL}/healthz").mock(return_value=httpx.Response(503))
    assert make_client().ping() is False


@respx.mock
def test_ping_false_on_connection_error(make_client: ClientFactory) -> None:
    respx.get(f"{BASE_URL}/healthz").mock(side_effect=httpx.ConnectError("down"))
    # Probe must swallow transport failures rather than raise.
    assert make_client().ping() is False


@respx.mock
def test_ready_uses_readyz(make_client: ClientFactory) -> None:
    route = respx.get(f"{BASE_URL}/readyz").mock(return_value=httpx.Response(200))
    assert make_client().ready() is True
    assert route.calls.last.request.url.path == "/readyz"


@respx.mock
def test_newer_major_api_version_warns_once(make_client: ClientFactory) -> None:
    respx.get(_PROBE).mock(return_value=httpx.Response(200, headers={"X-API-Version": "2.4.0"}))
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")
        client._request("GET", "/probe")

    api_warnings = [w for w in caught if issubclass(w.category, UserWarning)]
    assert len(api_warnings) == 1
    assert "newer than this sqi-client" in str(api_warnings[0].message)


@respx.mock
def test_current_server_version_does_not_warn(make_client: ClientFactory) -> None:
    # The deployed sqi-server sends the API contract major matching /api/v1.
    respx.get(_PROBE).mock(return_value=httpx.Response(200, headers={"X-API-Version": "1"}))
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")

    assert [w for w in caught if issubclass(w.category, UserWarning)] == []


@respx.mock
def test_older_major_version_does_not_warn(make_client: ClientFactory) -> None:
    # Talking down to an older server is not the skew this warning is for.
    respx.get(_PROBE).mock(return_value=httpx.Response(200, headers={"X-API-Version": "0.1.0"}))
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")

    assert [w for w in caught if issubclass(w.category, UserWarning)] == []


@respx.mock
def test_deprecated_endpoint_warns_with_path(make_client: ClientFactory) -> None:
    # RFC 8594: deprecation is signaled by the Deprecation header (a date or
    # "true"), not by the X-API-Deprecated header docs/api.md used to describe.
    respx.get(_PROBE).mock(return_value=httpx.Response(200, headers={"Deprecation": "true"}))
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")

    messages = [str(w.message) for w in caught if issubclass(w.category, UserWarning)]
    assert any("deprecated" in m and "/api/v1/probe" in m for m in messages)


@respx.mock
def test_deprecation_warns_once_per_endpoint(make_client: ClientFactory) -> None:
    respx.get(f"{BASE_URL}/api/v1/old-a").mock(
        return_value=httpx.Response(200, headers={"Deprecation": "true"})
    )
    respx.get(f"{BASE_URL}/api/v1/old-b").mock(
        return_value=httpx.Response(200, headers={"Deprecation": "true"})
    )
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/old-a")
        client._request("GET", "/old-a")  # repeat: no second warning
        client._request("GET", "/old-b")  # different endpoint: its own warning

    messages = [str(w.message) for w in caught if issubclass(w.category, UserWarning)]
    assert len(messages) == 2
    assert any("/api/v1/old-a" in m for m in messages)
    assert any("/api/v1/old-b" in m for m in messages)


@respx.mock
def test_deprecation_warns_once_per_endpoint_template(make_client: ClientFactory) -> None:
    # ID-bearing paths collapse to one endpoint key — a pipeline iterating
    # thousands of resources of a deprecated /jobs/{id} endpoint must warn
    # once, not once per resource.
    respx.get(url__regex=rf"{BASE_URL}/api/v1/jobs/.*").mock(
        return_value=httpx.Response(200, headers={"Deprecation": "true"})
    )
    respx.get(url__regex=rf"{BASE_URL}/api/v1/tasks/.*").mock(
        return_value=httpx.Response(200, headers={"Deprecation": "true"})
    )
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/jobs/018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345")
        client._request("GET", "/jobs/119e2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")  # same endpoint
        client._request("GET", "/tasks/42")
        client._request("GET", "/tasks/43")  # same endpoint, numeric ids

    messages = [str(w.message) for w in caught if issubclass(w.category, UserWarning)]
    assert len(messages) == 2  # one per endpoint template, not per resource
    assert any("/api/v1/jobs/" in m for m in messages)
    assert any("/api/v1/tasks/" in m for m in messages)


@respx.mock
def test_deprecation_warning_includes_sunset_and_link(make_client: ClientFactory) -> None:
    respx.get(_PROBE).mock(
        return_value=httpx.Response(
            200,
            headers={
                "Deprecation": "Mon, 01 Jun 2026 00:00:00 GMT",
                "Sunset": "Fri, 01 Jan 2027 00:00:00 GMT",
                "Link": '<https://docs.sqi.dev/deprecations#probe>; rel="deprecation"',
            },
        )
    )
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")

    messages = [str(w.message) for w in caught if issubclass(w.category, UserWarning)]
    assert len(messages) == 1
    assert "Fri, 01 Jan 2027 00:00:00 GMT" in messages[0]
    assert "https://docs.sqi.dev/deprecations#probe" in messages[0]


@respx.mock
def test_legacy_x_api_deprecated_header_is_ignored(make_client: ClientFactory) -> None:
    # The header documented by the stale docs/api.md was never sent by the
    # server; the client must not act on it.
    respx.get(_PROBE).mock(
        return_value=httpx.Response(200, headers={"X-API-Version": "1", "X-API-Deprecated": "true"})
    )
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")

    assert [w for w in caught if issubclass(w.category, UserWarning)] == []


@respx.mock
def test_request_json_returns_none_on_204(make_client: ClientFactory) -> None:
    respx.delete(f"{BASE_URL}/api/v1/jobs/abc").mock(return_value=httpx.Response(204))
    client = make_client()

    assert client._request_json("DELETE", "/jobs/abc") is None


@respx.mock
def test_request_json_returns_decoded_body(make_client: ClientFactory) -> None:
    respx.get(f"{BASE_URL}/api/v1/jobs/abc").mock(
        return_value=httpx.Response(200, json={"id": "abc", "status": "running"})
    )
    client = make_client()

    assert client._request_json("GET", "/jobs/abc") == {"id": "abc", "status": "running"}


def test_context_manager_closes_pool() -> None:
    with SqiClient(BASE_URL) as client:
        inner = client._http
    assert inner.is_closed is True


def test_repr_shows_base_url() -> None:
    client = SqiClient(BASE_URL)
    assert repr(client) == f"SqiClient(base_url={BASE_URL!r})"
    client.close()


@respx.mock
def test_unparseable_version_warns(make_client: ClientFactory) -> None:
    respx.get(_PROBE).mock(return_value=httpx.Response(200, headers={"X-API-Version": "garbage"}))
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")

    messages = [str(w.message) for w in caught if issubclass(w.category, UserWarning)]
    assert any("unrecognized API version" in m for m in messages)


@respx.mock
def test_missing_version_header_does_not_warn(make_client: ClientFactory) -> None:
    # An older server that omits the header entirely must not warn.
    respx.get(_PROBE).mock(return_value=httpx.Response(200))
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")

    assert [w for w in caught if issubclass(w.category, UserWarning)] == []


@respx.mock
def test_deprecated_and_newer_major_both_warn(make_client: ClientFactory) -> None:
    # Endpoint deprecation and version skew are independent signals — a caller
    # migrating off a sunset endpoint also needs to know the client is too old.
    respx.get(_PROBE).mock(
        return_value=httpx.Response(200, headers={"X-API-Version": "2.0.0", "Deprecation": "true"})
    )
    client = make_client()

    with warnings.catch_warnings(record=True) as caught:
        warnings.simplefilter("always")
        client._request("GET", "/probe")

    messages = [str(w.message) for w in caught if issubclass(w.category, UserWarning)]
    assert len(messages) == 2
    assert any("deprecated" in m for m in messages)
    assert any("newer than this sqi-client" in m for m in messages)


@respx.mock
def test_ready_false_on_error_status(make_client: ClientFactory) -> None:
    respx.get(f"{BASE_URL}/readyz").mock(return_value=httpx.Response(503))
    assert make_client().ready() is False


@respx.mock
def test_ready_false_on_connection_error(make_client: ClientFactory) -> None:
    respx.get(f"{BASE_URL}/readyz").mock(side_effect=httpx.ConnectError("down"))
    assert make_client().ready() is False


@respx.mock
def test_request_logged_at_debug(
    make_client: ClientFactory, caplog: pytest.LogCaptureFixture
) -> None:
    respx.get(_PROBE).mock(return_value=httpx.Response(200))
    client = make_client()

    with caplog.at_level(logging.DEBUG, logger="sqi_client"):
        client._request("GET", "/probe")

    debug = [r for r in caplog.records if r.levelno == logging.DEBUG]
    assert any("/api/v1/probe" in r.getMessage() and "200" in r.getMessage() for r in debug)


@respx.mock
def test_retry_logged_at_warning(
    make_client: ClientFactory, caplog: pytest.LogCaptureFixture
) -> None:
    respx.get(_PROBE).mock(side_effect=[httpx.Response(503), httpx.Response(200)])
    client = make_client(max_attempts=2)

    with caplog.at_level(logging.WARNING, logger="sqi_client"):
        client._request("GET", "/probe")

    warning_messages = [r.getMessage() for r in caplog.records if r.levelno == logging.WARNING]
    assert any("retry" in m for m in warning_messages)


def test_library_logger_has_no_handler_attached() -> None:
    # Library logging etiquette: the package attaches no handler of its own.
    assert logging.getLogger("sqi_client").handlers == []
