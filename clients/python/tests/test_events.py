# SPDX-FileCopyrightText: 2026 Uberware Inc. <https://uberware.net>
# SPDX-License-Identifier: AGPL-3.0-or-later
"""Tests for the optional WebSocket event stream.

The lazy-import tests simulate the ``ws`` extra being absent by
nulling ``websockets`` in ``sys.modules``. The protocol tests run an
in-process ``websockets`` server in a background thread and drive the real
client against it.
"""

from __future__ import annotations

import contextlib
import json
import sys
import threading
import time
from collections.abc import Callable, Iterator
from typing import Any

import pytest
from websockets.sync.server import ServerConnection, serve

from sqi_client.client import SqiClient
from sqi_client.errors import SqiConnectionError, SqiError
from sqi_client.events import Event, SqiEventStream

# ── In-process fake WebSocket server ──────────────────────────────────────────

Handler = Callable[[ServerConnection], None]


@contextlib.contextmanager
def run_ws_server(handler: Handler) -> Iterator[str]:
    """Run ``handler`` as a websockets server, yielding the base http(s) URL.

    The yielded URL is the http base (e.g. ``http://localhost:54321``) so a
    SqiClient built from it derives the matching ``ws://…/api/v1/ws`` endpoint.
    """
    server = serve(handler, "localhost", 0)
    host, port = server.socket.getsockname()[:2]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://{host}:{port}"
    finally:
        server.shutdown()
        thread.join(timeout=2)


@contextlib.contextmanager
def connected_stream(handler: Handler, **kwargs: Any) -> Iterator[SqiEventStream]:
    """Run ``handler`` and yield a connected :class:`SqiEventStream` to it."""
    with run_ws_server(handler) as base:
        stream = SqiEventStream(_ws_url(base), **kwargs)
        stream.connect()
        try:
            yield stream
        finally:
            stream.close()


def _ws_url(base: str) -> str:
    return base.replace("http://", "ws://") + "/api/v1/ws"


def _push(subject: str, seq: int, **payload: Any) -> str:
    return json.dumps({"type": "push", "subject": subject, "payload": payload, "seq": seq})


def _ack(client_seq: int, error: str = "") -> str:
    return json.dumps(
        {"type": "ack", "payload": {"client_seq": client_seq, "error": error}, "seq": 0}
    )


# ── Lazy import behavior ────────────────────────────────────────────


@pytest.fixture
def no_websockets(monkeypatch: pytest.MonkeyPatch) -> None:
    """Make ``import websockets`` fail, simulating the ``ws`` extra being absent."""
    for name in list(sys.modules):
        if name == "websockets" or name.startswith("websockets."):
            monkeypatch.setitem(sys.modules, name, None)


def test_core_client_constructs_without_websockets(no_websockets: None) -> None:
    # The core client never imports websockets; constructing it and building an
    # event stream object both work without the extra installed.
    client = SqiClient("http://sqi.test", max_attempts=1)
    stream = client.events()
    assert isinstance(stream, SqiEventStream)
    client.close()


def test_connect_without_websockets_raises_actionable_error(no_websockets: None) -> None:
    client = SqiClient("http://sqi.test", max_attempts=1)
    stream = client.events()
    with pytest.raises(ImportError, match=r"sqi-sdk\[ws\]"):
        stream.connect()
    client.close()


def test_tail_live_without_websockets_raises_actionable_error(no_websockets: None) -> None:
    client = SqiClient("http://sqi.test", max_attempts=1)
    with pytest.raises(ImportError, match=r"sqi-sdk\[ws\]"):
        next(client.tail_task_logs_live("t1"))
    client.close()


# ── Subscribe / ack / push sequencing + seq tracking ────────────────


