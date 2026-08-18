# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for the typed dataclass models and tolerant parsing.

Every model is parsed from a captured wire fixture (``tests/fixtures/``) so the
assertions track the real server response shapes, not hand-invented ones.
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from sqi_client.models import (
    CurrentTask,
    EffectiveRetryPolicy,
    FailureSummary,
    Farm,
    GPUInfo,
    Job,
    JobStatus,
    LogChunk,
    LogPage,
    Queue,
    ServerVersion,
    Step,
    StorageLocation,
    Task,
    TaskAttempt,
    TaskCounts,
    TaskStatus,
    UsagePool,
    Worker,
    WorkerStatus,
)

_FIXTURES = Path(__file__).parent / "fixtures"


def load(name: str) -> dict[str, Any]:
    """Load a JSON wire fixture by file stem."""
    data: dict[str, Any] = json.loads((_FIXTURES / f"{name}.json").read_text())
    return data


# ── Status enums ────────────────────────────────────────────────────


def test_job_status_known_values_parse_to_members() -> None:
    assert JobStatus.parse("running") is JobStatus.RUNNING
    assert JobStatus.parse("paused") is JobStatus.PAUSED
    # Members are real strings carrying their wire value.
    assert isinstance(JobStatus.RUNNING, str)
    assert JobStatus.RUNNING.value == "running"


def test_task_status_has_full_member_set() -> None:
    values = {s.value for s in TaskStatus}
    assert values == {
        "pending",
        "ready",
        "assigned",
        "running",
        "succeeded",
        "failed",
        "canceled",
    }


def test_worker_status_members() -> None:
    values = {s.value for s in WorkerStatus}
    assert values == {"online", "offline", "disabled"}


def test_unknown_enum_value_preserved_not_raised() -> None:
    # A newer server may introduce a status this client predates; parsing must
    # preserve it verbatim rather than raising.
    status = JobStatus.parse("archived")
    assert isinstance(status, JobStatus)
    # Check ``.value`` before the ``== "archived"`` assertion: mypy < 1.20 narrows
    # a str-enum to Literal['archived'] after the equality, which would otherwise
    # make the later ``.value`` access a spurious type error.
    assert status.value == "archived"
    assert status == "archived"
    # The unknown member is not registered in the canonical member map.
    assert "archived" not in {s.value for s in JobStatus}


def test_unknown_enum_value_in_model_parsing() -> None:
    raw = load("job")
    raw["status"] = "frobnicate"
    job = Job.from_dict(raw)
    assert isinstance(job.status, JobStatus)
    assert job.status == "frobnicate"


# ── Timestamp parsing ───────────────────────────────────────────────


def test_timestamps_are_timezone_aware() -> None:
    job = Job.from_dict(load("job"))
    assert job.created_at.tzinfo is not None
    assert job.created_at == datetime(2026, 1, 15, 10, 0, 0, tzinfo=timezone.utc)


def test_fractional_seconds_parsed() -> None:
    chunk = LogChunk.from_dict(load("log_chunk"))
    assert chunk.at == datetime(2026, 1, 15, 10, 5, 42, 123000, tzinfo=timezone.utc)
    assert chunk.received_at.microsecond == 130000


def test_nullable_timestamp_is_none() -> None:
    job = Job.from_dict(load("job"))
    # completed_at is null in the fixture.
    assert job.completed_at is None
    assert job.started_at == datetime(2026, 1, 15, 10, 1, 0, tzinfo=timezone.utc)


def test_naive_timestamp_assumed_utc() -> None:
    # A timestamp lacking an offset (not strictly RFC 3339) is treated as UTC so
    # callers still get a timezone-aware datetime rather than a naive one.
    raw = load("job")
    raw["created_at"] = "2026-01-15T10:00:00"
    job = Job.from_dict(raw)
    assert job.created_at == datetime(2026, 1, 15, 10, 0, 0, tzinfo=timezone.utc)


_ZERO_TIME = datetime(1, 1, 1, tzinfo=timezone.utc)


