# SPDX-License-Identifier: AGPL-3.0-or-later
"""Per-job override values, kept separate from FormModel (product parameters)."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass
class JobOptions:
    """Optional per-job overrides. Each None field inherits the server default."""

    owner: str | None = None
    priority: int | None = None
    project: str | None = None
    max_attempts: int | None = None
    retry_delay_seconds: int | None = None
    failure_limit: int | None = None
    #: Create the job paused, so nothing runs until it is resumed. Unlike the
    #: fields above it has no "inherit" state: ``False`` sends nothing.
    paused: bool = False

    def is_empty(self) -> bool:
        """True when every override is unset and the job is not paused (nothing to send)."""
        return not self.paused and all(
            v is None
            for v in (
                self.owner,
                self.priority,
                self.project,
                self.max_attempts,
                self.retry_delay_seconds,
                self.failure_limit,
            )
        )
