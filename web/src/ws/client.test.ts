// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { WebSocketClient } from './client'
import type { ConnectionState } from './types'

// ── Mock WebSocket ────────────────────────────────────────────────────────────

class MockWebSocket {
  static instances: MockWebSocket[] = []
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3

  readyState = MockWebSocket.CONNECTING
  url: string
  sentMessages: string[] = []

  onopen: ((event: Event) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null

  constructor(url: string) {
    this.url = url
    MockWebSocket.instances.push(this)
  }

  send(data: string): void {
    this.sentMessages.push(data)
  }

  close(): void {
    this.readyState = MockWebSocket.CLOSED
  }

  // Test helpers

  simulateOpen(): void {
    this.readyState = MockWebSocket.OPEN
    this.onopen?.(new Event('open'))
  }

  simulateClose(): void {
    this.readyState = MockWebSocket.CLOSED
    this.onclose?.(new CloseEvent('close'))
  }

  simulateMessage(data: unknown): void {
    const raw = typeof data === 'string' ? data : JSON.stringify(data)
    this.onmessage?.(new MessageEvent('message', { data: raw }))
  }

  simulateError(): void {
    this.onerror?.(new Event('error'))
  }
}

function instance(i: number): MockWebSocket {
  const ws = MockWebSocket.instances[i]
  if (!ws) throw new Error(`No MockWebSocket at index ${i}`)
  return ws
}

// ── Setup / teardown ──────────────────────────────────────────────────────────

beforeEach(() => {
  MockWebSocket.instances = []
  vi.stubGlobal('WebSocket', MockWebSocket)
  vi.useFakeTimers()
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
})

function makeConnected(): [WebSocketClient, MockWebSocket] {
  const client = new WebSocketClient('ws://test')
  client.connect()
  const ws = instance(0)
  ws.simulateOpen()
  return [client, ws]
}

// ── Subscription dispatch ─────────────────────────────────────────────────────

describe('subscription dispatch', () => {
  it('calls the handler when a push arrives for the subscribed subject', () => {
    const [client, ws] = makeConnected()
    const handler = vi.fn()
    client.subscribe('jobs', handler)

    ws.simulateMessage({ type: 'push', subject: 'jobs', payload: { id: 'j1' }, seq: 1 })

    expect(handler).toHaveBeenCalledExactlyOnceWith(
      { id: 'j1' },
      expect.objectContaining({ type: 'push', subject: 'jobs', seq: 1 }),
    )
  })

  it('does not call the handler for a different subject', () => {
    const [client, ws] = makeConnected()
    const handler = vi.fn()
    client.subscribe('jobs', handler)

    ws.simulateMessage({ type: 'push', subject: 'workers', payload: {}, seq: 1 })

    expect(handler).not.toHaveBeenCalled()
  })

  it('dispatches to all handlers registered for the same subject', () => {
    const [client, ws] = makeConnected()
    const h1 = vi.fn()
    const h2 = vi.fn()
    client.subscribe('jobs', h1)
    client.subscribe('jobs', h2)

    ws.simulateMessage({ type: 'push', subject: 'jobs', payload: {}, seq: 1 })

    expect(h1).toHaveBeenCalledOnce()
    expect(h2).toHaveBeenCalledOnce()
  })

  it('sends a subscribe wire message when the first handler for a subject is added', () => {
    const [client, ws] = makeConnected()
    client.subscribe('workers', vi.fn())

    const raw0 = ws.sentMessages[0]
    expect(raw0).toBeDefined()
    const msg = JSON.parse(raw0 as string) as { type: string; subject: string }
    expect(msg.type).toBe('subscribe')
    expect(msg.subject).toBe('workers')
  })

  it('does not send a second subscribe message when a duplicate handler is added', () => {
    const [client, ws] = makeConnected()
    const h = vi.fn()
    client.subscribe('jobs', h)
    client.subscribe('jobs', vi.fn())

    const subs = ws.sentMessages.filter(
      (m) => (JSON.parse(m) as { type: string }).type === 'subscribe',
    )
    expect(subs).toHaveLength(1)
  })
})

// ── Handler cleanup on unsubscribe ────────────────────────────────────────────

describe('handler cleanup on unsubscribe', () => {
  it('stops calling a handler after it is unsubscribed', () => {
    const [client, ws] = makeConnected()
    const handler = vi.fn()
    client.subscribe('jobs', handler)
    client.unsubscribe('jobs', handler)

    ws.simulateMessage({ type: 'push', subject: 'jobs', payload: {}, seq: 1 })

    expect(handler).not.toHaveBeenCalled()
  })

  it('sends an unsubscribe wire message when the last handler is removed', () => {
    const [client, ws] = makeConnected()
    const handler = vi.fn()
    client.subscribe('jobs', handler)
    client.unsubscribe('jobs', handler)

    const raw1 = ws.sentMessages[1]
    expect(raw1).toBeDefined()
    const unsubMsg = JSON.parse(raw1 as string) as { type: string; subject: string }
    expect(unsubMsg.type).toBe('unsubscribe')
    expect(unsubMsg.subject).toBe('jobs')
  })

  it('keeps remaining handlers when only one of multiple is removed', () => {
    const [client, ws] = makeConnected()
    const h1 = vi.fn()
    const h2 = vi.fn()
    client.subscribe('jobs', h1)
    client.subscribe('jobs', h2)
    client.unsubscribe('jobs', h1)

    ws.simulateMessage({ type: 'push', subject: 'jobs', payload: {}, seq: 1 })

    expect(h1).not.toHaveBeenCalled()
    expect(h2).toHaveBeenCalledOnce()
  })

  it('does not send unsubscribe when other handlers remain on the subject', () => {
    const [client, ws] = makeConnected()
    const h1 = vi.fn()
    const h2 = vi.fn()
    client.subscribe('jobs', h1)
    client.subscribe('jobs', h2)
    const msgsBefore = ws.sentMessages.length
    client.unsubscribe('jobs', h1)

    const newMsgs = ws.sentMessages.slice(msgsBefore)
    const hasSub = newMsgs.some((m) => (JSON.parse(m) as { type: string }).type === 'unsubscribe')
    expect(hasSub).toBe(false)
  })

  it('ignores unsubscribe calls for unknown subjects', () => {
    const [client] = makeConnected()
    expect(() => client.unsubscribe('unknown', vi.fn())).not.toThrow()
  })
})

// ── Reconnect sequencing ──────────────────────────────────────────────────────

describe('reconnect sequencing', () => {
  it('transitions: connecting → connected → reconnecting on close', () => {
    const states: ConnectionState[] = []
    const client = new WebSocketClient('ws://test')
    client.onStateChange((s) => states.push(s))
    client.connect()
    instance(0).simulateOpen()
    instance(0).simulateClose()

    expect(states).toContain('connected')
    expect(states).toContain('reconnecting')
  })

  it('creates a new socket after the first backoff delay', () => {
    const client = new WebSocketClient('ws://test')
    client.connect()
    instance(0).simulateOpen()
    instance(0).simulateClose()

    expect(MockWebSocket.instances).toHaveLength(1)
    vi.advanceTimersByTime(1_000)
    expect(MockWebSocket.instances).toHaveLength(2)
  })

  it('resets the reconnect counter on successful reconnect', () => {
    const client = new WebSocketClient('ws://test')
    client.connect()
    instance(0).simulateOpen()
    instance(0).simulateClose()

    vi.advanceTimersByTime(1_000)
    instance(1).simulateOpen()

    expect(client.getState()).toBe('connected')
  })

  it('re-subscribes all active subjects after reconnect', () => {
    const client = new WebSocketClient('ws://test')
    client.connect()
    const ws0 = instance(0)
    ws0.simulateOpen()
    client.subscribe('jobs', vi.fn())
    client.subscribe('workers', vi.fn())

    ws0.simulateClose()
    vi.advanceTimersByTime(1_000)
    const ws1 = instance(1)
    ws1.simulateOpen()

    const subjects = ws1.sentMessages
      .map((m) => JSON.parse(m) as { type: string; subject: string })
      .filter((m) => m.type === 'subscribe')
      .map((m) => m.subject)

    expect(subjects).toContain('jobs')
    expect(subjects).toContain('workers')
  })

  it('transitions to failed after max reconnect attempts are exhausted', () => {
    const states: ConnectionState[] = []
    const client = new WebSocketClient('ws://test')
    client.onStateChange((s) => states.push(s))
    client.connect()

    instance(0).simulateOpen()
    instance(0).simulateClose()

    for (let i = 1; i <= 10; i++) {
      vi.advanceTimersByTime(30_001)
      instance(i).simulateClose()
    }

    expect(client.getState()).toBe('failed')
  })

  it('does not reconnect after disconnect() is called', () => {
    const client = new WebSocketClient('ws://test')
    client.connect()
    instance(0).simulateOpen()
    client.disconnect()

    vi.advanceTimersByTime(30_000)
    expect(MockWebSocket.instances).toHaveLength(1)
    expect(client.getState()).toBe('disconnected')
  })
})

// ── Malformed message handling ────────────────────────────────────────────────

describe('malformed message handling', () => {
  it('does not throw on invalid JSON', () => {
    const [, ws] = makeConnected()
    expect(() => ws.simulateMessage('not-valid-json!!')).not.toThrow()
  })

  it('logs an error on invalid JSON', () => {
    const [, ws] = makeConnected()
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    ws.simulateMessage('not-valid-json!!')
    expect(spy).toHaveBeenCalledWith('[sqi/ws] failed to parse message:', 'not-valid-json!!')
  })

  it('does not throw when the envelope is missing the type field', () => {
    const [, ws] = makeConnected()
    expect(() => ws.simulateMessage({ seq: 1 })).not.toThrow()
  })

  it('logs an error when the envelope shape is invalid', () => {
    const [, ws] = makeConnected()
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})
    ws.simulateMessage({ seq: 1 })
    expect(spy).toHaveBeenCalledWith('[sqi/ws] invalid envelope shape:', JSON.stringify({ seq: 1 }))
  })

  it('does not call subject handlers for non-push message types', () => {
    const [client, ws] = makeConnected()
    const handler = vi.fn()
    client.subscribe('jobs', handler)

    ws.simulateMessage({ type: 'ack', payload: { client_seq: 1 }, seq: 0 })
    ws.simulateMessage({ type: 'pong', seq: 0 })

    expect(handler).not.toHaveBeenCalled()
  })

  it('logs server error envelopes and does not dispatch to handlers', () => {
    const [client, ws] = makeConnected()
    const handler = vi.fn()
    client.subscribe('jobs', handler)
    const spy = vi.spyOn(console, 'error').mockImplementation(() => {})

    ws.simulateMessage({
      type: 'error',
      payload: { code: 'invalid_subject', message: 'bad subject' },
      seq: 0,
    })

    expect(spy).toHaveBeenCalled()
    expect(handler).not.toHaveBeenCalled()
  })

  it('ignores push messages that have no subject', () => {
    const [client, ws] = makeConnected()
    const handler = vi.fn()
    client.subscribe('jobs', handler)

    ws.simulateMessage({ type: 'push', payload: {}, seq: 1 })

    expect(handler).not.toHaveBeenCalled()
  })
})
