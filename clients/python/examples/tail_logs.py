#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tail a task's log output.

With the ``ws`` extra installed (``pip install 'sqi-sdk[ws]'``) this streams
chunks live over WebSocket until the connection closes or you interrupt it
(Ctrl-C). Without the extra it falls back to polling, which stops automatically
once the task reaches a terminal state.

Usage:
    python tail_logs.py <server-url> <task-id>

Example:
    python tail_logs.py http://localhost:8080 018f1a2b-...-task
"""

from __future__ import annotations

import sys

from sqi_client import SqiClient


def _tail(sqi: SqiClient, task_id: str) -> None:
    try:
        # Live WebSocket tail (requires the `ws` extra). A live stream: it runs
        # until the connection closes or the caller stops iterating.
        for chunk in sqi.tail_task_logs_live(task_id):
            print(chunk.data, end="", flush=True)
    except ImportError:
        # `ws` extra not installed — poll instead. follow=True stops once the
        # task reaches a terminal state and the final chunks are drained.
        print("(websockets not installed; polling instead)", file=sys.stderr)
        for chunk in sqi.tail_task_logs(task_id, poll_interval=1.0, follow=True):
            print(chunk.data, end="", flush=True)


def main(argv: list[str]) -> int:
    if len(argv) != 3:
        print(__doc__)
        return 2
    server_url, task_id = argv[1], argv[2]

    with SqiClient(server_url) as sqi:
        try:
            _tail(sqi, task_id)
        except KeyboardInterrupt:
            print()  # newline after the interrupt

    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