def test_required_timestamp_missing_or_malformed_falls_back_to_zero_time() -> None:
    # A required timestamp that is absent or unparseable must not raise — it
    # reads as the zero-time sentinel (matching the Go server's zero time.Time),
    # keeping parsing crash-free and inside the documented contract.
    missing = load("job")
    del missing["created_at"]
    assert Job.from_dict(missing).created_at == _ZERO_TIME

    malformed = load("job")
    malformed["created_at"] = "not-a-timestamp"
    job = Job.from_dict(malformed)
    assert job.created_at == _ZERO_TIME
    assert job.created_at.tzinfo is not None


def test_optional_timestamp_malformed_becomes_none() -> None:
    raw = load("job")
    raw["started_at"] = "garbage"
    assert Job.from_dict(raw).started_at is None


# ── Mistyped-field tolerance ────────────────────────────────────────


def test_mistyped_fields_fall_back_to_zero_values() -> None:
    # A server (or proxy) returning the wrong JSON type for a field must not
    # crash parsing — each field coerces to its declared type's zero value.
    raw = load("job")
    raw["priority"] = "not-a-number"
    raw["name"] = 12345
    job = Job.from_dict(raw)
    assert job.priority == 0
    assert job.name == ""


def test_bool_is_not_coerced_to_int() -> None:
    raw = load("task_counts")
    raw["total"] = True  # bool is an int subclass; must not become 1
    counts = TaskCounts.from_dict(raw)
    assert counts.total == 0


def test_mistyped_collections_fall_back_to_empty() -> None:
    raw = load("task")
    raw["parameters"] = ["not", "a", "map"]
    task = Task.from_dict(raw)
    assert task.parameters == {}

    raw_step = load("step")
    raw_step["depends_on"] = "not-a-list"
    step = Step.from_dict(raw_step)
    assert step.depends_on == []


def test_mistyped_optional_fields_become_none() -> None:
    raw = load("worker")
    raw["cpu_count"] = "sixty-four"
    raw["ram_mb"] = True  # bool is an int subclass; must not survive as 1
    raw["ip_address"] = 12345
    worker = Worker.from_dict(raw)
    assert worker.cpu_count is None
    assert worker.ram_mb is None
    assert worker.ip_address is None


# ── Unknown-field tolerance ─────────────────────────────────────────


def test_unknown_fields_ignored() -> None:
    raw = load("job")
    raw["some_future_field"] = {"nested": "value"}
    raw["another_new_one"] = 12345
    job = Job.from_dict(raw)
    assert job.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345"
    assert not hasattr(job, "some_future_field")


# ── Job, Step, TaskCounts ───────────────────────────────────────────


def test_job_list_level_fields() -> None:
    job = Job.from_dict(load("job"))
    assert job.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345"
    assert job.farm_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10001"
    assert job.queue_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10002"
    assert job.name == "Arnold Render — shot_0010"
    assert job.owner == "alice"
    assert job.submitter == "deadline-bridge"
    assert job.priority == 50
    assert job.status is JobStatus.RUNNING
    assert job.project == "my-film"
    assert job.template_format == "yaml"
    # Detail-only fields are absent on a list-level job.
    assert job.steps is None
    assert job.task_counts is None


def test_job_retry_policy_fields_default_when_absent() -> None:
    # Back-compat: an older/unpatched server's job response carries none of the
    # retry-policy fields; parsing must not raise and must default gracefully.
    job = Job.from_dict(load("job"))
    assert job.max_attempts is None
    assert job.retry_delay_seconds is None
    assert job.failure_limit is None
    assert job.failed_attempts == 0
    assert job.park_reason is None


def test_job_retry_policy_fields_parsed_when_present() -> None:
    raw = dict(
        load("job"),
        max_attempts=5,
        retry_delay_seconds=30,
        failure_limit=25,
        failed_attempts=3,
        park_reason="failure limit reached (25)",
    )
    job = Job.from_dict(raw)
    assert job.max_attempts == 5
    assert job.retry_delay_seconds == 30
    assert job.failure_limit == 25
    assert job.failed_attempts == 3
    assert job.park_reason == "failure limit reached (25)"