def test_subscribe_sends_since_seq_in_payload_and_yields_pushes() -> None:
    received: list[dict[str, Any]] = []

    def handler(ws: ServerConnection) -> None:
        for raw in ws:
            frame = json.loads(raw)
            received.append(frame)
            if frame["type"] == "subscribe":
                ws.send(_ack(frame["seq"]))
                ws.send(_push(frame["subject"], 1, data="a"))
                ws.send(_push(frame["subject"], 2, data="b"))
                return

    with connected_stream(handler, reconnect=False) as stream:
        stream.subscribe("tasks/t1/logs", since_seq=5)
        events = list(stream)

    subscribe = next(f for f in received if f["type"] == "subscribe")
    # since_seq travels inside payload, per the real server protocol.
    assert subscribe["payload"]["since_seq"] == 5
    # The ack is consumed silently; only pushes are yielded, in order.
    assert [e.seq for e in events] == [1, 2]
    assert [e.payload["data"] for e in events] == ["a", "b"]
    assert all(isinstance(e, Event) for e in events)


def test_reconnect_resumes_from_last_seen_seq() -> None:
    since_seqs: list[int] = []
    connections = {"n": 0}

    def handler(ws: ServerConnection) -> None:
        connections["n"] += 1
        conn_num = connections["n"]
        for raw in ws:
            frame = json.loads(raw)
            if frame["type"] == "subscribe":
                since_seqs.append(frame["payload"]["since_seq"])
                ws.send(_ack(frame["seq"]))
                if conn_num == 1:
                    ws.send(_push(frame["subject"], 1, data="a"))
                    ws.send(_push(frame["subject"], 2, data="b"))
                else:
                    ws.send(_push(frame["subject"], 3, data="c"))
                return  # close → client reconnects

    with connected_stream(
        handler, reconnect=True, reconnect_attempts=3, reconnect_backoff=0.0
    ) as stream:
        stream.subscribe("tasks/t1/logs")
        collected = []
        for event in stream:
            collected.append(event.seq)
            if event.seq == 3:
                break

    assert collected == [1, 2, 3]
    # First subscribe at 0; after the drop, resubscribe resumes from the last
    # seen seq (2), so no events are missed across the reconnect.
    assert since_seqs == [0, 2]


def test_unsubscribe_sends_frame() -> None:
    received: list[dict[str, Any]] = []
    got_unsubscribe = threading.Event()

    def handler(ws: ServerConnection) -> None:
        for raw in ws:
            frame = json.loads(raw)
            received.append(frame)
            if frame["type"] == "subscribe":
                ws.send(_ack(frame["seq"]))
            elif frame["type"] == "unsubscribe":
                got_unsubscribe.set()
                return

    with connected_stream(handler, reconnect=False) as stream:
        stream.subscribe("workers")
        stream.unsubscribe("workers")
        assert got_unsubscribe.wait(timeout=2)

    unsubscribe = next(f for f in received if f["type"] == "unsubscribe")
    assert unsubscribe["subject"] == "workers"


def test_error_frame_raises_sqi_error() -> None:
    def handler(ws: ServerConnection) -> None:
        for raw in ws:
            frame = json.loads(raw)
            if frame["type"] == "subscribe":
                ws.send(
                    json.dumps(
                        {
                            "type": "error",
                            "payload": {"code": "invalid_subject", "message": "bad subject"},
                            "seq": 0,
                        }
                    )
                )
                return

    with connected_stream(handler, reconnect=False) as stream:
        stream.subscribe("bogus")
        with pytest.raises(SqiError, match="invalid_subject"):
            list(stream)


def test_ack_error_raises_sqi_error() -> None:
    def handler(ws: ServerConnection) -> None:
        for raw in ws:
            frame = json.loads(raw)
            if frame["type"] == "subscribe":
                ws.send(_ack(frame["seq"], error="subject is required"))
                return

    with connected_stream(handler, reconnect=False) as stream:
        stream.subscribe("")
        with pytest.raises(SqiError, match="subscription failed"):
            list(stream)


def test_reconnect_exhausted_raises_connection_error() -> None:
    def handler(ws: ServerConnection) -> None:
        for raw in ws:
            frame = json.loads(raw)
            if frame["type"] == "subscribe":
                ws.send(_ack(frame["seq"]))
                ws.send(_push(frame["subject"], 1, data="a"))
                return  # closes this connection

    with run_ws_server(handler) as base:
        stream = SqiEventStream(
            _ws_url(base), reconnect=True, reconnect_attempts=2, reconnect_backoff=0.0
        )
        stream.connect()
        stream.subscribe("workers")
        events = iter(stream)
        assert next(events).seq == 1

    # The server is gone now (context exited); the next event triggers a
    # reconnect that exhausts its attempts and surfaces a connection error.
    with pytest.raises(SqiConnectionError):
        next(events)
    stream.close()


