// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { useState } from 'react'
import type { ReactNode } from 'react'
import Dashboard from './Dashboard'
import { WebSocketProvider } from '@/ws/context'
import type { Job, Farm, ListResponse } from '@/api/types'

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

function makeJob(overrides: Partial<Job> = {}): Job {
  return {
    id: 'job-aabbccdd',
    farm_id: 'farm-1',
    queue_id: 'queue-1',
    queue_name: 'default',
    name: 'Test Job',
    owner: 'alice',
    submitter: 'alice',
    priority: 50,
    status: 'failed',
    template_format: 'yaml',
    created_at: '2024-01-01T08:00:00Z',
    updated_at: '2024-01-01T09:00:00Z',
    completed_at: '2024-01-01T09:00:00Z',
    ...overrides,
  }
}

function makeFarm(overrides: Partial<Farm> = {}): Farm {
  return {
    id: 'farm-1',
    name: 'Main Farm',
    max_concurrent_tasks: 100,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
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

function Wrapper({ children, route = '/' }: { children: ReactNode; route?: string }) {
  const [client] = useState(makeClient)
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>
        <WebSocketProvider url="ws://test">{children}</WebSocketProvider>
      </MemoryRouter>
    </QueryClientProvider>
  )
}

// The Dashboard makes multiple fetch calls on mount. This helper routes them by
// URL pattern so each query gets appropriate data.
function mockDashboardFetches({
  onlineWorkers = 0,
  offlineWorkers = 0,
  disabledWorkers = 0,
  runningJobs = 0,
  pendingJobs = 0,
  completedJobs = 0,
  failedJobs = 0,
  recentFailures = [] as Job[],
  farms = [] as Farm[],
} = {}): void {
  fetchMock.mockImplementation((input) => {
    const url = typeof input === 'string' ? input : (input as Request).url

    if (url.includes('/farms')) {
      return Promise.resolve(okJson(farms))
    }

    // Worker count queries (status=X&limit=1)
    if (url.includes('/workers') && url.includes('status=online')) {
      return Promise.resolve(okJson(makeListResponse([], onlineWorkers)))
    }
    if (url.includes('/workers') && url.includes('status=offline')) {
      return Promise.resolve(okJson(makeListResponse([], offlineWorkers)))
    }
    if (url.includes('/workers') && url.includes('status=disabled')) {
      return Promise.resolve(okJson(makeListResponse([], disabledWorkers)))
    }

    // Recent failures query must match before the generic failed count query
    if (
      url.includes('/jobs') &&
      url.includes('status=failed') &&
      url.includes('sort_by=updated_at') &&
      url.includes('sort_dir=desc') &&
      url.includes('limit=10')
    ) {
      return Promise.resolve(okJson(makeListResponse(recentFailures, failedJobs)))
    }

    // Job count queries (status=X&limit=1)
    if (url.includes('/jobs') && url.includes('status=running')) {
      return Promise.resolve(okJson(makeListResponse([], runningJobs)))
    }
    if (url.includes('/jobs') && url.includes('status=pending')) {
      return Promise.resolve(okJson(makeListResponse([], pendingJobs)))
    }
    if (url.includes('/jobs') && url.includes('status=completed')) {
      return Promise.resolve(okJson(makeListResponse([], completedJobs)))
    }
    if (url.includes('/jobs') && url.includes('status=failed')) {
      return Promise.resolve(okJson(makeListResponse([], failedJobs)))
    }

    return Promise.resolve(okJson([]))
  })
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('Dashboard', () => {
  // ── Page structure ───────────────────────────────────────────────────

  describe('page structure', () => {
    it('renders the page heading', async () => {
      mockDashboardFetches()
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByRole('heading', { name: 'dASHBOARD' }))
      expect(screen.getByRole('heading', { name: 'dASHBOARD' })).toBeInTheDocument()
    })

    it('renders the workers card', async () => {
      mockDashboardFetches()
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByTestId('workers-card'))
      expect(screen.getByTestId('workers-card')).toBeInTheDocument()
    })

    it('renders the jobs card', async () => {
      mockDashboardFetches()
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByTestId('jobs-card'))
      expect(screen.getByTestId('jobs-card')).toBeInTheDocument()
    })

    it('renders the recent failures widget', async () => {
      mockDashboardFetches()
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByTestId('recent-failures-widget'))
      expect(screen.getByTestId('recent-failures-widget')).toBeInTheDocument()
    })
  })

  // ── Workers summary card ─────────────────────────────────────────────

  describe('workers card', () => {
    it('shows the online worker count as the primary big number', async () => {
      mockDashboardFetches({ onlineWorkers: 7, offlineWorkers: 2, disabledWorkers: 1 })
      render(<Dashboard />, { wrapper: Wrapper })
      // The big number and the "Online" breakdown link both show 7; wait for the
      // stat link which carries the accessible label.
      await waitFor(() => expect(screen.getByLabelText('7 online workers')).toBeInTheDocument())
    })

    it('shows the total worker count as the denominator', async () => {
      mockDashboardFetches({ onlineWorkers: 7, offlineWorkers: 2, disabledWorkers: 1 })
      render(<Dashboard />, { wrapper: Wrapper })
      // total = 7 + 2 + 1 = 10
      await waitFor(() => expect(screen.getByText(/\/\s*10\s*total/)).toBeInTheDocument())
    })

    it('shows per-status counts in the breakdown', async () => {
      mockDashboardFetches({ onlineWorkers: 5, offlineWorkers: 3, disabledWorkers: 2 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => {
        expect(screen.getByLabelText('5 online workers')).toBeInTheDocument()
        expect(screen.getByLabelText('3 offline workers')).toBeInTheDocument()
        expect(screen.getByLabelText('2 disabled workers')).toBeInTheDocument()
      })
    })

    it('links the online count to the worker list filtered by online', async () => {
      mockDashboardFetches({ onlineWorkers: 4 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByLabelText('4 online workers'))
      const link = screen.getByLabelText('4 online workers').closest('a')
      expect(link).toHaveAttribute('href', '/workers?status=online')
    })

    it('links the offline count to the worker list filtered by offline', async () => {
      mockDashboardFetches({ offlineWorkers: 2 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByLabelText('2 offline workers'))
      const link = screen.getByLabelText('2 offline workers').closest('a')
      expect(link).toHaveAttribute('href', '/workers?status=offline')
    })

    it('links the disabled count to the worker list filtered by disabled', async () => {
      mockDashboardFetches({ disabledWorkers: 1 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByLabelText('1 disabled workers'))
      const link = screen.getByLabelText('1 disabled workers').closest('a')
      expect(link).toHaveAttribute('href', '/workers?status=disabled')
    })

    it('shows farm capacity when farms are present', async () => {
      mockDashboardFetches({ farms: [makeFarm({ max_concurrent_tasks: 200 })] })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => expect(screen.getByText(/Farm capacity: 200/)).toBeInTheDocument())
    })

    it('sums capacity across multiple farms', async () => {
      mockDashboardFetches({
        farms: [
          makeFarm({ id: 'f1', max_concurrent_tasks: 100 }),
          makeFarm({ id: 'f2', max_concurrent_tasks: 50 }),
        ],
      })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => expect(screen.getByText(/Farm capacity: 150/)).toBeInTheDocument())
    })
  })

  // ── Jobs summary card ─────────────────────────────────────────────────

  describe('jobs card', () => {
    it('shows running job count', async () => {
      mockDashboardFetches({ runningJobs: 3 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => {
        const card = screen.getByTestId('jobs-card')
        expect(card).toHaveTextContent('3')
      })
    })

    it('shows pending job count', async () => {
      mockDashboardFetches({ pendingJobs: 12 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => {
        const card = screen.getByTestId('jobs-card')
        expect(card).toHaveTextContent('12')
      })
    })

    it('shows completed job count', async () => {
      mockDashboardFetches({ completedJobs: 99 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => {
        const card = screen.getByTestId('jobs-card')
        expect(card).toHaveTextContent('99')
      })
    })

    it('shows failed job count', async () => {
      mockDashboardFetches({ failedJobs: 7 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => {
        const card = screen.getByTestId('jobs-card')
        expect(card).toHaveTextContent('7')
      })
    })

    it('links the running count to the job list filtered by running', async () => {
      mockDashboardFetches({ runningJobs: 1 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByTestId('jobs-card'))
      const links = screen
        .getByTestId('jobs-card')
        .querySelectorAll<HTMLAnchorElement>('a[href="/jobs?status=running"]')
      expect(links.length).toBeGreaterThan(0)
    })

    it('links the failed count to the job list filtered by failed', async () => {
      mockDashboardFetches({ failedJobs: 2 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByTestId('jobs-card'))
      const links = screen
        .getByTestId('jobs-card')
        .querySelectorAll<HTMLAnchorElement>('a[href="/jobs?status=failed"]')
      expect(links.length).toBeGreaterThan(0)
    })
  })

  // ── Recent failures widget ───────────────────────────────────────────

  describe('recent failures widget', () => {
    it('shows job names for recent failures', async () => {
      const job = makeJob({ name: 'Render Scene 42', status: 'failed' })
      mockDashboardFetches({ recentFailures: [job], failedJobs: 1 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('Render Scene 42'))
      expect(screen.getByText('Render Scene 42')).toBeInTheDocument()
    })

    it('shows the job owner', async () => {
      const job = makeJob({ owner: 'bob', status: 'failed' })
      mockDashboardFetches({ recentFailures: [job], failedJobs: 1 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('bob'))
      expect(screen.getByText('bob')).toBeInTheDocument()
    })

    it('shows the queue name', async () => {
      const job = makeJob({ queue_name: 'vfx-queue', status: 'failed' })
      mockDashboardFetches({ recentFailures: [job], failedJobs: 1 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('vfx-queue'))
      expect(screen.getByText('vfx-queue')).toBeInTheDocument()
    })

    it('links each failure to the job detail page', async () => {
      const job = makeJob({ id: 'job-xyz', name: 'Failed Job', status: 'failed' })
      mockDashboardFetches({ recentFailures: [job], failedJobs: 1 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByRole('link', { name: 'Failed Job' }))
      expect(screen.getByRole('link', { name: 'Failed Job' })).toHaveAttribute(
        'href',
        '/jobs/job-xyz',
      )
    })

    it('shows an empty message when there are no failures', async () => {
      mockDashboardFetches({ recentFailures: [], failedJobs: 0 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('No recent failures.'))
      expect(screen.getByText('No recent failures.')).toBeInTheDocument()
    })

    it('renders multiple recent failures', async () => {
      const jobs = [
        makeJob({ id: 'j1', name: 'Job Alpha', status: 'failed' }),
        makeJob({ id: 'j2', name: 'Job Beta', status: 'failed' }),
        makeJob({ id: 'j3', name: 'Job Gamma', status: 'failed' }),
      ]
      mockDashboardFetches({ recentFailures: jobs, failedJobs: 3 })
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('Job Alpha'))
      expect(screen.getByText('Job Beta')).toBeInTheDocument()
      expect(screen.getByText('Job Gamma')).toBeInTheDocument()
    })

    it('includes a "View all" link to the failed jobs list', async () => {
      mockDashboardFetches()
      render(<Dashboard />, { wrapper: Wrapper })
      await waitFor(() => screen.getByTestId('recent-failures-widget'))
      const viewAll = screen
        .getByTestId('recent-failures-widget')
        .querySelector<HTMLAnchorElement>('a[href="/jobs?status=failed"]')
      expect(viewAll).toBeInTheDocument()
    })

    it('shows an error message when the failures query fails', async () => {
      fetchMock.mockImplementation((input) => {
        const url = typeof input === 'string' ? input : (input as Request).url
        if (url.includes('/jobs')) return Promise.reject(new Error('Network error'))
        return Promise.resolve(
          new Response(JSON.stringify([]), {
            status: 200,
            headers: { 'Content-Type': 'application/json' },
          }),
        )
      })

      render(<Dashboard />, { wrapper: Wrapper })

      await waitFor(() => expect(screen.queryByText('No recent failures.')).not.toBeInTheDocument())
      await waitFor(() =>
        expect(screen.getByText('Failed to load recent failures.')).toBeInTheDocument(),
      )
    })
  })

  // ── WebSocket live updates ───────────────────────────────────────────

  describe('websocket updates', () => {
    it('does not show the disconnect banner when connected', async () => {
      mockDashboardFetches()
      render(<Dashboard />, { wrapper: Wrapper })

      act(() => {
        wsInstance(0).simulateOpen()
      })

      await waitFor(() => screen.getByTestId('workers-card'))
      expect(screen.queryByTestId('disconnect-banner')).not.toBeInTheDocument()
    })

    it('shows the disconnect banner when WebSocket is not connected', async () => {
      mockDashboardFetches()
      render(<Dashboard />, { wrapper: Wrapper })

      // The WebSocket starts in CONNECTING state (not 'connected') so the banner
      // should be visible immediately before the socket opens.
      await waitFor(() => screen.getByTestId('disconnect-banner'))
      expect(screen.getByTestId('disconnect-banner')).toBeInTheDocument()
    })

    it('hides the disconnect banner after WebSocket connects', async () => {
      mockDashboardFetches()
      render(<Dashboard />, { wrapper: Wrapper })

      // Banner is visible while connecting.
      await waitFor(() => screen.getByTestId('disconnect-banner'))

      act(() => {
        wsInstance(0).simulateOpen()
      })

      await waitFor(() => {
        expect(screen.queryByTestId('disconnect-banner')).not.toBeInTheDocument()
      })
    })

    it('triggers a worker query re-fetch when a worker event arrives', async () => {
      mockDashboardFetches({ onlineWorkers: 5 })
      render(<Dashboard />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('5 online workers'))

      const callsBefore = fetchMock.mock.calls.length

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'workers',
          payload: { worker_id: 'w-1', status: 'offline' },
          seq: 1,
        })
      })

      await waitFor(() => {
        expect(fetchMock.mock.calls.length).toBeGreaterThan(callsBefore)
      })
    })

    it('triggers a job query re-fetch when a job event arrives', async () => {
      mockDashboardFetches({ runningJobs: 2 })
      render(<Dashboard />, { wrapper: Wrapper })

      await waitFor(() => screen.getByTestId('jobs-card'))

      const callsBefore = fetchMock.mock.calls.length

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: { job_id: 'j-1', status: 'failed', updated_at: new Date().toISOString() },
          seq: 1,
        })
      })

      await waitFor(() => {
        expect(fetchMock.mock.calls.length).toBeGreaterThan(callsBefore)
      })
    })

    it('immediately re-fetches the recent failures widget when a failure job event arrives', async () => {
      mockDashboardFetches({ failedJobs: 1, recentFailures: [makeJob({ name: 'Failed Job' })] })
      render(<Dashboard />, { wrapper: Wrapper })

      await waitFor(() => screen.getByTestId('recent-failures-widget'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: { job_id: 'j-new', status: 'failed', updated_at: new Date().toISOString() },
          seq: 1,
        })
      })

      // The failures widget query should be immediately invalidated (no 5s wait)
      // so fetchMock should be called for the failures URL right away.
      await waitFor(() => {
        const calls = (fetchMock.mock.calls.flat() as string[]).filter(
          (u) =>
            typeof u === 'string' &&
            u.includes('status=failed') &&
            u.includes('sort_by=updated_at'),
        )
        expect(calls.length).toBeGreaterThan(0)
      })
    })

    it('triggers a full job re-fetch after the 5 s debounce window', async () => {
      mockDashboardFetches({ runningJobs: 1 })
      render(<Dashboard />, { wrapper: Wrapper })

      await waitFor(() => screen.getByTestId('jobs-card'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: { job_id: 'j-1', status: 'running', updated_at: new Date().toISOString() },
          seq: 1,
        })
      })

      const callsBefore = fetchMock.mock.calls.length

      // Advance past the 5-second debounce window.
      await act(async () => {
        vi.advanceTimersByTime(5_100)
      })

      await waitFor(() => {
        expect(fetchMock.mock.calls.length).toBeGreaterThan(callsBefore)
      })
    })
  })
})