def test_job_failure_summary_parsed_when_present() -> None:
    raw = dict(
        load("job_detail"),
        failure_summary={
            "failed_count": 50,
            "dominant_reason": "staging",
            "distinct_reasons": 2,
        },
    )
    job = Job.from_dict(raw)
    assert job.failure_summary is not None
    assert isinstance(job.failure_summary, FailureSummary)
    assert job.failure_summary.failed_count == 50
    assert job.failure_summary.dominant_reason == "staging"
    assert job.failure_summary.distinct_reasons == 2


def test_job_failure_summary_absent_defaults_to_none() -> None:
    # Back-compat: an older/unpatched server's job response carries no
    # failure_summary; parsing must not raise and must default to None.
    assert Job.from_dict(load("job")).failure_summary is None
    assert Job.from_dict(load("job_detail")).failure_summary is None


def test_job_effective_retry_parsed_when_present() -> None:
    raw = dict(
        load("job_detail"),
        effective_retry={
            "max_attempts": 3,
            "retry_delay_seconds": 30,
            "failure_limit": 0,
        },
    )
    job = Job.from_dict(raw)
    assert job.effective_retry is not None
    assert isinstance(job.effective_retry, EffectiveRetryPolicy)
    assert job.effective_retry.max_attempts == 3
    assert job.effective_retry.retry_delay_seconds == 30
    # failure_limit 0 (auto-park off) must survive as a real 0, not a falsy drop.
    assert job.effective_retry.failure_limit == 0


def test_job_effective_retry_absent_defaults_to_none() -> None:
    # Back-compat: an older/unpatched server's job response carries no
    # effective_retry; parsing must not raise and must default to None.
    assert Job.from_dict(load("job")).effective_retry is None
    assert Job.from_dict(load("job_detail")).effective_retry is None


def test_job_effective_retry_mistyped_becomes_none() -> None:
    raw = dict(load("job_detail"), effective_retry="not-an-object")
    assert Job.from_dict(raw).effective_retry is None


def test_job_detail_nested_steps_and_counts() -> None:
    job = Job.from_dict(load("job_detail"))
    assert job.steps is not None
    assert len(job.steps) == 2
    assert all(isinstance(s, Step) for s in job.steps)
    setup, render = job.steps
    assert setup.name == "Setup"
    assert setup.step_order == 0
    assert setup.depends_on == []
    assert render.name == "Render"
    assert render.depends_on == ["018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1a001"]

    counts = job.task_counts
    assert isinstance(counts, TaskCounts)
    assert counts.total == 100
    assert counts.succeeded == 80
    assert counts.ready == 10


def test_step_fields() -> None:
    step = Step.from_dict(load("step"))
    assert step.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1a002"
    assert step.name == "Render"
    assert step.step_order == 1
    assert step.status == "running"
    assert step.depends_on == ["018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1a001"]
    assert step.created_at.tzinfo is not None


def test_task_counts_fields() -> None:
    counts = TaskCounts.from_dict(load("task_counts"))
    assert counts.total == 100
    assert counts.pending == 0
    assert counts.ready == 10
    assert counts.assigned == 5
    assert counts.running == 5
    assert counts.succeeded == 80
    assert counts.failed == 0
    assert counts.canceled == 0


# ── Task ────────────────────────────────────────────────────────────


def test_task_fields() -> None:
    task = Task.from_dict(load("task"))
    assert task.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1b001"
    assert task.job_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345"
    assert task.step_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1a002"
    assert task.name == "Render-1001"
    assert task.parameters == {
        "Frame": "1001",
        "OutputDir": "/mnt/farm/shot_0010/renders",
    }
    assert task.status is TaskStatus.RUNNING
    assert task.assigned_worker_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1c001"
    assert task.assigned_at == datetime(2026, 1, 15, 10, 2, 0, tzinfo=timezone.utc)


def test_task_optional_fields_default() -> None:
    raw = load("task")
    del raw["parameters"]
    del raw["assigned_worker_id"]
    raw["assigned_at"] = None
    task = Task.from_dict(raw)
    assert task.parameters == {}
    assert task.assigned_worker_id is None
    assert task.assigned_at is None


def test_task_failure_reason_parsed() -> None:
    raw = dict(load("task"), failure_reason="worker not configured for staging")
    task = Task.from_dict(raw)
    assert task.failure_reason == "worker not configured for staging"