def test_reconnect_storm_is_throttled(monkeypatch: pytest.MonkeyPatch) -> None:
    # A server that accepts then immediately drops every connection must not spin
    # the reconnect loop: a flap shorter than the base backoff triggers a sleep.
    def handler(ws: ServerConnection) -> None:
        for raw in ws:
            frame = json.loads(raw)
            if frame["type"] == "subscribe":
                ws.send(_ack(frame["seq"]))
                return  # close immediately, no pushes

    sleeps: list[float] = []

    class _Stop(Exception):
        pass

    real_sleep = time.sleep

    def fake_sleep(seconds: float) -> None:
        # setattr on "sqi_client.events.time.sleep" patches the stdlib time
        # module object itself, so EVERY time.sleep in the process lands here --
        # including websockets' own server.shutdown(), which sleeps
        # SHUTDOWN_POLLING_INTERVAL while waiting for the serving thread to
        # stop. Raising unconditionally therefore threw _Stop out of the
        # run_ws_server teardown, outside the pytest.raises block, in roughly
        # 8% of runs (5/60 locally) -- and appended a stray 0.1 to sleeps.
        #
        # Hijack only the first call, which is the throttle this test is about,
        # and let every later sleep behave normally.
        if sleeps:
            real_sleep(seconds)
            return
        sleeps.append(seconds)
        raise _Stop  # break the otherwise-endless flap loop after one throttle

    monkeypatch.setattr("sqi_client.events.time.sleep", fake_sleep)

    with run_ws_server(handler) as base:
        stream = SqiEventStream(_ws_url(base), reconnect=True, reconnect_backoff=0.5)
        stream.connect()
        stream.subscribe("workers")
        with pytest.raises(_Stop):
            list(stream)
        stream.close()

    assert sleeps == [0.5]


def test_iterating_unconnected_stream_raises() -> None:
    stream = SqiEventStream("ws://localhost:1/api/v1/ws")
    with pytest.raises(SqiError, match="not connected"):
        next(iter(stream))


def test_subscribe_before_connect_is_queued() -> None:
    # Subscribing before connecting only records the resume cursor; the frame is
    # (re)sent when the connection opens.
    stream = SqiEventStream("ws://localhost:1/api/v1/ws")
    stream.subscribe("workers", since_seq=7)
    # White-box check that the resume cursor was recorded without a connection.
    assert stream._subscriptions == {"workers": 7}


# ── Live log tailing over WebSocket ─────────────────────────────────


def test_tail_task_logs_live_yields_log_chunks() -> None:
    def handler(ws: ServerConnection) -> None:
        for raw in ws:
            frame = json.loads(raw)
            if frame["type"] == "subscribe":
                ws.send(_ack(frame["seq"]))
                for seq, data in [(1, "line one\n"), (2, "line two\n")]:
                    ws.send(
                        _push(
                            frame["subject"],
                            seq,
                            task_id="t1",
                            attempt_id="a1",
                            seq_num=seq,
                            stream="stdout",
                            data=data,
                            at="2026-01-15T10:05:42.123Z",
                        )
                    )
                return

    with run_ws_server(handler) as base:
        client = SqiClient(base, max_attempts=1)
        chunks = []
        for chunk in client.tail_task_logs_live("t1"):
            chunks.append(chunk)
            if len(chunks) == 2:
                break
        client.close()

    assert [c.data for c in chunks] == ["line one\n", "line two\n"]
    assert [c.seq_num for c in chunks] == [1, 2]
    assert chunks[0].stream == "stdout"
    assert chunks[0].at.tzinfo is not None
    # The live payload omits these, so they are zero-valued.
    assert chunks[0].nats_seq == 0
    assert chunks[0].id == ""
