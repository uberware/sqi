# SPDX-License-Identifier: AGPL-3.0-or-later
"""Server session, per-user settings, and product grouping for submitter UIs."""

from __future__ import annotations

import json
import os
from collections.abc import Sequence
from pathlib import Path
from typing import Any

from sqi_client import SqiClient
from sqi_client.models import Farm, Job, Product, ProductParameter, Queue
from sqi_submitter.core.errors import translate_errors
from sqi_submitter.core.joboptions import JobOptions

DEFAULT_SERVER_URL = "http://localhost:8080"


class Settings:
    """Tiny JSON key-value store at ~/.sqi/submitter.json (env-overridable)."""

    def __init__(self, path: str | None = None) -> None:
        self._path = Path(
            path
            or os.environ.get("SQI_SUBMITTER_SETTINGS")
            or Path.home() / ".sqi" / "submitter.json"
        )

    def _load(self) -> dict[str, Any]:
        try:
            data = json.loads(self._path.read_text(encoding="utf-8"))
            return data if isinstance(data, dict) else {}
        except (OSError, ValueError):
            return {}

    def get(self, key: str, default: Any = None) -> Any:
        return self._load().get(key, default)

    def set(self, key: str, value: Any) -> None:
        data = self._load()
        data[key] = value
        self._path.parent.mkdir(parents=True, exist_ok=True)
        self._path.write_text(json.dumps(data, indent=2), encoding="utf-8")


def resolve_server_url(explicit: str | None, settings: Settings) -> str:
    """explicit arg → $SQI_SERVER_URL → settings → default."""
    if explicit:
        return explicit
    env = os.environ.get("SQI_SERVER_URL")
    if env:
        return env
    stored = settings.get("server_url")
    if stored:
        return str(stored)
    return DEFAULT_SERVER_URL


class SubmitterSession:
    """Everything a submitter UI needs from the server, error-translated."""

    def __init__(
        self,
        server_url: str | None = None,
        client: SqiClient | None = None,
        settings: Settings | None = None,
    ) -> None:
        self.settings = settings or Settings()
        self.server_url = resolve_server_url(server_url, self.settings)
        self.client = client or SqiClient(self.server_url)

    def products(self) -> list[Product]:
        with translate_errors(self.server_url):
            return self.client.list_products()

    def parameters(self, name: str) -> list[ProductParameter]:
        with translate_errors(self.server_url):
            return self.client.get_product_parameters(name)

    def farms(self) -> list[Farm]:
        with translate_errors(self.server_url):
            return self.client.list_farms()

    def queues(self, farm_id: str) -> list[Queue]:
        with translate_errors(self.server_url):
            return list(self.client.iter_queues(farm_id=farm_id))

    def submit(
        self,
        product_name: str,
        *,
        parameters: dict[str, str],
        farm_id: str,
        queue_id: str,
        job_name: str | None = None,
        job_options: JobOptions | None = None,
    ) -> Job:
        opts = job_options or JobOptions()
        with translate_errors(self.server_url):
            return self.client.submit_product_job(
                product_name,
                farm_id=farm_id,
                queue_id=queue_id,
                job_name=job_name,
                parameters=parameters,
                owner=opts.owner,
                priority=opts.priority,
                project=opts.project,
                max_attempts=opts.max_attempts,
                retry_delay_seconds=opts.retry_delay_seconds,
                failure_limit=opts.failure_limit,
            )


def group_products(
    products: Sequence[Product], host_token: str
) -> tuple[list[Product], list[Product]]:
    """Split into (suggested, rest) by a cosmetic host-token match.

    Nothing is ever hidden: ordering only. Pre-fill is independent of this.
    """
    token = host_token.lower()
    if not token:
        return [], list(products)
    suggested = [
        p
        for p in products
        if token in p.name.lower() or token in p.title.lower() or token in p.description.lower()
    ]
    rest = [p for p in products if p not in suggested]
    return suggested, rest
