import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { WebSocketProvider } from '@/ws/context'
import App from '@/App'

class MockWebSocket {
  static readonly OPEN = 1
  onopen: null = null
  onclose: null = null
  onerror: null = null
  onmessage: null = null
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  vi.stubGlobal('WebSocket', MockWebSocket)
})

afterEach(() => {
  vi.restoreAllMocks()
})

function wrapper({ children }: { children: React.ReactNode }) {
  return <WebSocketProvider url="ws://test">{children}</WebSocketProvider>
}

describe('App', () => {
  it('renders the primary navigation sidebar', () => {
    render(<App />, { wrapper })
    expect(screen.getByRole('navigation', { name: 'Primary navigation' })).toBeInTheDocument()
  })

  it('renders the main content area', () => {
    render(<App />, { wrapper })
    expect(screen.getByRole('main')).toBeInTheDocument()
  })
})
