// SPDX-License-Identifier: AGPL-3.0-only

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import JobList from './JobList'
import type { Job, ListResponse } from '@/api/types'

// ── Mocks ─────────────────────────────────────────────────────────────────────

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
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

// ── Test wrapper ──────────────────────────────────────────────────────────────

function makeClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
}

function Wrapper({ children, route = '/jobs' }: { children: ReactNode; route?: string }) {
  const client = makeClient()
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>{children}</MemoryRouter>
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

  describe('status filter (task 38)', () => {
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

  describe('search input (task 39)', () => {
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

  describe('per-row cancel (task 41)', () => {
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

  describe('bulk cancel (task 42)', () => {
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
})
