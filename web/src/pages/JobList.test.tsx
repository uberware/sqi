// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { useState } from 'react'
import type { ReactNode } from 'react'
import JobList from './JobList'
import { WebSocketProvider } from '@/ws/context'
import type { Job, ListResponse } from '@/api/types'

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
    status: 'running',
    template_format: 'yaml',
    task_counts: {
      total: 10,
      pending: 2,
      ready: 1,
      assigned: 0,
      running: 3,
      succeeded: 3,
      failed: 1,
      canceled: 0,
    },
    created_at: '2024-01-01T10:00:00Z',
    updated_at: '2024-01-01T10:05:00Z',
    started_at: '2024-01-01T10:01:00Z',
    ...overrides,
  }
}

function makeListResponse(jobs: Job[], total?: number): ListResponse<Job> {
  return { items: jobs, total: total ?? jobs.length, limit: 50, offset: 0 }
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

/** Standard wrapper — includes WebSocketProvider (using mock WS) for all tests. */
function Wrapper({ children, route = '/jobs' }: { children: ReactNode; route?: string }) {
  const [client] = useState(makeClient)
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>
        <WebSocketProvider url="ws://test">{children}</WebSocketProvider>
      </MemoryRouter>
    </QueryClientProvider>
  )
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('JobList', () => {
  describe('rendering a list of jobs', () => {
    it('renders job name in the table', async () => {
      const job = makeJob({ name: 'My Render Job' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('My Render Job'))
      expect(screen.getByText('My Render Job')).toBeInTheDocument()
    })

    it('renders a truncated job ID', async () => {
      const job = makeJob({ id: 'abcdef1234567890' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('abcdef12'))
      expect(screen.getByText('abcdef12')).toBeInTheDocument()
    })

    it('renders the status badge', async () => {
      const job = makeJob({ status: 'running' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByText('Running'))
      // At least one badge should exist (there may also be a filter pill)
      const badges = screen.getAllByLabelText('Status: Running')
      expect(badges.length).toBeGreaterThan(0)
    })

    it('renders task progress fraction', async () => {
      const job = makeJob({
        task_counts: {
          total: 10,
          pending: 0,
          ready: 0,
          assigned: 0,
          running: 3,
          succeeded: 4,
          failed: 1,
          canceled: 1,
        },
      })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      // 4 succeeded + 1 failed + 1 canceled = 6 done / 10 total
      await waitFor(() => screen.getByText('6/10'))
      expect(screen.getByText('6/10')).toBeInTheDocument()
    })

    it('shows empty state when no jobs are returned', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('No jobs found.'))
      expect(screen.getByText('No jobs found.')).toBeInTheDocument()
    })

    it('renders multiple jobs', async () => {
      const jobs = [
        makeJob({ id: 'id-1', name: 'Job One' }),
        makeJob({ id: 'id-2', name: 'Job Two' }),
      ]
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse(jobs)))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Job One'))
      expect(screen.getByText('Job Two')).toBeInTheDocument()
    })
  })

  describe('status filter', () => {
    it('renders all status filter pills', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('All'))
      expect(screen.getByText('Running')).toBeInTheDocument()
      expect(screen.getByText('Pending')).toBeInTheDocument()
      expect(screen.getByText('Paused')).toBeInTheDocument()
      expect(screen.getByText('Completed')).toBeInTheDocument()
      expect(screen.getByText('Failed')).toBeInTheDocument()
      expect(screen.getByText('Canceled')).toBeInTheDocument()
    })

    it('clicking a status filter updates the URL and re-queries', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Failed'))
      fireEvent.click(screen.getByText('Failed'))

      await waitFor(() => {
        const calls = fetchMock.mock.calls.flat() as string[]
        expect(calls.some((u) => typeof u === 'string' && u.includes('status=failed'))).toBe(true)
      })
    })

    it('All pill removes status filter', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<JobList />, {
        wrapper: ({ children }) => <Wrapper route="/jobs?status=failed">{children}</Wrapper>,
      })

      await waitFor(() => screen.getByText('All'))
      fireEvent.click(screen.getByText('All'))

      await waitFor(() => {
        const calls = fetchMock.mock.calls.flat() as string[]
        const withoutStatus = calls.filter(
          (u) => typeof u === 'string' && !(u as string).includes('status='),
        )
        expect(withoutStatus.length).toBeGreaterThan(0)
      })
    })
  })

  describe('search input', () => {
    it('renders the search input', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('searchbox'))
      expect(screen.getByRole('searchbox')).toBeInTheDocument()
    })

    it('debounces search input: input value updates immediately, fetch waits 300ms', async () => {
      const user = userEvent.setup({ advanceTimers: (ms) => vi.advanceTimersByTime(ms) })
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })
      await waitFor(() => screen.getByRole('searchbox'))

      const input = screen.getByRole('searchbox')
      await user.type(input, 'hi')

      // Input value updates immediately (uncontrolled-style via useState)
      expect(input).toHaveValue('hi')

      // Before debounce fires, the fetch should not include the search term
      const callsBeforeDebounce = fetchMock.mock.calls
        .flat()
        .filter((u) => typeof u === 'string' && (u as string).includes('search=hi'))
      expect(callsBeforeDebounce).toHaveLength(0)

      // Advance past 300ms debounce window
      act(() => vi.advanceTimersByTime(350))

      // After debounce fires, the query should eventually include search=hi
      await waitFor(
        () => {
          const calls = fetchMock.mock.calls.flat() as string[]
          expect(
            calls.some((u) => typeof u === 'string' && (u as string).includes('search=hi')),
          ).toBe(true)
        },
        { timeout: 3000 },
      )
    })
  })

  describe('per-row cancel', () => {
    it('shows Cancel button for running jobs', async () => {
      const job = makeJob({ status: 'running', name: 'Running Job' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Cancel job Running Job'))
      expect(screen.getByLabelText('Cancel job Running Job')).toBeInTheDocument()
    })

    it('does not show Cancel for terminal jobs', async () => {
      const job = makeJob({ status: 'completed', name: 'Done Job' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Done Job'))
      expect(screen.queryByLabelText('Cancel job Done Job')).not.toBeInTheDocument()
    })

    it('calls cancelJob mutation when Cancel is clicked', async () => {
      const job = makeJob({ status: 'running', name: 'Running Job' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))
      // Cancel DELETE returns 204
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
      // Invalidation re-fetch
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Cancel job Running Job'))
      fireEvent.click(screen.getByLabelText('Cancel job Running Job'))

      await waitFor(() => {
        const calls = fetchMock.mock.calls.flat() as string[]
        expect(calls.some((u) => typeof u === 'string' && u.includes(`/jobs/${job.id}`))).toBe(true)
      })
    })
  })

  describe('bulk cancel', () => {
    it('shows selection checkboxes', async () => {
      const job = makeJob({ status: 'running' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Test Job'))
      const checkboxes = screen.getAllByRole('checkbox')
      expect(checkboxes.length).toBeGreaterThan(0)
    })

    it('selecting a row shows the bulk action bar', async () => {
      const job = makeJob({ status: 'running', name: 'A Job' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Select job A Job'))
      fireEvent.click(screen.getByLabelText('Select job A Job'))

      expect(screen.getByText('1 selected')).toBeInTheDocument()
    })

    it('bulk cancel button is disabled when no cancelable rows selected', async () => {
      const job = makeJob({ status: 'completed', name: 'Done' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Done'))
      // Completed jobs cannot be selected for cancel — select-all checkbox should be disabled
      const selectAll = screen.getByLabelText('Select all cancelable jobs')
      expect(selectAll).toBeDisabled()
    })

    it('select-all checkbox selects all cancelable rows', async () => {
      const jobs = [
        makeJob({ id: 'j1', name: 'Job 1', status: 'running' }),
        makeJob({ id: 'j2', name: 'Job 2', status: 'pending' }),
      ]
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse(jobs)))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Job 1'))
      fireEvent.click(screen.getByLabelText('Select all cancelable jobs'))

      expect(screen.getByText('2 selected')).toBeInTheDocument()
    })

    it('bulk cancel calls mutation for each selected cancelable job', async () => {
      const jobs = [
        makeJob({ id: 'j1', name: 'Job 1', status: 'running' }),
        makeJob({ id: 'j2', name: 'Job 2', status: 'pending' }),
      ]
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse(jobs)))
      fetchMock.mockResolvedValue(new Response(null, { status: 204 }))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Job 1'))
      fireEvent.click(screen.getByLabelText('Select all cancelable jobs'))
      expect(screen.getByText('2 selected')).toBeInTheDocument()

      fireEvent.click(screen.getByText(/Cancel selected/))

      await waitFor(() => {
        const urls = fetchMock.mock.calls
          .flat()
          .filter((u) => typeof u === 'string' && (u as string).includes('/jobs/'))
        expect(urls.length).toBeGreaterThanOrEqual(2)
      })
    })
  })

  // ── WS job-list updates ─────────────────────────────────────────────

  describe('websocket job-list updates', () => {
    it('updates the status badge when a JobEvent arrives for a visible job', async () => {
      const job = makeJob({ id: 'job-ws-1', status: 'running', name: 'WS Job' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))
      // Allow background refetches
      fetchMock.mockResolvedValue(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Running'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: {
            job_id: 'job-ws-1',
            status: 'completed',
            updated_at: '2024-01-01T10:10:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() =>
        expect(screen.getAllByLabelText('Status: Completed').length).toBeGreaterThan(0),
      )
    })

    it('ignores JobEvents for jobs not in the current list', async () => {
      const job = makeJob({ id: 'job-visible', status: 'running', name: 'Visible Job' })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))
      fetchMock.mockResolvedValue(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('Visible Job'))

      // Event is for a different job — should not cause errors or state changes
      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: {
            job_id: 'job-not-in-list',
            status: 'completed',
            updated_at: '2024-01-01T10:10:00Z',
          },
          seq: 1,
        })
      })

      // Visible job's badge should remain Running
      expect(screen.getAllByLabelText('Status: Running').length).toBeGreaterThan(0)
    })

    it('updates task progress when a TaskEvent with a known previous status arrives', async () => {
      const job = makeJob({
        id: 'job-progress',
        status: 'running',
        task_counts: {
          total: 5,
          pending: 0,
          ready: 0,
          assigned: 0,
          running: 2,
          succeeded: 3,
          failed: 0,
          canceled: 0,
        },
      })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))
      fetchMock.mockResolvedValue(okJson(makeListResponse([job])))

      render(<JobList />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('3/5'))

      act(() => {
        wsInstance(0).simulateOpen()
        // First event: seed the task status (running)
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: {
            job_id: 'job-progress',
            task_id: 'task-1',
            status: 'running',
            updated_at: '2024-01-01T10:05:00Z',
          },
          seq: 1,
        })
        // Second event: task transitions running → succeeded
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: {
            job_id: 'job-progress',
            task_id: 'task-1',
            status: 'succeeded',
            updated_at: '2024-01-01T10:06:00Z',
          },
          seq: 2,
        })
      })

      // After the delta: succeeded goes from 3→4, running goes from 2→1, done = 4/5
      await waitFor(() => expect(screen.getByText('4/5')).toBeInTheDocument())
    })
  })

  // ── Last-updated timestamp ──────────────────────────────────────────

  describe('last-updated timestamp', () => {
    it('shows a last-updated label after data loads', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText(/Updated/))
      expect(screen.getByText(/Updated/)).toBeInTheDocument()
    })
  })

  // ── Manual refresh ───────────────────────────────────────────────────

  describe('manual refresh button', () => {
    it('renders a Refresh button', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('button', { name: 'Refresh jobs' }))
      expect(screen.getByRole('button', { name: 'Refresh jobs' })).toBeInTheDocument()
    })

    it('clicking Refresh triggers a re-fetch of the jobs list', async () => {
      fetchMock.mockResolvedValue(okJson(makeListResponse([])))

      render(<JobList />, { wrapper: Wrapper })
      await waitFor(() => screen.getByRole('button', { name: 'Refresh jobs' }))

      const callsBefore = fetchMock.mock.calls.length
      fireEvent.click(screen.getByRole('button', { name: 'Refresh jobs' }))

      await waitFor(() => {
        expect(fetchMock.mock.calls.length).toBeGreaterThan(callsBefore)
      })
    })
  })

  // ── Per-row and bulk delete ───────────────────────────────────────────

  describe('per-row and bulk delete', () => {
    /**
     * Render the job list seeded with a single completed job (id "job-1",
     * name "job-1") and wait for the name link to appear.  Using status
     * "completed" avoids rendering a Cancel button so /^cancel$/i queries are
     * unambiguous.  We wait for `getByRole('link', { name: 'job-1' })` rather
     * than `getByText('job-1')` because RTL's getByText matches the IdCell
     * span's direct text node too (it only checks direct child text nodes, not
     * the concatenated textContent of the whole subtree).
     */
    async function renderJobList(overrides: Partial<Job> = {}) {
      const job = makeJob({ id: 'job-1', name: 'job-1', status: 'completed', ...overrides })
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([job])))
      render(<JobList />, { wrapper: Wrapper })
      await waitFor(() => screen.getByRole('link', { name: 'job-1' }))
    }

    /** Fire a WS push event on the given subject via the first MockWebSocket. */
    function emitWsEvent(subject: string, payload: unknown) {
      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({ type: 'push', subject, payload, seq: 1 })
      })
    }

    it('deletes a job after confirmation', async () => {
      await renderJobList()
      // Mock the DELETE response and the subsequent invalidation re-fetch.
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([])))

      fireEvent.click(screen.getByRole('button', { name: /delete job job-1/i }))
      expect(screen.getByRole('dialog', { name: /confirm delete/i })).toBeInTheDocument()

      fireEvent.click(screen.getByRole('button', { name: /^delete$/i }))

      await waitFor(() => {
        expect(fetchMock).toHaveBeenCalledWith(
          expect.stringContaining('/jobs/job-1'),
          expect.objectContaining({ method: 'DELETE' }),
        )
      })
    })

    it('cancel in the confirm dialog aborts the delete', async () => {
      await renderJobList()

      fireEvent.click(screen.getByRole('button', { name: /delete job job-1/i }))
      expect(screen.getByRole('dialog', { name: /confirm delete/i })).toBeInTheDocument()

      fireEvent.click(screen.getByRole('button', { name: /^cancel$/i }))
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()

      expect(fetchMock).not.toHaveBeenCalledWith(
        expect.stringContaining('/jobs/job-1'),
        expect.objectContaining({ method: 'DELETE' }),
      )
    })

    it('drops a row on a job removed WS event', async () => {
      await renderJobList()
      // Use the name link (role="link") to be unambiguous — the IdCell also has
      // a direct text node 'job-1' which confuses queryByText.
      expect(screen.getByRole('link', { name: 'job-1' })).toBeInTheDocument()

      emitWsEvent('jobs', { job_id: 'job-1', status: 'removed', updated_at: new Date().toISOString() })

      await waitFor(() => expect(screen.queryByRole('link', { name: 'job-1' })).not.toBeInTheDocument())
    })

    it('bulk delete selected confirms and deletes the job', async () => {
      await renderJobList()
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
      fetchMock.mockResolvedValueOnce(okJson(makeListResponse([])))

      fireEvent.click(screen.getByLabelText('Select job job-1'))
      expect(screen.getByText('1 selected')).toBeInTheDocument()

      fireEvent.click(screen.getByRole('button', { name: /delete selected/i }))
      expect(screen.getByRole('dialog', { name: /confirm delete/i })).toBeInTheDocument()

      fireEvent.click(screen.getByRole('button', { name: /^delete$/i }))

      await waitFor(() => {
        expect(fetchMock).toHaveBeenCalledWith(
          expect.stringContaining('/jobs/job-1'),
          expect.objectContaining({ method: 'DELETE' }),
        )
      })
    })
  })
})
