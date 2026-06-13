# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Optional WebSocket live-event stream (requires the ``ws`` extra).

This module is importable without ``websockets`` installed — the dependency is
imported lazily inside the entry points (:meth:`SqiEventStream.connect`), which
raise an actionable :class:`ImportError` when it is missing. So the core client
stays dependency-light while ``pip install 'sqi-client[ws]'`` unlocks live
streaming.

The protocol matches the server's WebSocket contract (``internal/ws`` /
``docs/api.md``): every frame is a JSON envelope ``{type, subject, payload,
seq}``. The client sends ``subscribe`` (with ``since_seq`` inside ``payload``)
and ``unsubscribe`` frames; the server replies with an ``ack`` and then streams
``push`` frames, plus ``error`` frames for protocol failures. Each ``push``
carries a per-subject ``seq`` the client tracks so it can resume — passing the
last seen ``seq`` as ``since_seq`` — across a reconnect without missing events.
"""

from __future__ import annotations

import contextlib
import json
import time
from collections.abc import Iterator, Mapping
from dataclasses import dataclass
from types import TracebackType
from typing import Any

from .errors import SqiConnectionError, SqiError

__all__ = ["Event", "SqiEventStream"]

# Exact remedy surfaced when the optional dependency is missing.
_WS_EXTRA_HINT = (
    "the sqi-client WebSocket features require the 'ws' extra; "
    "install it with: pip install 'sqi-client[ws]'"
)


@dataclass(frozen=True)
class Event:
    """One server push event, parsed from a ``push`` envelope.

    Attributes:
        subject: The subscription subject that generated the event (e.g.
            ``jobs``, ``jobs/{id}/tasks``, ``tasks/{id}/logs``, ``workers``).
        seq: The per-subject sequence number; pass the last seen value as
            ``since_seq`` to resume after a reconnect.
        payload: The decoded event payload. Its shape depends on ``subject``
            (see ``docs/api.md`` for the documented push payloads).
    """

    subject: str
    seq: int
    payload: dict[str, Any]


def _load_ws_connect() -> Any:
    """Import the synchronous ``websockets`` connect function, or raise.

    Raises:
        ImportError: ``websockets`` is not installed, with the exact pip remedy.
    """
    try:
        from websockets.sync.client import connect
    except ImportError as exc:  # pragma: no cover - exercised via monkeypatch in tests
        raise ImportError(_WS_EXTRA_HINT) from exc
    return connect


class SqiEventStream:
    """A synchronous WebSocket subscription to a server's live event feed.

    Construct directly with a WebSocket URL, or via
    :meth:`~sqi_client.client.SqiClient.events`. Use as a context manager (which
    opens the connection on entry and closes it on exit) or manage the lifecycle
    explicitly with :meth:`connect` / :meth:`close`.

    Subscribe to one or more subjects, then iterate the stream to receive
    :class:`Event` objects::

        with client.events() as stream:
            stream.subscribe("workers")
            for event in stream:
                print(event.subject, event.payload)

    The stream tracks the last ``seq`` seen per subject and, when ``reconnect``
    is true (the default), automatically reconnects and re-subscribes with
    ``since_seq`` set to that value after an idle disconnect or server restart,
    so no events are missed across the gap.

    Args:
        ws_url: Full WebSocket endpoint URL, e.g.
            ``ws://localhost:8080/api/v1/ws``.
        headers: Extra headers to send on the upgrade request (the Phase 3 auth
            hook). A ``User-Agent`` here overrides the websockets default.
        reconnect: Auto-reconnect and resubscribe on disconnect (default true).
        open_timeout: Seconds to wait for the connection to open (default 10).
        reconnect_attempts: Reconnect tries before giving up (default 5).
        reconnect_backoff: Base backoff delay in seconds between reconnect
            attempts (doubles each try, default 0.5).
        reconnect_backoff_max: Cap on the reconnect backoff delay (default 30).
    """

    def __init__(
        self,
        ws_url: str,
        *,
        headers: Mapping[str, str] | None = None,
        reconnect: bool = True,
        open_timeout: float = 10.0,
        reconnect_attempts: int = 5,
        reconnect_backoff: float = 0.5,
        reconnect_backoff_max: float = 30.0,
    ) -> None:
        self._ws_url = ws_url
        self._headers = dict(headers) if headers else {}
        self._reconnect = reconnect
        self._open_timeout = open_timeout
        self._reconnect_attempts = max(1, reconnect_attempts)
        self._reconnect_backoff = reconnect_backoff
        self._reconnect_backoff_max = reconnect_backoff_max

        self._conn: Any = None
        # subject → last seen seq (the resume cursor). Seeded with the caller's
        # since_seq at subscribe time, then advanced to each push's seq.
        self._subscriptions: dict[str, int] = {}
        # Client-chosen seq echoed by the server's ack, for correlation.
        self._client_seq = 0
        # Exception types learned from websockets once connected (kept off the
        # module top level so importing this module needs no websockets).
        self._closed_excs: tuple[type[BaseException], ...] = ()
        self._ws_base_exc: type[BaseException] = Exception

    # ── Lifecycle ─────────────────────────────────────────────────────────────

    def __enter__(self) -> SqiEventStream:
        self.connect()
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        self.close()

    def connect(self) -> None:
        """Open the WebSocket connection and (re-)send any active subscriptions.

        Raises:
            ImportError: The ``ws`` extra (``websockets``) is not installed.
        """
        connect_fn = _load_ws_connect()
        from websockets.exceptions import ConnectionClosed, WebSocketException

        self._closed_excs = (ConnectionClosed,)
        self._ws_base_exc = WebSocketException

        # Send our User-Agent via the dedicated parameter so it isn't duplicated
        # alongside the websockets default header.
        user_agent = self._headers.get("User-Agent")
        extra = {k: v for k, v in self._headers.items() if k.lower() != "user-agent"}
        kwargs: dict[str, Any] = {"open_timeout": self._open_timeout}
        if user_agent:
            kwargs["user_agent_header"] = user_agent

        self._conn = connect_fn(self._ws_url, additional_headers=extra or None, **kwargs)

        # Re-establish existing subscriptions (also used on reconnect), resuming
        # from each subject's last seen seq.
        for subject, since_seq in self._subscriptions.items():
            self._send_subscribe(subject, since_seq)

    def close(self) -> None:
        """Close the underlying WebSocket connection (idempotent)."""
        self._close_conn()

    # ── Subscriptions ─────────────────────────────────────────────────────────

    def subscribe(self, subject: str, since_seq: int = 0) -> None:
        """Subscribe to ``subject``, replaying buffered events after ``since_seq``.

        Args:
            subject: The subscription subject (``jobs``, ``jobs/{id}/tasks``,
                ``tasks/{id}/logs``, or ``workers``).
            since_seq: Replay buffered events with ``seq`` greater than this
                before streaming live ones (default 0 — live only).
        """
        self._subscriptions[subject] = since_seq
        if self._conn is not None:
            self._send_subscribe(subject, since_seq)

    def unsubscribe(self, subject: str) -> None:
        """Cancel a subscription and stop receiving its events."""
        self._subscriptions.pop(subject, None)
        if self._conn is not None:
            self._client_seq += 1
            self._send({"type": "unsubscribe", "subject": subject, "seq": self._client_seq})

    def _send_subscribe(self, subject: str, since_seq: int) -> None:
        self._client_seq += 1
        self._send(
            {
                "type": "subscribe",
                "subject": subject,
                "payload": {"since_seq": since_seq},
                "seq": self._client_seq,
            }
        )

    def _send(self, frame: Mapping[str, Any]) -> None:
        if self._conn is None:
            raise SqiError("event stream is not connected; call connect() first")
        self._conn.send(json.dumps(frame))

    # ── Iteration ─────────────────────────────────────────────────────────────

    def __iter__(self) -> Iterator[Event]:
        return self._events()

    def _events(self) -> Iterator[Event]:
        # Grows when connections close almost immediately (a flapping server or
        # proxy), to throttle a reconnect storm; resets after a healthy run.
        storm_delay = 0.0
        while True:
            conn = self._conn
            if conn is None:
                raise SqiError(
                    "event stream is not connected; use it as a context manager "
                    "or call connect() first"
                )
            started = time.monotonic()
            try:
                for message in conn:
                    event = self._handle(message)
                    if event is not None:
                        yield event
            except self._closed_excs:
                # Abnormal close — fall through to the reconnect decision below.
                pass
            # The frame loop ended (graceful close) or raised a close error.
            if not self._reconnect:
                return
            # A connection that lasted less than the base backoff is treated as a
            # flap: back off (growing, capped) before retrying so a server that
            # accepts then immediately drops the connection cannot spin the loop.
            # A connection that ran longer resets the delay.
            if time.monotonic() - started < self._reconnect_backoff:
                storm_delay = min(
                    max(self._reconnect_backoff, storm_delay * 2),
                    self._reconnect_backoff_max,
                )
                time.sleep(storm_delay)
            else:
                storm_delay = 0.0
            self._reconnect_resume()

    def _handle(self, message: Any) -> Event | None:
        """Decode one frame, returning an :class:`Event` for pushes else ``None``.

        Raises:
            SqiError: The server sent an ``error`` frame, or an ``ack`` reporting
                a failed subscription.
        """
        env = json.loads(message)
        if not isinstance(env, dict):
            return None
        frame_type = env.get("type")

        if frame_type == "push":
            subject = env.get("subject")
            subject = subject if isinstance(subject, str) else ""
            seq_raw = env.get("seq")
            seq = seq_raw if isinstance(seq_raw, int) and not isinstance(seq_raw, bool) else 0
            payload_raw = env.get("payload")
            payload = payload_raw if isinstance(payload_raw, dict) else {}
            if subject in self._subscriptions:
                # Advance the resume cursor so a reconnect replays from here.
                self._subscriptions[subject] = seq
            return Event(subject=subject, seq=seq, payload=payload)

        if frame_type == "error":
            payload_raw = env.get("payload")
            payload = payload_raw if isinstance(payload_raw, dict) else {}
            raise SqiError(
                f"server error [{payload.get('code', 'unknown')}]: "
                f"{payload.get('message', '')}".rstrip(": ")
            )

        if frame_type == "ack":
            payload_raw = env.get("payload")
            payload = payload_raw if isinstance(payload_raw, dict) else {}
            err = payload.get("error")
            if err:
                raise SqiError(f"subscription failed: {err}")
            return None

        # pong, or any frame we do not model — ignore.
        return None

    # ── Reconnect ─────────────────────────────────────────────────────────────

    def _reconnect_resume(self) -> None:
        self._close_conn()
        excs = (OSError, self._ws_base_exc)
        delay = self._reconnect_backoff
        for attempt in range(1, self._reconnect_attempts + 1):
            try:
                self.connect()
                return
            except excs as exc:
                if attempt >= self._reconnect_attempts:
                    raise SqiConnectionError(
                        f"failed to reconnect event stream to {self._ws_url}: {exc}"
                    ) from exc
                time.sleep(min(delay, self._reconnect_backoff_max))
                delay *= 2

    def _close_conn(self) -> None:
        if self._conn is not None:
            with contextlib.suppress(Exception):
                self._conn.close()
            self._conn = None
