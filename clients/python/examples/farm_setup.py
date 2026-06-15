#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Provision a farm, a queue, a storage location, and a usage pool from scratch.

Creates the four resource families a fresh sqi deployment needs before workers
can run jobs, printing the ID of each created resource.

Usage:
    python farm_setup.py <server-url>

Example:
    python farm_setup.py http://localhost:8080
"""

from __future__ import annotations

import sys

from sqi_client import SqiClient


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(__doc__)
        return 2
    server_url = argv[1]

    with SqiClient(server_url) as sqi:
        farm = sqi.create_farm(
            name="studio-a",
            description="Studio A render farm",
            max_concurrent_tasks=200,
        )
        print(f"farm:            {farm.id}  ({farm.name})")

        # A queue belongs to a farm; higher priority schedules sooner.
        queue = sqi.create_queue(
            farm_id=farm.id,
            name="renders",
            priority=50,
            max_concurrent_tasks=200,
        )
        print(f"queue:           {queue.id}  ({queue.name})")

        # roots maps each compute-location name to its absolute root path.
        location = sqi.create_storage_location(
            name="project-root",
            type="filesystem",
            roots={"on-prem": "/mnt/farm", "aws-us-east-1": "s3://sqi-bucket/root"},
        )
        print(f"storage location:{location.id}  ({location.name})")

        pool = sqi.create_usage_pool(
            name="arnold-pool",
            max_concurrent=10,
            server_hint="27000@licsrv.studio.internal",
        )
        print(f"usage pool:      {pool.id}  ({pool.name})")

    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