def test_task_failure_reason_absent_defaults_to_none() -> None:
    # Back-compat: an older/unpatched server's task response carries no
    # failure_reason; parsing must not raise and must default to None.
    assert Task.from_dict(load("task")).failure_reason is None


# ── Worker, GPUInfo, CurrentTask ────────────────────────────────────


def test_worker_fields() -> None:
    worker = Worker.from_dict(load("worker"))
    assert worker.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1c001"
    assert worker.farm_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10001"
    assert worker.queue_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10002"
    assert worker.hostname == "render-node-01"
    assert worker.ip_address == "10.0.0.5"
    assert worker.compute_location == "on-prem"
    assert worker.os == "linux"
    assert worker.os_version == "5.15.0-101-generic"
    assert worker.version == "v0.1.0"
    assert worker.cpu_count == 64
    assert worker.ram_mb == 131072
    assert worker.tags == {"arnold": "7.2.1", "gpu": "true"}
    assert worker.status is WorkerStatus.ONLINE
    assert worker.removable is False
    assert worker.last_heartbeat_at is not None
    assert worker.current_tasks == []


def test_worker_removable_defaults_false_when_absent() -> None:
    raw = load("worker")
    del raw["removable"]
    assert Worker.from_dict(raw).removable is False


def test_worker_version_none_when_absent() -> None:
    raw = load("worker")
    del raw["version"]
    assert Worker.from_dict(raw).version is None


def test_worker_gpu_nested() -> None:
    worker = Worker.from_dict(load("worker"))
    assert isinstance(worker.gpu, GPUInfo)
    assert worker.gpu.vendor == "NVIDIA"
    assert worker.gpu.model == "RTX 4090"
    assert worker.gpu.vram_mb == 24576
    assert worker.gpu.count == 2


def test_worker_detail_current_tasks() -> None:
    worker = Worker.from_dict(load("worker_detail"))
    assert len(worker.current_tasks) == 1
    current = worker.current_tasks[0]
    assert isinstance(current, CurrentTask)
    assert current.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1b001"
    assert current.job_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f12345"
    assert current.name == "Render-1001"
    assert current.status is TaskStatus.RUNNING
    assert current.assigned_at == datetime(2026, 1, 15, 10, 2, 0, tzinfo=timezone.utc)


def test_worker_missing_gpu_yields_empty_gpuinfo() -> None:
    raw = load("worker")
    del raw["gpu"]
    worker = Worker.from_dict(raw)
    assert isinstance(worker.gpu, GPUInfo)
    assert worker.gpu.vendor is None
    assert worker.gpu.count is None


# ── ServerVersion ───────────────────────────────────────────────────


def test_server_version_fields() -> None:
    info = ServerVersion.from_dict(load("version"))
    assert info.version == "v0.1.0"
    assert info.commit == "c6ef195"
    assert info.build_date == "2026-06-22T12:00:00Z"
    assert info.go_version == "go1.24.0"


def test_server_version_mistyped_fields_fall_back_to_empty() -> None:
    info = ServerVersion.from_dict({"version": 1, "commit": None})
    assert info.version == ""
    assert info.commit == ""
    assert info.build_date == ""
    assert info.go_version == ""


# ── Farm, Queue, StorageLocation, UsagePool ─────────────────────────


def test_farm_fields() -> None:
    farm = Farm.from_dict(load("farm"))
    assert farm.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10001"
    assert farm.name == "studio-a"
    assert farm.description == "Studio A render farm"
    assert farm.max_concurrent_tasks == 200
    assert farm.created_at.tzinfo is not None


def test_farm_retry_policy_fields_default_when_absent() -> None:
    farm = Farm.from_dict(load("farm"))
    assert farm.max_attempts is None
    assert farm.retry_delay_seconds is None
    assert farm.failure_limit is None


def test_farm_retry_policy_fields_parsed_when_present() -> None:
    raw = dict(load("farm"), max_attempts=5, retry_delay_seconds=30, failure_limit=25)
    farm = Farm.from_dict(raw)
    assert farm.max_attempts == 5
    assert farm.retry_delay_seconds == 30
    assert farm.failure_limit == 25


