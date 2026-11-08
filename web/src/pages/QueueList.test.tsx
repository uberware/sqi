// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent, within, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import { WebSocketProvider } from '@/ws/context'
import { formatClock } from '@/lib/time'
import QueueList from './QueueList'
import type { Principal, Queue } from '@/api/types'

// ── Auth mock ─────────────────────────────────────────────────────────────────
// QueueList reads useAuth() to gate mutating controls behind 'infra.manage'.
// Mock the auth context directly (as JobList.test.tsx does) rather than
// driving a real AuthProvider through /auth/me, since fetchMock in this file
// is dedicated to the farm/queue list/mutation endpoints under test.

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(),
}))
import { useAuth } from '@/auth/context'
import { OPERATOR_PERMISSIONS, READ_ONLY_PERMISSIONS } from '@/test/principals'

const OPERATOR_PRINCIPAL: Principal = {
  subject: 'u-operator',
  display_name: 'Operator',
  roles: ['operator'],
  kind: 'user',
  permissions: OPERATOR_PERMISSIONS,
}
const READONLY_PRINCIPAL: Principal = {
  subject: 'u-readonly',
  display_name: 'Read Only',
  roles: ['read-only'],
  kind: 'user',
  permissions: READ_ONLY_PERMISSIONS,
}

/** Sets the principal returned by the mocked useAuth() for the next render. */
function setPrincipal(principal: Principal) {
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal,
    status: 'authed',
    refresh: () => {},
  })
}

class MockWebSocket {
  static instances: MockWebSocket[] = []
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3

