// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, act } from '@testing-library/react'
import type { ReactNode } from 'react'
import { WebSocketProvider } from '@/ws/context'
import ConnectionStatusBadge from './ConnectionStatusBadge'

// ── Mock WebSocket ────────────────────────────────────────────────────────────

class MockWebSocket {
  static instances: MockWebSocket[] = []
  static readonly OPEN = 1
  static readonly CLOSED = 3
  readyState = 0
  url: string
  onopen: ((e: Event) => void) | null = null
  onclose: ((e: CloseEvent) => void) | null = null
  onerror: ((e: Event) => void) | null = null
  onmessage: ((e: MessageEvent) => void) | null = null

  constructor(url: string) {
    this.url = url
    MockWebSocket.instances.push(this)
  }

  send(): void {}
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

// ── Helpers ───────────────────────────────────────────────────────────────────

function Wrapper({ children }: { children: ReactNode }) {
  return <WebSocketProvider url="ws://test">{children}</WebSocketProvider>
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('ConnectionStatusBadge', () => {
  it('shows Reconnecting while connecting', () => {
    render(<ConnectionStatusBadge />, { wrapper: Wrapper })
    expect(screen.getByRole('status')).toHaveTextContent('Reconnecting')
  })

  it('shows Live when connected', async () => {
    render(<ConnectionStatusBadge />, { wrapper: Wrapper })
    act(() => {
      instance(0).simulateOpen()
    })
    expect(screen.getByRole('status')).toHaveTextContent('Live')
  })

  it('shows Reconnecting after the socket closes', async () => {
    render(<ConnectionStatusBadge />, { wrapper: Wrapper })
    act(() => {
      instance(0).simulateOpen()
      instance(0).simulateClose()
    })
    expect(screen.getByRole('status')).toHaveTextContent('Reconnecting')
  })

  it('shows Disconnected in failed state', async () => {
    render(<ConnectionStatusBadge />, { wrapper: Wrapper })

    act(() => {
      instance(0).simulateOpen()
      instance(0).simulateClose()
    })

    for (let i = 1; i <= 10; i++) {
      act(() => {
        vi.advanceTimersByTime(30_001)
        instance(i).simulateClose()
      })
    }

    expect(screen.getByRole('status')).toHaveTextContent('Disconnected')
  })

  it('has an aria-label describing the WebSocket state', () => {
    render(<ConnectionStatusBadge />, { wrapper: Wrapper })
    const status = screen.getByRole('status')
    expect(status).toHaveAttribute('aria-label')
    expect(status.getAttribute('aria-label')).toMatch(/WebSocket/)
  })
})
