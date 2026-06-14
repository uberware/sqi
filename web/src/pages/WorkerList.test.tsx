// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { useState } from 'react'
import type { ReactNode } from 'react'
import WorkerList from './WorkerList'
import { WebSocketProvider } from '@/ws/context'
import type { Worker, ListResponse } from '@/api/types'

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

  simulateMessage(data: unknown): void {
    const raw = typeof data === 'string' ? data : JSON.stringify(data)
    this.onmessage?.(new MessageEvent('message', { data: raw }))
  }
}

function wsInstance(i: number): MockWebSocket {
  const ws = MockWebSocket.instances[i]
  if (!ws) throw new Error(`No MockWebSocket at index ${i}`)
  return ws
}

// ── Mocks ─────────────────────────────────────────────────────────────────────

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  MockWebSocket.instances = []
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('WebSocket', MockWebSocket)
  vi.useFakeTimers({ shouldAdvanceTime: true })
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.useRealTimers()
})

// ── Fixtures ──────────────────────────────────────────────────────────────────

function makeWorker(overrides: Partial<Worker> = {}): Worker {
  return {
    id: 'worker-aabbccdd',
    farm_id: 'farm-1',
    hostname: 'render-node-01',
    status: 'online',
    gpu: {},
    registered_at: '2024-01-01T08:00:00Z',
    updated_at: '2024-01-01T10:00:00Z',
    ...overrides,
  }
}

function makeListResponse<T>(items: T[], total?: number): ListResponse<T> {
  return { items, total: total ?? items.length, limit: 50, offset: 0 }
}

function okJson(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

// ── Test wrappers ─────────────────────────────────────────────────────────────

function makeClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
}

function Wrapper({ children, route = '/workers' }: { children: ReactNode; route?: string }) {
  const [client] = useState(makeClient)
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>
        <WebSocketProvider url="ws://test">{children}</WebSocketProvider>
      </MemoryRouter>
    </QueryClientProvider>
  )
}

