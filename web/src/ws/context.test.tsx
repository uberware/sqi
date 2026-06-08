// SPDX-License-Identifier: AGPL-3.0-only

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { renderHook, act } from '@testing-library/react'
import type { ReactNode } from 'react'
import { WebSocketProvider, useWebSocket, useConnectionState } from './context'

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
}

function instance(i: number): MockWebSocket {
  const ws = MockWebSocket.instances[i]
  if (!ws) throw new Error(`No MockWebSocket at index ${i}`)
  return ws
}

beforeEach(() => {
  MockWebSocket.instances = []
  vi.stubGlobal('WebSocket', MockWebSocket)
  vi.useFakeTimers()
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
})

// ── Test wrappers ─────────────────────────────────────────────────────────────

function makeWrapper(url = 'ws://test') {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <WebSocketProvider url={url}>{children}</WebSocketProvider>
  }
}

// ── useConnectionState ────────────────────────────────────────────────────────

describe('useConnectionState', () => {
  it('returns connecting while the socket is being established', () => {
    const { result } = renderHook(() => useConnectionState(), { wrapper: makeWrapper() })
    expect(result.current).toBe('connecting')
  })

  it('returns connected once the socket opens', () => {
    const { result } = renderHook(() => useConnectionState(), { wrapper: makeWrapper() })
    act(() => {
      instance(0).simulateOpen()
    })
    expect(result.current).toBe('connected')
  })

  it('returns reconnecting after the socket closes', () => {
    const { result } = renderHook(() => useConnectionState(), { wrapper: makeWrapper() })
    act(() => {
      instance(0).simulateOpen()
      instance(0).simulateClose()
    })
    expect(result.current).toBe('reconnecting')
  })

  it('throws when used outside WebSocketProvider', () => {
    expect(() => renderHook(() => useConnectionState())).toThrow(
      'WebSocket hooks must be used inside WebSocketProvider',
    )
  })
})

// ── useWebSocket ──────────────────────────────────────────────────────────────

describe('useWebSocket', () => {
  it('subscribes the handler and calls it when a push arrives', async () => {
    const handler = vi.fn()
    renderHook(() => useWebSocket('jobs', handler), { wrapper: makeWrapper() })

    act(() => {
      instance(0).simulateOpen()
      instance(0).simulateMessage({ type: 'push', subject: 'jobs', payload: { id: 'j1' }, seq: 1 })
    })

    expect(handler).toHaveBeenCalledWith({ id: 'j1' }, expect.objectContaining({ subject: 'jobs' }))
  })

  it('unsubscribes when the component unmounts', () => {
    const handler = vi.fn()
    const { unmount } = renderHook(() => useWebSocket('jobs', handler), {
      wrapper: makeWrapper(),
    })

    act(() => {
      instance(0).simulateOpen()
    })

    unmount()

    act(() => {
      instance(0).simulateMessage({ type: 'push', subject: 'jobs', payload: {}, seq: 2 })
    })

    expect(handler).not.toHaveBeenCalled()
  })

  it('uses the latest handler reference without resubscribing', () => {
    let count = 0
    const { rerender } = renderHook(() => useWebSocket('jobs', () => count++), {
      wrapper: makeWrapper(),
    })

    act(() => {
      instance(0).simulateOpen()
    })

    rerender()

    act(() => {
      instance(0).simulateMessage({ type: 'push', subject: 'jobs', payload: {}, seq: 1 })
    })

    // Only one subscribe message should have been sent (no resubscribe on rerender)
    const subs = instance(0).sentMessages.filter(
      (m) => (JSON.parse(m) as { type: string }).type === 'subscribe',
    )
    expect(subs).toHaveLength(1)
    expect(count).toBe(1)
  })

  it('throws when used outside WebSocketProvider', () => {
    expect(() => renderHook(() => useWebSocket('jobs', vi.fn()))).toThrow(
      'WebSocket hooks must be used inside WebSocketProvider',
    )
  })
})
