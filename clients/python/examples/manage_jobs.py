#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""List jobs and demonstrate the management operations on one of them.

Lists running/pending jobs, then (if given a job ID) pauses, reprioritizes,
resumes, and finally cancels it.

Usage:
    python manage_jobs.py <server-url> [job-id]

Example:
    python manage_jobs.py http://localhost:8080
    python manage_jobs.py http://localhost:8080 018f1a2b-...-job
"""

from __future__ import annotations

import sys

from sqi_client import JobStatus, SqiClient


def main(argv: list[str]) -> int:
    if len(argv) not in (2, 3):
        print(__doc__)
        return 2
    server_url = argv[1]
    job_id = argv[2] if len(argv) == 3 else None

    with SqiClient(server_url) as sqi:
        # List the active jobs, newest first, walking every page lazily.
        print("Active jobs:")
        for job in sqi.iter_jobs(sort_by="created_at", sort_dir="desc"):
            if job.status in (JobStatus.PENDING, JobStatus.RUNNING, JobStatus.PAUSED):
                print(f"  {job.id}  {job.status.value:<9}  prio={job.priority}  {job.name}")

        if job_id is None:
            print("\nPass a job ID to exercise pause/resume/priority/cancel.")
            return 0

        # Management operations — each returns the updated job.
        print(f"\nManaging job {job_id}:")
        print("  pause     ->", sqi.pause_job(job_id).status)
        print("  priority  ->", sqi.set_job_priority(job_id, 90).priority)
        print("  resume    ->", sqi.resume_job(job_id).status)
        sqi.cancel_job(job_id)
        print("  cancel    ->", sqi.get_job(job_id).status)

    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
