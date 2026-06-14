# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for raw OpenJD job submission.

HTTP is mocked at the transport layer with respx; no server runs.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import httpx
import pytest
import respx

from sqi_client.errors import ValidationError
from sqi_client.models import Job, JobStatus
from tests.conftest import BASE_URL, ClientFactory

_JOBS_URL = f"{BASE_URL}/api/v1/jobs"

_JOB_RESPONSE: dict[str, Any] = {
    "id": "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345",
    "farm_id": "farm-1",
    "queue_id": "queue-1",
    "name": "My Render Job",
    "owner": "alice",
    "submitter": "",
    "priority": 50,
    "status": "pending",
    "template_format": "yaml",
    "created_at": "2026-01-15T10:00:00Z",
    "updated_at": "2026-01-15T10:00:00Z",
}

_YAML_TEMPLATE = (
    "specificationVersion: jobtemplate-2025-09\n"
    "name: My Render Job\n"
    "steps:\n"
    "  - name: Render\n"
    "    script:\n"
    "      actions:\n"
    "        onRun:\n"
    "          command: echo\n"
)

_DICT_TEMPLATE: dict[str, Any] = {
    "specificationVersion": "jobtemplate-2025-09",
    "name": "My Render Job",
    "steps": [{"name": "Render"}],
}


def _created() -> httpx.Response:
    return httpx.Response(201, json=_JOB_RESPONSE)


# ── Return value ────────────────────────────────────────────────────


@respx.mock
def test_submit_returns_parsed_job(make_client: ClientFactory) -> None:
    respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()

    job = client.submit_job(_DICT_TEMPLATE, farm_id="farm-1", queue_id="queue-1")

    assert isinstance(job, Job)
    assert job.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345"
    assert job.status is JobStatus.PENDING


# ── Template input types + content-type selection ──────────────


@respx.mock
def test_dict_template_is_json(make_client: ClientFactory) -> None:
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()

    client.submit_job(_DICT_TEMPLATE, farm_id="farm-1", queue_id="queue-1")

    request = route.calls.last.request
    assert request.headers["Content-Type"] == "application/json"
    # Body is the dict serialized to JSON and round-trips back to it.
    assert json.loads(request.content) == _DICT_TEMPLATE


@respx.mock
def test_json_string_template_is_json(make_client: ClientFactory) -> None:
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()
    payload = json.dumps(_DICT_TEMPLATE)

    client.submit_job(payload, farm_id="farm-1", queue_id="queue-1")

    request = route.calls.last.request
    assert request.headers["Content-Type"] == "application/json"
    # A string is sent verbatim, not re-serialized.
    assert request.content.decode() == payload


@respx.mock
def test_yaml_string_template_is_yaml(make_client: ClientFactory) -> None:
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()

    client.submit_job(_YAML_TEMPLATE, farm_id="farm-1", queue_id="queue-1")

    request = route.calls.last.request
    assert request.headers["Content-Type"] == "application/x-yaml"
    assert request.content.decode() == _YAML_TEMPLATE


@respx.mock
def test_yaml_path_template_is_yaml(make_client: ClientFactory, tmp_path: Path) -> None:
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()
    template_file = tmp_path / "job.yaml"
    template_file.write_text(_YAML_TEMPLATE)

    client.submit_job(template_file, farm_id="farm-1", queue_id="queue-1")

    request = route.calls.last.request
    assert request.headers["Content-Type"] == "application/x-yaml"
    assert request.content.decode() == _YAML_TEMPLATE


@respx.mock
def test_yml_path_with_json_content_still_yaml(make_client: ClientFactory, tmp_path: Path) -> None:
    # A .yml path is always treated as YAML, even if its bytes happen to parse as
    # JSON (JSON is a subset of YAML).
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()
    template_file = tmp_path / "job.yml"
    template_file.write_text(json.dumps(_DICT_TEMPLATE))

    client.submit_job(template_file, farm_id="farm-1", queue_id="queue-1")

    assert route.calls.last.request.headers["Content-Type"] == "application/x-yaml"


@respx.mock
def test_json_path_template_is_json(make_client: ClientFactory, tmp_path: Path) -> None:
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()
    template_file = tmp_path / "job.json"
    template_file.write_text(json.dumps(_DICT_TEMPLATE))

    client.submit_job(template_file, farm_id="farm-1", queue_id="queue-1")

    request = route.calls.last.request
    assert request.headers["Content-Type"] == "application/json"
    assert json.loads(request.content) == _DICT_TEMPLATE