def test_queue_fields() -> None:
    queue = Queue.from_dict(load("queue"))
    assert queue.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10002"
    assert queue.farm_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10001"
    assert queue.name == "renders"
    assert queue.description == "Main render queue"
    assert queue.priority == 50
    assert queue.max_concurrent_tasks == 200
    assert queue.paused is False


def test_queue_retry_policy_fields_default_when_absent() -> None:
    queue = Queue.from_dict(load("queue"))
    assert queue.max_attempts is None
    assert queue.retry_delay_seconds is None
    assert queue.failure_limit is None


def test_queue_retry_policy_fields_parsed_when_present() -> None:
    raw = dict(load("queue"), max_attempts=5, retry_delay_seconds=30, failure_limit=25)
    queue = Queue.from_dict(raw)
    assert queue.max_attempts == 5
    assert queue.retry_delay_seconds == 30
    assert queue.failure_limit == 25


def test_storage_location_fields() -> None:
    loc = StorageLocation.from_dict(load("storage_location"))
    assert loc.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10003"
    assert loc.name == "project-root"
    assert loc.type == "filesystem"
    assert loc.description == "Primary project storage"
    assert loc.roots == {
        "on-prem": "/mnt/farm",
        "aws-us-east-1": "s3://sqi-bucket/root",
    }


def test_usage_pool_fields() -> None:
    pool = UsagePool.from_dict(load("usage_pool"))
    assert pool.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f10004"
    assert pool.name == "arnold-pool"
    assert pool.server_hint == "27000@licsrv.studio.internal"
    assert pool.max_concurrent == 10


# ── LogChunk, LogPage ───────────────────────────────────────────────


def test_log_chunk_fields() -> None:
    chunk = LogChunk.from_dict(load("log_chunk"))
    assert chunk.id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1d001"
    assert chunk.task_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1b001"
    assert chunk.attempt_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1e001"
    assert chunk.seq_num == 42
    assert chunk.nats_seq == 1001
    assert chunk.stream == "stdout"
    assert chunk.data == "Frame 0042 rendered in 3.2s\n"
    assert chunk.at.tzinfo is not None


def test_log_page_wraps_chunks_with_cursor() -> None:
    page = LogPage.from_dict(load("log_page"))
    assert page.after_nats_seq == 1002
    assert page.limit == 100
    assert len(page.items) == 2
    assert all(isinstance(c, LogChunk) for c in page.items)
    # Items preserve server order (ascending nats_seq).
    assert [c.nats_seq for c in page.items] == [1001, 1002]
    assert page.items[1].stream == "stderr"


# ── TaskAttempt ───────────────────────────────────────────────────────


def test_task_attempt_from_dict() -> None:
    a = TaskAttempt.from_dict(load("task_attempt"))
    assert a.attempt_number == 1
    assert a.status == "failed"
    assert a.worker_id == "018f1a2b-3c4d-7e5f-a6b7-c8d9e0f1c001"
    assert a.exit_code == 1
    assert a.message == "worker not configured for staging"
    assert a.started_at.tzinfo is not None
    assert a.ended_at is not None
    assert a.ended_at.tzinfo is not None


def test_task_attempt_running_has_no_terminal_fields() -> None:
    a = TaskAttempt.from_dict(
        {
            "attempt_number": 2,
            "status": "running",
            "started_at": "2026-07-11T19:00:02Z",
        }
    )
    assert a.attempt_number == 2
    assert a.status == "running"
    assert a.worker_id is None
    assert a.exit_code is None
    assert a.message is None
    assert a.ended_at is None


# ── CancelResult ────────────────────────────────────────────────────


def test_cancel_result_from_dict() -> None:
    from sqi_client.models import CancelResult, TaskStatus

    result = CancelResult.from_dict({"task_id": "t1", "status": "canceled"})
    assert result.task_id == "t1"
    assert result.status is TaskStatus.CANCELED


def test_cancel_result_tolerates_missing_fields() -> None:
    from sqi_client.models import CancelResult

    result = CancelResult.from_dict({})
    assert result.task_id == ""


# ── RetryJobResult ──────────────────────────────────────────────────


