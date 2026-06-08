import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
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

const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })

function wrapper({ children }: { children: React.ReactNode }) {
  return (
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <WebSocketProvider url="ws://test">{children}</WebSocketProvider>
      </MemoryRouter>
    </QueryClientProvider>
  )
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

  it('renders the dashboard at the root path', () => {
    render(<App />, { wrapper })
    expect(screen.getByRole('heading', { name: 'Dashboard' })).toBeInTheDocument()
  })
})