  readyState = MockWebSocket.CONNECTING
  onopen: ((event: Event) => void) | null = null
  onclose: ((event: CloseEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  onmessage: ((event: MessageEvent) => void) | null = null

  url: string

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

  simulateMessage(data: unknown): void {
    this.onmessage?.(new MessageEvent('message', { data: JSON.stringify(data) }))
  }
}

function wsInstance(): MockWebSocket {
  const ws = MockWebSocket.instances[0]
  if (!ws) throw new Error('No MockWebSocket')
  return ws
}

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  MockWebSocket.instances = []
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('WebSocket', MockWebSocket)
  // Default every test to an operator principal so the gated
  // controls render; the read-only gating test overrides
  // this via setPrincipal(READONLY_PRINCIPAL).
  setPrincipal(OPERATOR_PRINCIPAL)
})
afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** A farm fixture used in several tests. */
const farm = {
  id: 'farm-1',
  name: 'render',
  max_concurrent_tasks: 0,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

/** A queue fixture used in several tests. */
const queue: Queue = {
  id: 'queue-1',
  farm_id: 'farm-1',
  name: 'lighting',
  priority: 50,
  max_concurrent_tasks: 0,
  paused: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

function mockFarmsAndQueues(queues: Queue[] = [queue]) {
  fetchMock.mockResolvedValueOnce(ok([farm]))
  fetchMock.mockResolvedValueOnce(
    ok({ items: queues, total: queues.length, limit: 1000, offset: 0 }),
  )
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <WebSocketProvider url="ws://test">
          <ToastProvider>
            <QueueList />
          </ToastProvider>
        </WebSocketProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('QueueList', () => {
  it('renders queues grouped under their farm', async () => {
    mockFarmsAndQueues()
    renderPage()
    expect(await screen.findByRole('link', { name: 'lighting' })).toBeInTheDocument()
    expect(screen.getByText('render')).toBeInTheDocument()
  })

  it('requests the queue timing figures', async () => {
    mockFarmsAndQueues()
    renderPage()
    await screen.findByRole('link', { name: 'lighting' })
    const urls = fetchMock.mock.calls.map((c) => String(c[0])).filter((u) => u.includes('/queues'))
    expect(urls).toHaveLength(1)
    expect(new URL(urls[0] ?? '', 'http://localhost').searchParams.get('include')).toBe('timing')
  })

  it('has a New Queue action linking to /queues/new', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    const link = await screen.findByRole('link', { name: /new queue/i })
    expect(link.getAttribute('href')).toBe('/queues/new')
  })

  it('shows loading state while data is fetching', () => {
    // Never resolves — keeps the query in loading state.
    fetchMock.mockReturnValue(new Promise(() => {}))
    renderPage()
    // "Loading…" appears both in the subtitle and in the table body.
    const loadingEls = screen.getAllByText('Loading…')
    expect(loadingEls.length).toBeGreaterThanOrEqual(1)
  })

  it('shows empty state when there are no queues', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    await waitFor(() => expect(screen.getByText(/no queues yet/i)).toBeInTheDocument())
  })

  it('shows an error banner when the fetch fails', async () => {
    fetchMock.mockRejectedValueOnce(new Error('Network error'))
    renderPage()
    const banner = await screen.findByRole('alert')
    expect(banner).toHaveTextContent(/failed to load queues/i)
    expect(banner).toHaveTextContent('Network error')
  })

  it('renders priority, max concurrent tasks, and paused columns correctly', async () => {
    mockFarmsAndQueues([{ ...queue, priority: 75, max_concurrent_tasks: 8, paused: true }])
    renderPage()
    await screen.findByRole('link', { name: 'lighting' })
    expect(screen.getByText('75')).toBeInTheDocument()
    expect(screen.getByText('8')).toBeInTheDocument()
    expect(screen.getByText('Yes')).toBeInTheDocument()
  })

  it('shows "Unlimited" when max_concurrent_tasks is 0', async () => {
    mockFarmsAndQueues([{ ...queue, max_concurrent_tasks: 0 }])
    renderPage()
    await screen.findByRole('link', { name: 'lighting' })
    expect(screen.getByText('Unlimited')).toBeInTheDocument()
    expect(screen.getByText('No')).toBeInTheDocument()
  })

  it('calls DELETE and shows success toast when delete is confirmed', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockFarmsAndQueues()
    renderPage()

    await screen.findByRole('button', { name: /delete queue lighting/i })
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    fireEvent.click(screen.getByRole('button', { name: /delete queue lighting/i }))

    await waitFor(() => {
      const deleteCall = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(deleteCall).toBeDefined()
    })
    await screen.findByText(/queue "lighting" deleted/i)
  })

  it('does not call DELETE when the confirm dialog is canceled', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    mockFarmsAndQueues()
    renderPage()

    await screen.findByRole('button', { name: /delete queue lighting/i })
    fireEvent.click(screen.getByRole('button', { name: /delete queue lighting/i }))

    // Give async paths a chance to run, then assert no DELETE was issued.
    await waitFor(() => expect(fetchMock).toHaveBeenCalled()) // initial GETs already ran
    const deleteCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
    )
    expect(deleteCall).toBeUndefined()
  })

  it('shows an error toast when the DELETE request fails', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    mockFarmsAndQueues()
    renderPage()

    await screen.findByRole('button', { name: /delete queue lighting/i })
    fetchMock.mockRejectedValueOnce(new Error('server unavailable'))
    fireEvent.click(screen.getByRole('button', { name: /delete queue lighting/i }))

    expect(await screen.findByText(/server unavailable/i)).toBeInTheDocument()
  })

  describe('role gating (infra.manage)', () => {
    it('hides New Queue and Delete controls for a read-only principal', async () => {
      setPrincipal(READONLY_PRINCIPAL)
      mockFarmsAndQueues()
      renderPage()

      await screen.findByRole('link', { name: 'lighting' })
      expect(screen.queryByRole('link', { name: /new queue/i })).not.toBeInTheDocument()
      expect(
        screen.queryByRole('button', { name: /delete queue lighting/i }),
      ).not.toBeInTheDocument()
    })

    it('shows New Queue and Delete controls for an operator principal', async () => {
      setPrincipal(OPERATOR_PRINCIPAL)
      mockFarmsAndQueues()
      renderPage()

      expect(await screen.findByRole('link', { name: /new queue/i })).toBeInTheDocument()
      expect(
        await screen.findByRole('button', { name: /delete queue lighting/i }),
      ).toBeInTheDocument()
    })
  })

  describe('remaining work', () => {
    const timed: Partial<Queue> = {
      unfinished_jobs: 3,
      work_remaining_seconds: 11520,
      jobs_without_estimate: 0,
      running_tasks: 1,
      as_of: '2026-03-10T10:00:00Z',
    }

    /** The cells of the row for the fixture queue, in column order. */
    async function cells(): Promise<HTMLElement[]> {
      const row = (await screen.findByRole('link', { name: 'lighting' })).closest('tr')
      if (!row) throw new Error('no row')
      return within(row).getAllByRole('cell')
    }

    function freezeClock(): number {
      vi.useFakeTimers({ shouldAdvanceTime: true })
      const now = new Date(2026, 2, 10, 10, 0, 0).getTime()
      vi.setSystemTime(now)
      return now
    }

    it('shows unfinished jobs, remaining work and ETA', async () => {
      const now = freezeClock()
      mockFarmsAndQueues([{ ...queue, ...timed }])
      renderPage()
      const c = await cells()
      expect(c[5]).toHaveTextContent('3')
      expect(c[6]).toHaveTextContent('3h 12m')
      expect(c[7]).toHaveTextContent(formatClock(now + 11_520_000, now))
    })

    it('notes jobs without an estimate', async () => {
      mockFarmsAndQueues([{ ...queue, ...timed, jobs_without_estimate: 2 }])
      renderPage()
      const c = await cells()
      expect(c[6]).toHaveTextContent('+2 without an estimate')
    })

    it('shows no ETA when nothing is known', async () => {
      mockFarmsAndQueues([
        {
          ...queue,
          ...timed,
          unfinished_jobs: 2,
          work_remaining_seconds: 0,
          jobs_without_estimate: 2,
          running_tasks: 0,
        },
      ])
      renderPage()
      const c = await cells()
      expect(c[6]).toHaveTextContent('—')
      expect(c[6]).toHaveTextContent('+2 without an estimate')
      expect(c[7]).toHaveTextContent('—')
    })

    it('shows paused instead of an ETA', async () => {
      mockFarmsAndQueues([{ ...queue, ...timed, paused: true }])
      renderPage()
      const c = await cells()
      expect(c[7]).toHaveTextContent('paused')
    })

    it('shows nothing for a queue without timing', async () => {
      mockFarmsAndQueues([queue])
      renderPage()
      const c = await cells()
      expect(c[5]).toHaveTextContent('—')
      expect(c[6]).toHaveTextContent('—')
      expect(c[7]).toHaveTextContent('—')
    })

    it('counts down while tasks run', async () => {
      freezeClock()
      mockFarmsAndQueues([{ ...queue, ...timed, work_remaining_seconds: 600, running_tasks: 2 }])
      renderPage()
      expect((await cells())[6]).toHaveTextContent('10m 0s')
      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000)
      })
      expect((await cells())[6]).toHaveTextContent('8m 0s')
    })

    describe('refetch on events', () => {
      function queueFetches(): number {
        return fetchMock.mock.calls.filter((c) => String(c[0]).includes('/queues')).length
      }

      async function setup(): Promise<number> {
        freezeClock()
        fetchMock.mockImplementation((input) =>
          Promise.resolve(
            String(input).includes('/queues')
              ? ok({ items: [{ ...queue, ...timed }], total: 1, limit: 1000, offset: 0 })
              : ok([farm]),
          ),
        )
        renderPage()
        await cells()
        act(() => wsInstance().simulateOpen())
        return queueFetches()
      }

      const push = (i: number) =>
        act(() =>
          wsInstance().simulateMessage({
            type: 'push',
            subject: 'jobs',
            payload: { job_id: 'j', task_id: 't', status: 'running', progress: i },
            seq: i + 1,
          }),
        )

      it('refetches once after a burst of events', async () => {
        const before = await setup()
        push(1)
        await act(async () => {
          await vi.advanceTimersByTimeAsync(1_000)
        })
        push(2)
        await act(async () => {
          await vi.advanceTimersByTimeAsync(4_100)
        })
        expect(queueFetches() - before).toBe(1)
        await act(async () => {
          await vi.advanceTimersByTimeAsync(10_000)
        })
        expect(queueFetches() - before).toBe(1)
      })

      it('keeps refetching while events stream', async () => {
        const before = await setup()
        for (let i = 0; i < 6; i++) {
          push(i)
          await act(async () => {
            await vi.advanceTimersByTimeAsync(2_000)
          })
        }
        expect(queueFetches() - before).toBeGreaterThanOrEqual(2)
      })
    })
  })
})