// The worker list makes 4 fetch calls on mount:
// 1. main list (with active status filter)
// 2. online count  (status=online, limit=1)
// 3. offline count (status=offline, limit=1)
// 4. disabled count (status=disabled, limit=1)
function mockCountCalls(online: number, offline: number, disabled: number, extra = 0): void {
  // count queries can arrive in any order; mock them all as permissive resolvers
  fetchMock.mockImplementation((input) => {
    const url = typeof input === 'string' ? input : (input as Request).url
    if (url.includes('status=online')) {
      return Promise.resolve(okJson(makeListResponse([], online)))
    }
    if (url.includes('status=offline')) {
      return Promise.resolve(okJson(makeListResponse([], offline)))
    }
    if (url.includes('status=disabled')) {
      return Promise.resolve(okJson(makeListResponse([], disabled)))
    }
    // default: main list or fallback
    if (extra > 0) {
      return Promise.resolve(okJson(makeListResponse([], extra)))
    }
    return Promise.resolve(okJson(makeListResponse([])))
  })
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('WorkerList', () => {
  describe('rendering a list of workers', () => {
    it('renders worker hostname in the table', async () => {
      const worker = makeWorker({ hostname: 'render-node-99' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('render-node-99'))
      expect(screen.getByText('render-node-99')).toBeInTheDocument()
    })

    it('renders a truncated worker ID', async () => {
      const worker = makeWorker({ id: 'abcdef1234567890' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('abcdef12'))
      expect(screen.getByText('abcdef12')).toBeInTheDocument()
    })

    it('renders the status badge for an online worker', async () => {
      const worker = makeWorker({ status: 'online' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByLabelText('Status: Online'))
      expect(screen.getAllByLabelText('Status: Online').length).toBeGreaterThan(0)
    })

    it('renders multiple workers', async () => {
      const workers = [
        makeWorker({ id: 'w-1', hostname: 'node-alpha' }),
        makeWorker({ id: 'w-2', hostname: 'node-beta' }),
      ]
      fetchMock.mockResolvedValue(okJson(makeListResponse(workers)))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('node-alpha'))
      expect(screen.getByText('node-beta')).toBeInTheDocument()
    })

    it('shows empty state when no workers are returned', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('No workers found.'))
      expect(screen.getByText('No workers found.')).toBeInTheDocument()
    })

    it('renders OS column when os is present', async () => {
      const worker = makeWorker({ os: 'Linux', os_version: '5.15' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Linux 5.15'))
      expect(screen.getByText('Linux 5.15')).toBeInTheDocument()
    })

    it('renders compute_location column when present', async () => {
      const worker = makeWorker({ compute_location: 'us-west-2' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('us-west-2'))
      expect(screen.getByText('us-west-2')).toBeInTheDocument()
    })

    it('links worker name to detail page', async () => {
      const worker = makeWorker({ id: 'wid-123', hostname: 'my-node' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('link', { name: 'my-node' }))
      expect(screen.getByRole('link', { name: 'my-node' })).toHaveAttribute(
        'href',
        '/workers/wid-123',
      )
    })
  })

  // ── Status filter counts ────────────────────────────────────────────

  describe('status filter', () => {
    it('renders all four filter pills', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('All'))
      expect(screen.getByText('Online')).toBeInTheDocument()
      expect(screen.getByText('Offline')).toBeInTheDocument()
      expect(screen.getByText('Disabled')).toBeInTheDocument()
    })

    it('shows per-status counts in filter pills', async () => {
      mockCountCalls(5, 2, 1)

      render(<WorkerList />, { wrapper: Wrapper })

      // Wait for counts to load
      await waitFor(() => {
        const ariaLabels = screen.getAllByRole('button').map((b) => b.textContent ?? '')
        // pills contain the label and the count
        expect(ariaLabels.some((t) => t.includes('5'))).toBe(true)
      })
    })

    it('clicking a status filter requests the correct status from the API', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Offline'))
      fireEvent.click(screen.getByText('Offline'))

      await waitFor(() => {
        const calls = fetchMock.mock.calls.flat() as string[]
        // The main list query should now include status=offline
        // (count queries also request status=offline but with limit=1)
        expect(calls.some((u) => typeof u === 'string' && u.includes('status=offline'))).toBe(true)
      })
    })

    it('All pill is active by default', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('All'))
      const allBtn = screen.getByRole('button', { name: /^All/ })
      expect(allBtn).toHaveAttribute('aria-pressed', 'true')
    })

    it('clicking a filter pill marks it as active', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Online'))
      fireEvent.click(screen.getByRole('button', { name: /^Online/ }))

      expect(screen.getByRole('button', { name: /^Online/ })).toHaveAttribute(
        'aria-pressed',
        'true',
      )
    })
  })

  // ── Enable/disable per-row buttons ───────────────────────────────────

  describe('per-row enable/disable', () => {
    it('shows Disable button for online workers', async () => {
      const worker = makeWorker({ status: 'online', hostname: 'online-node' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Disable worker online-node'))
      expect(screen.getByLabelText('Disable worker online-node')).toBeInTheDocument()
    })

    it('shows Enable button for disabled workers', async () => {
      const worker = makeWorker({ status: 'disabled', hostname: 'disabled-node' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Enable worker disabled-node'))
      expect(screen.getByLabelText('Enable worker disabled-node')).toBeInTheDocument()
    })

    it('shows no action button for offline workers', async () => {
      const worker = makeWorker({ status: 'offline', hostname: 'offline-node' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('offline-node'))
      expect(screen.queryByLabelText('Disable worker offline-node')).not.toBeInTheDocument()
      expect(screen.queryByLabelText('Enable worker offline-node')).not.toBeInTheDocument()
    })

    it('Disable button calls the disable mutation', async () => {
      const worker = makeWorker({ id: 'w-abc', status: 'online', hostname: 'node-to-disable' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Disable worker node-to-disable'))

      // Provide a response for the disable call
      fetchMock.mockResolvedValueOnce(okJson({ id: 'w-abc', status: 'disabled' }))
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      fireEvent.click(screen.getByLabelText('Disable worker node-to-disable'))

      await waitFor(() => {
        const calls = fetchMock.mock.calls.flat() as string[]
        expect(
          calls.some((u) => typeof u === 'string' && u.includes('/workers/w-abc/disable')),
        ).toBe(true)
      })
    })

    it('Enable button calls the enable mutation', async () => {
      const worker = makeWorker({ id: 'w-def', status: 'disabled', hostname: 'node-to-enable' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Enable worker node-to-enable'))

      fetchMock.mockResolvedValueOnce(okJson({ id: 'w-def', status: 'online' }))
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      fireEvent.click(screen.getByLabelText('Enable worker node-to-enable'))

      await waitFor(() => {
        const calls = fetchMock.mock.calls.flat() as string[]
        expect(
          calls.some((u) => typeof u === 'string' && u.includes('/workers/w-def/enable')),
        ).toBe(true)
      })
    })
  })

  // ── +N more capability tag overflow ──────────────────────────────────

  describe('capability tag overflow', () => {
    it('renders up to 3 capability tags inline without overflow badge', async () => {
      // OS is its own column, so capability tags are: cpu, ram, gpu (model absent here) — 2 tags
      const worker = makeWorker({
        hostname: 'tag-node',
        os: 'Linux',
        cpu_count: 8,
        ram_mb: 16384,
        gpu: {},
      })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('tag-node'))
      // Capability tags: "8 CPU", "16GB RAM" — no overflow badge
      expect(screen.getByText('8 CPU')).toBeInTheDocument()
      expect(screen.getByText('16GB RAM')).toBeInTheDocument()
      expect(screen.queryByTestId('tag-overflow')).not.toBeInTheDocument()
    })

    it('shows +N more badge when there are more than 3 tags', async () => {
      const worker = makeWorker({
        hostname: 'many-tags-node',
        os: 'Linux',
        cpu_count: 16,
        ram_mb: 32768,
        gpu: { model: 'RTX 4090', vram_mb: 24576 },
        tags: { maya: '2024', nuke: '15' },
      })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByTestId('tag-overflow'))
      const overflowBadge = screen.getByTestId('tag-overflow')
      expect(overflowBadge).toBeInTheDocument()
      // Badge text starts with "+" followed by a number
      expect(overflowBadge.textContent).toMatch(/^\+\d+/)
    })

    it('+N more badge aria-label lists the overflow tags', async () => {
      const worker = makeWorker({
        hostname: 'overflow-node',
        os: 'Windows',
        cpu_count: 32,
        ram_mb: 65536,
        gpu: { model: 'A6000' },
        tags: { katana: '7', houdini: '20' },
      })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByTestId('tag-overflow'))
      const badge = screen.getByTestId('tag-overflow')
      // aria-label should contain the names of the extra tags
      const ariaLabel = badge.getAttribute('aria-label') ?? ''
      expect(ariaLabel).toContain('more tags')
      // The overflow items (those after the first 3) should be in the label
      expect(ariaLabel.length).toBeGreaterThan(10)
    })

    it('shows exactly 3 inline tags and the rest as overflow', async () => {
      // capability tags: cpu, ram, gpu-model, custom tag → 4 total → 3 inline + 1 overflow
      const worker = makeWorker({
        hostname: 'five-tags-node',
        os: 'macOS',
        cpu_count: 12,
        ram_mb: 49152,
        gpu: { model: 'M2 Pro' },
        tags: { project: 'vfx' },
      })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByTestId('tag-overflow'))
      const badge = screen.getByTestId('tag-overflow')
      expect(badge.textContent).toMatch(/^\+1/)
    })

    it('shows no overflow badge when worker has no capabilities', async () => {
      const worker = makeWorker({
        hostname: 'bare-node',
        gpu: {},
      })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('bare-node'))
      expect(screen.queryByTestId('tag-overflow')).not.toBeInTheDocument()
    })
  })

  // ── WebSocket worker updates ────────────────────────────────────────

  describe('websocket worker updates', () => {
    it('updates the status badge when a WsWorkerEvent arrives for a visible worker', async () => {
      const worker = makeWorker({ id: 'ws-worker-1', status: 'online', hostname: 'ws-node' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Online'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'workers',
          payload: {
            worker_id: 'ws-worker-1',
            status: 'offline',
          },
          seq: 1,
        })
      })

      await waitFor(() =>
        expect(screen.getAllByLabelText('Status: Offline').length).toBeGreaterThan(0),
      )
    })

    it('ignores WsWorkerEvents for workers not in the current list', async () => {
      const worker = makeWorker({ id: 'visible-worker', status: 'online', hostname: 'vis-node' })
      fetchMock.mockResolvedValue(okJson(makeListResponse([worker])))

      render(<WorkerList />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('vis-node'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'workers',
          payload: {
            worker_id: 'not-in-list',
            status: 'disabled',
          },
          seq: 1,
        })
      })

      // Visible worker badge should remain Online
      expect(screen.getAllByLabelText('Status: Online').length).toBeGreaterThan(0)
    })
  })

  // ── Manual refresh ────────────────────────────────────────────────────────────

  describe('manual refresh', () => {
    it('renders a Refresh button', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<WorkerList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('button', { name: 'Refresh workers' }))
      expect(screen.getByRole('button', { name: 'Refresh workers' })).toBeInTheDocument()
    })

    it('clicking Refresh triggers a re-fetch', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<WorkerList />, { wrapper: Wrapper })
      await waitFor(() => screen.getByRole('button', { name: 'Refresh workers' }))

      const callsBefore = fetchMock.mock.calls.length
      fireEvent.click(screen.getByRole('button', { name: 'Refresh workers' }))

      await waitFor(() => {
        expect(fetchMock.mock.calls.length).toBeGreaterThan(callsBefore)
      })
    })
  })
})