def test_unsupported_template_type_raises(make_client: ClientFactory) -> None:
    client = make_client()
    with pytest.raises(TypeError):
        client.submit_job(12345, farm_id="farm-1", queue_id="queue-1")  # type: ignore[arg-type]


def test_unserializable_dict_raises_value_error(make_client: ClientFactory) -> None:
    # A dict carrying a non-JSON-serializable value must surface as ValueError,
    # not collide with the wrong-type TypeError.
    client = make_client()
    with pytest.raises(ValueError, match="not JSON-serializable"):
        client.submit_job(
            {"name": "bad", "extra": {1, 2, 3}},  # a set is not JSON-serializable
            farm_id="farm-1",
            queue_id="queue-1",
        )


@respx.mock
def test_empty_string_template_sent_as_yaml(make_client: ClientFactory) -> None:
    # An empty template is not parseable as JSON, so it sniffs to YAML and is
    # sent verbatim; the server is left to reject the empty body.
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()

    client.submit_job("", farm_id="farm-1", queue_id="queue-1")

    request = route.calls.last.request
    assert request.headers["Content-Type"] == "application/x-yaml"
    assert request.content == b""


@respx.mock
def test_unsuffixed_path_is_sniffed_by_content(make_client: ClientFactory, tmp_path: Path) -> None:
    # A path whose suffix is neither .yaml/.yml nor .json falls back to sniffing
    # the file's bytes: YAML content -> application/x-yaml.
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()
    template_file = tmp_path / "job.openjd"
    template_file.write_text(_YAML_TEMPLATE)

    client.submit_job(template_file, farm_id="farm-1", queue_id="queue-1")

    assert route.calls.last.request.headers["Content-Type"] == "application/x-yaml"


# ── Query parameter encoding ────────────────────────────────────────


@respx.mock
def test_required_params_only_omits_optionals(make_client: ClientFactory) -> None:
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()

    client.submit_job(_DICT_TEMPLATE, farm_id="farm-1", queue_id="queue-1")

    params = route.calls.last.request.url.params
    assert params["farm_id"] == "farm-1"
    assert params["queue_id"] == "queue-1"
    # None-valued optionals never appear in the URL.
    assert "owner" not in params
    assert "priority" not in params
    assert "project" not in params


@respx.mock
def test_optional_params_encoded_when_set(make_client: ClientFactory) -> None:
    route = respx.post(_JOBS_URL).mock(return_value=_created())
    client = make_client()

    client.submit_job(
        _DICT_TEMPLATE,
        farm_id="farm-1",
        queue_id="queue-1",
        owner="alice",
        priority=90,
        project="my-film",
    )

    params = route.calls.last.request.url.params
    assert params["owner"] == "alice"
    assert params["priority"] == "90"
    assert params["project"] == "my-film"


# ── Validation failures ─────────────────────────────────────────────


@respx.mock
def test_422_becomes_validation_error(make_client: ClientFactory) -> None:
    detail = "step 'Render' references undefined parameter 'Frames'"
    respx.post(_JOBS_URL).mock(
        return_value=httpx.Response(
            422,
            json={
                "type": "about:blank",
                "title": "Unprocessable Entity",
                "status": 422,
                "detail": detail,
                "instance": "a1b2c3d4e5f60708",
            },
            headers={"Content-Type": "application/problem+json"},
        )
    )
    client = make_client()

    with pytest.raises(ValidationError) as excinfo:
        client.submit_job(_YAML_TEMPLATE, farm_id="farm-1", queue_id="queue-1")

    err = excinfo.value
    assert err.status == 422
    assert err.detail == detail
    # The detail is surfaced verbatim so a submitter can show it to the artist.
    assert detail in str(err)


@respx.mock
def test_submit_is_not_retried(make_client: ClientFactory) -> None:
    # POST is non-idempotent: a transient 500 must surface immediately, not retry.
    route = respx.post(_JOBS_URL).mock(return_value=httpx.Response(500))
    client = make_client(max_attempts=3, retry_backoff=0.0, retry_jitter=False)

    from sqi_client.errors import ServerError

    with pytest.raises(ServerError):
        client.submit_job(_DICT_TEMPLATE, farm_id="farm-1", queue_id="queue-1")

    assert route.call_count == 1
