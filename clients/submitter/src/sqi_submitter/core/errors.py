# SPDX-License-Identifier: AGPL-3.0-or-later
"""One error translation layer so Qt and bpy UIs share identical error text."""

from __future__ import annotations

import contextlib
from collections.abc import Iterator

from sqi_client.errors import (
    APIError,
    NotFoundError,
    SqiConnectionError,
    SqiError,
    SqiTimeoutError,
    ValidationError,
)


class SubmitterError(Exception):
    """An error with a message fit to show an artist."""

    def __init__(self, user_message: str) -> None:
        super().__init__(user_message)
        self.user_message = user_message


class FormInvalidError(SubmitterError):
    """Client-side validation failed; per-field messages in ``errors``."""

    def __init__(self, errors: dict[str, str]) -> None:
        super().__init__("Please fix the highlighted fields.")
        self.errors = errors


@contextlib.contextmanager
def translate_errors(server_url: str) -> Iterator[None]:
    """Map sqi-sdk exceptions to SubmitterError with artist-facing text."""
    try:
        yield
    except (SqiConnectionError, SqiTimeoutError) as exc:
        raise SubmitterError(
            f"Cannot reach the sqi server at {server_url}. Check the server URL in Settings."
        ) from exc
    except ValidationError as exc:
        raise SubmitterError(str(exc)) from exc
    except NotFoundError as exc:
        raise SubmitterError(f"{exc} — it no longer exists on the server.") from exc
    except (APIError, SqiError) as exc:
        raise SubmitterError(str(exc)) from exc