def test_retry_job_result_from_dict_tolerant() -> None:
    from sqi_client.models import RetryJobResult

    r = RetryJobResult.from_dict({"job_id": "j1", "retried": 2, "extra": "ignored"})
    assert r == RetryJobResult(job_id="j1", retried=2)
    # Missing fields fall back to type defaults.
    assert RetryJobResult.from_dict({}) == RetryJobResult(job_id="", retried=0)


# ── Product, ProductParameter, ParameterUserInterface ───────────────


def test_product_from_dict() -> None:
    from sqi_client.models import Product

    p = Product.from_dict(
        {
            "name": "blender",
            "title": "Blender",
            "description": "Render a .blend",
            "category": "render",
            "version": "1.0.0",
            "source": "builtin",
            "template": "specificationVersion: jobtemplate-2023-09\n",
            "format": "yaml",
        }
    )
    assert p.name == "blender"
    assert p.source == "builtin"
    assert p.format == "yaml"


def test_product_parses_readme() -> None:
    from sqi_client.models import Product

    p = Product.from_dict({"name": "probe", "readme": "# Docs\n\nBody.\n"})
    assert p.readme == "# Docs\n\nBody.\n"


def test_product_readme_defaults_empty() -> None:
    from sqi_client.models import Product

    assert Product.from_dict({"name": "probe"}).readme == ""


def test_product_readme_tolerates_mistyped_value() -> None:
    # Matches the module's tolerant-parsing contract: a mistyped field falls
    # back to a type-appropriate default rather than raising.
    from sqi_client.models import Product

    assert Product.from_dict({"name": "probe", "readme": 17}).readme == ""


def test_product_parameter_from_dict_with_user_interface() -> None:
    from sqi_client.models import ProductParameter

    param = ProductParameter.from_dict(
        {
            "name": "Quality",
            "type": "STRING",
            "default": "final",
            "allowed_values": ["draft", "final"],
            "user_interface": {
                "control": "DROPDOWN_LIST",
                "label": "Render quality",
                "group_label": "Output",
            },
        }
    )
    assert param.name == "Quality"
    assert param.default == "final"
    assert param.allowed_values == ["draft", "final"]
    assert param.user_interface is not None
    assert param.user_interface.control == "DROPDOWN_LIST"
    assert param.user_interface.group_label == "Output"


def test_product_parameter_from_dict_without_user_interface() -> None:
    from sqi_client.models import ProductParameter

    param = ProductParameter.from_dict({"name": "Frames", "type": "INT"})
    assert param.user_interface is None
    assert param.default is None


def test_product_parameter_from_dict_with_file_filters() -> None:
    from sqi_client.models import ProductParameter

    param = ProductParameter.from_dict(
        {
            "name": "SceneFile",
            "type": "PATH",
            "file_filters": [
                {"label": "Maya Scene", "patterns": ["*.ma", "*.mb"]},
                {"label": "All Files", "patterns": ["*"]},
            ],
            "file_filter_default": {"label": "Maya Scene", "patterns": ["*.ma", "*.mb"]},
        }
    )
    assert param.file_filters is not None
    assert [f.label for f in param.file_filters] == ["Maya Scene", "All Files"]
    assert param.file_filters[0].patterns == ["*.ma", "*.mb"]
    assert param.file_filter_default is not None
    assert param.file_filter_default.label == "Maya Scene"
    assert param.file_filter_default.patterns == ["*.ma", "*.mb"]


def test_product_parameter_from_dict_with_null_file_filters() -> None:
    """The server marshals a nil slice/pointer as JSON null, not an absent key."""
    from sqi_client.models import ProductParameter

    param = ProductParameter.from_dict(
        {"name": "SceneFile", "type": "PATH", "file_filters": None, "file_filter_default": None}
    )
    assert param.file_filters is None
    assert param.file_filter_default is None


def test_path_file_filter_from_dict() -> None:
    from sqi_client.models import PathFileFilter

    f = PathFileFilter.from_dict({"label": "Maya Scene", "patterns": ["*.ma", "*.mb"], "extra": 1})
    assert f == PathFileFilter(label="Maya Scene", patterns=["*.ma", "*.mb"])
    # Missing fields fall back to type defaults.
    assert PathFileFilter.from_dict({}) == PathFileFilter(label="", patterns=[])
