#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Submit an OpenJD template file to a farm/queue and wait for it to finish.

Usage:
    python submit_job.py <server-url> <farm-id> <queue-id> <template.yaml>

Example:
    python submit_job.py http://localhost:8080 farm-1 queue-1 render.yaml
"""

from __future__ import annotations

import sys
from pathlib import Path

from sqi_client import JobStatus, SqiClient


def main(argv: list[str]) -> int:
    if len(argv) != 5:
        print(__doc__)
        return 2
    server_url, farm_id, queue_id, template = argv[1], argv[2], argv[3], argv[4]

    with SqiClient(server_url) as sqi:
        # submit_and_wait composes submit_job + wait_for_job: it accepts the
        # template as a path (read from disk), a YAML/JSON string, or a dict.
        job = sqi.submit_and_wait(
            Path(template),
            farm_id=farm_id,
            queue_id=queue_id,
            owner="submit_job.py",
            poll_interval=2.0,
            timeout=3600,
        )
        print(f"job {job.id} finished: {job.status}")

        # Print each task's captured output.
        for task in sqi.iter_job_tasks(job.id):
            logs = sqi.get_task_logs(task.id)
            print(f"--- task {task.name} ({task.status}) ---")
            print("".join(chunk.data for chunk in logs.items), end="")

    return 0 if job.status == JobStatus.COMPLETED else 1


# Cross-job dependencies: pass depends_on=[<upstream job id>] to submit_job /
# submit_and_wait to hold a job until its upstreams finish (e.g. a comp job
# that must wait for a render job). The dependency job must already exist, be
# in the same farm, and not already be failed/canceled; while any dependency
# is unfinished the new job is returned in the "blocked" status rather than
# "pending", and starts once every dependency completes. For example:
#
#     with SqiClient(server_url) as sqi:
#         render = sqi.submit_job(
#             "render.yaml", farm_id=farm_id, queue_id=queue_id
#         )
#         comp = sqi.submit_job(
#             "comp.yaml",
#             farm_id=farm_id,
#             queue_id=queue_id,
#             depends_on=[render.id],
#         )


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
