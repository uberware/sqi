// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor, act } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { useState } from 'react'
import type { ReactNode } from 'react'
import JobDetail from './JobDetail'
import { WebSocketProvider } from '@/ws/context'
import type { JobDetail as JobDetailType, ListResponse, Task, Worker } from '@/api/types'

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
})
afterEach(() => {
  vi.restoreAllMocks()
})

// ── Fixtures ──────────────────────────────────────────────────────────────────

function makeJob(overrides: Partial<JobDetailType> = {}): JobDetailType {
  return {
    id: 'job-aabbccdd-eeff',
    farm_id: 'farm-1',
    queue_id: 'queue-1',
    queue_name: 'default',
    name: 'Test Render Job',
    owner: 'alice',
    submitter: 'bob',
    priority: 50,
    status: 'running',
    template_format: 'yaml',
    failed_attempts: 0,
    task_counts: {
      total: 3,
      pending: 0,
      ready: 0,
      assigned: 0,
      running: 1,
      succeeded: 1,
      failed: 1,
      canceled: 0,
    },
    created_at: '2024-01-15T10:00:00Z',
    updated_at: '2024-01-15T10:05:00Z',
    started_at: '2024-01-15T10:01:00Z',
    steps: [
      {
        id: 'step-1',
        name: 'RenderFrames',
        step_order: 0,
        status: 'running',
        created_at: '2024-01-15T10:00:00Z',
        updated_at: '2024-01-15T10:01:00Z',
      },
      {
        id: 'step-2',
        name: 'ComposeVideo',
        step_order: 1,
        status: 'pending',
        depends_on: ['RenderFrames'],
        created_at: '2024-01-15T10:00:00Z',
        updated_at: '2024-01-15T10:00:00Z',
      },
    ],
    ...overrides,
  }
}

function makeTask(overrides: Partial<Task> = {}): Task {
  return {
    id: 'task-11223344-5566',
    job_id: 'job-aabbccdd-eeff',
    step_id: 'step-1',
    name: 'task.0',
    status: 'running',
    created_at: '2024-01-15T10:01:00Z',
    updated_at: '2024-01-15T10:03:00Z',
    ...overrides,
  }
}

function makeTaskListResponse(tasks: Task[]): ListResponse<Task> {
  return { items: tasks, total: tasks.length, limit: 1000, offset: 0 }
}

function makeWorker(overrides: Partial<Worker> = {}): Worker {
  return {
    id: 'worker-abcdef12',
    farm_id: 'farm-1',
    hostname: 'render-node-42',
    gpu: {},
    status: 'online',
    removable: false,
    registered_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

function makeWorkerListResponse(workers: Worker[]): ListResponse<Worker> {
  return { items: workers, total: workers.length, limit: 1000, offset: 0 }
}

function okJson(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function problemJson(status: number, detail: string): Response {
  return new Response(
    JSON.stringify({
      type: 'about:blank',
      title: 'Error',
      status,
      detail,
    }),
    { status, headers: { 'Content-Type': 'application/problem+json' } },
  )
}

// ── Test wrappers ─────────────────────────────────────────────────────────────

function makeClient() {
  return new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
}

function Wrapper({
  children,
  jobId = 'job-aabbccdd-eeff',
}: {
  children: ReactNode
  jobId?: string
}) {
  const [client] = useState(makeClient)
  return (
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/jobs/${jobId}`]}>
        <WebSocketProvider url="ws://test">
          <Routes>
            <Route path="/jobs/:id" element={children} />
            <Route path="/jobs/:id/tasks/:taskId/logs" element={<div>Log viewer</div>} />
            <Route path="/workers/:id" element={<div>Worker detail</div>} />
          </Routes>
        </WebSocketProvider>
      </MemoryRouter>
    </QueryClientProvider>
  )
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('JobDetail', () => {
  describe('loading and error states', () => {
    it('renders loading state while job is being fetched', () => {
      // Never resolves — stays in loading state
      fetchMock.mockReturnValue(new Promise(() => undefined))

      render(<JobDetail />, { wrapper: Wrapper })

      expect(screen.getByText('Loading…')).toBeInTheDocument()
    })

    it('renders error banner when job fetch fails', async () => {
      fetchMock.mockResolvedValueOnce(problemJson(404, 'job not found'))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('alert'))
      expect(screen.getByRole('alert')).toHaveTextContent('Failed to load job')
    })
  })

  describe('job metadata', () => {
    it('renders the job name in the page header', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ name: 'My Render Job' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('My Render Job'))
      expect(screen.getByText('My Render Job')).toBeInTheDocument()
    })

    it('renders owner and submitter separately', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ owner: 'alice', submitter: 'bob' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('alice'))
      expect(screen.getByText('alice')).toBeInTheDocument()
      expect(screen.getByText('bob')).toBeInTheDocument()
    })

    it('renders the status badge in the header action slot', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ status: 'failed' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByLabelText('Status: Failed'))
      const badges = screen.getAllByLabelText('Status: Failed')
      expect(badges.length).toBeGreaterThan(0)
    })

    it('renders queue name', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ queue_name: 'vfx-queue' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('vfx-queue'))
      expect(screen.getByText('vfx-queue')).toBeInTheDocument()
    })

    it('renders project tag when present', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ project: 'dragon-film' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('dragon-film'))
      expect(screen.getByText('dragon-film')).toBeInTheDocument()
    })

    it('renders a truncated job ID in the metadata', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ id: 'abcdef1234567890' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByText('abcdef12'))
      expect(screen.getAllByText('abcdef12').length).toBeGreaterThan(0)
    })

    it('renders a task progress bar in the metadata', async () => {
      // Fixture: total 3, succeeded 1 + failed 1 + canceled 0 = 2 done.
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await screen.findByRole('progressbar', { name: /task progress/i })
      expect(screen.getByText('2/3')).toBeInTheDocument()
    })
  })

  describe('step breakdown', () => {
    it('renders each step section with its name', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('RenderFrames'))
      expect(screen.getByText('RenderFrames')).toBeInTheDocument()
      expect(screen.getByText('ComposeVideo')).toBeInTheDocument()
    })

    it('renders the depends_on annotation for dependent steps', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText(/Depends on:/))
      expect(screen.getByText(/Depends on:/)).toHaveTextContent('RenderFrames')
    })

    it('renders step status badge', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      // Multiple Running badges exist (job header, metadata, step) — any is sufficient
      await waitFor(() => screen.getAllByLabelText('Status: Running'))
      expect(screen.getAllByLabelText('Status: Running').length).toBeGreaterThan(0)
    })

    it('renders steps in step_order (ascending)', async () => {
      const job = makeJob({
        steps: [
          {
            id: 'step-2',
            name: 'SecondStep',
            step_order: 1,
            status: 'pending',
            created_at: '',
            updated_at: '',
          },
          {
            id: 'step-1',
            name: 'FirstStep',
            step_order: 0,
            status: 'running',
            created_at: '',
            updated_at: '',
          },
        ],
      })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('FirstStep'))

      const sections = screen.getAllByRole('region')
      const firstIdx = sections.findIndex((s) => s.getAttribute('aria-label') === 'Step: FirstStep')
      const secondIdx = sections.findIndex(
        (s) => s.getAttribute('aria-label') === 'Step: SecondStep',
      )
      expect(firstIdx).toBeLessThan(secondIdx)
    })
  })

  describe('cross-job dependencies', () => {
    it('renders a "Waiting on" section linking each upstream job when depends_on is non-empty', async () => {
      const job = makeJob({ depends_on: ['job-upstream-1', 'job-upstream-2'] })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Waiting on'))
      const upstreamLink1 = screen.getByRole('link', { name: 'job-upstream-1' })
      const upstreamLink2 = screen.getByRole('link', { name: 'job-upstream-2' })
      expect(upstreamLink1).toHaveAttribute('href', '/jobs/job-upstream-1')
      expect(upstreamLink2).toHaveAttribute('href', '/jobs/job-upstream-2')
    })

    it('omits the "Waiting on" section when depends_on is absent', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('RenderFrames'))
      expect(screen.queryByText('Waiting on')).not.toBeInTheDocument()
    })
  })

  describe('task table', () => {
    it('renders tasks within their parent step section', async () => {
      const task = makeTask({ id: 'task-99887766', step_id: 'step-1', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('task-998'))
      expect(screen.getByText('task-998')).toBeInTheDocument()
    })

    it('renders parameters collapsed by default showing key=value preview', async () => {
      const task = makeTask({
        parameters: { frame: '42', layer: 'beauty' },
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText(/frame=42/))
      expect(screen.getByText(/frame=42/)).toBeInTheDocument()
    })

    it('expands parameters on click', async () => {
      const task = makeTask({
        parameters: { frame: '42', layer: 'beauty', camera: 'main' },
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByTitle('Click to expand parameters'))
      fireEvent.click(screen.getByTitle('Click to expand parameters'))

      // After expand all three keys should be visible
      await waitFor(() => screen.getByText('frame'))
      expect(screen.getByText('frame')).toBeInTheDocument()
      expect(screen.getByText('layer')).toBeInTheDocument()
      expect(screen.getByText('camera')).toBeInTheDocument()
    })

    it('renders the worker name (not hostname) with link for assigned tasks', async () => {
      const task = makeTask({ assigned_worker_id: 'worker-abcdef12' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValueOnce(
        okJson(
          makeWorkerListResponse([
            makeWorker({
              id: 'worker-abcdef12',
              name: 'gpu-box-01',
              hostname: 'render-node-42',
            }),
          ]),
        ),
      )

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('gpu-box-01'))
      // The human-readable name is shown in preference to the hostname.
      expect(screen.queryByText('render-node-42')).not.toBeInTheDocument()
      const link = screen.getByText('gpu-box-01').closest('a')
      expect(link).toHaveAttribute('href', '/workers/worker-abcdef12')
    })

    it('falls back to hostname when the worker has no name', async () => {
      const task = makeTask({ assigned_worker_id: 'worker-abcdef12' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValueOnce(
        okJson(
          makeWorkerListResponse([
            makeWorker({ id: 'worker-abcdef12', hostname: 'render-node-42' }),
          ]),
        ),
      )

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('render-node-42'))
      const link = screen.getByText('render-node-42').closest('a')
      expect(link).toHaveAttribute('href', '/workers/worker-abcdef12')
    })

    it('falls back to the truncated worker ID when the name is unknown', async () => {
      const task = makeTask({ assigned_worker_id: 'worker-abcdef12' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValueOnce(okJson(makeWorkerListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('worker-a'))
      const link = screen.getByText('worker-a').closest('a')
      expect(link).toHaveAttribute('href', '/workers/worker-abcdef12')
    })

    it('shows dash for unassigned worker', async () => {
      const task = makeTask({ status: 'pending' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('task-112'))
      // Check that the worker column shows a dash (muted "—")
      const taskTable = screen.getByRole('table', { name: /Tasks for step RenderFrames/ })
      const rows = taskTable.querySelectorAll('tbody tr')
      expect(rows[0]).toBeInTheDocument()
    })
  })

  describe('attempt timeline', () => {
    it('expands a task row to show its attempt history with reasons', async () => {
      const task = makeTask({
        id: 't1',
        name: 'render[Frame=2]',
        status: 'ready',
        failed_attempts: 1,
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockImplementation((url) => {
        const u = String(url)
        if (u.includes('/attempts')) {
          return Promise.resolve(
            okJson({
              items: [
                {
                  attempt_number: 1,
                  status: 'failed',
                  worker_id: 'w1',
                  exit_code: 1,
                  message: 'worker not configured for staging',
                  started_at: '2026-07-11T19:00:00Z',
                  ended_at: '2026-07-11T19:00:01Z',
                },
                { attempt_number: 2, status: 'running', started_at: '2026-07-11T19:00:30Z' },
              ],
            }),
          )
        }
        if (u.includes('/workers')) return Promise.resolve(okJson(makeWorkerListResponse([])))
        return Promise.resolve(okJson(makeTaskListResponse([task])))
      })

      render(<JobDetail />, { wrapper: Wrapper })

      const toggle = await screen.findByRole('button', {
        name: /attempts for render\[Frame=2\]/i,
      })
      expect(toggle).toHaveAttribute('aria-expanded', 'false')
      fireEvent.click(toggle)

      expect(await screen.findByText(/worker not configured for staging/i)).toBeInTheDocument()
      // Exact, case-sensitive: the row's existing lowercase "attempt 2" retry
      // indicator (from failed_attempts) must not satisfy this assertion —
      // only the attempt-history entries ("Attempt 1"/"Attempt 2") should.
      expect(screen.getByText('Attempt 1')).toBeInTheDocument()
      expect(screen.getByText('Attempt 2')).toBeInTheDocument()
      expect(screen.getByText(/exit 1/i)).toBeInTheDocument()

      // The button flips to the "Hide" accessible name and aria-expanded once open.
      expect(
        await screen.findByRole('button', { name: /hide attempts for render\[Frame=2\]/i }),
      ).toHaveAttribute('aria-expanded', 'true')
    })

    it('shows an empty state when a task has no recorded attempts', async () => {
      const task = makeTask({ id: 't2', name: 'render[Frame=3]', status: 'pending' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockImplementation((url) => {
        const u = String(url)
        if (u.includes('/attempts')) return Promise.resolve(okJson({ items: [] }))
        if (u.includes('/workers')) return Promise.resolve(okJson(makeWorkerListResponse([])))
        return Promise.resolve(okJson(makeTaskListResponse([task])))
      })

      render(<JobDetail />, { wrapper: Wrapper })

      fireEvent.click(
        await screen.findByRole('button', { name: /attempts for render\[Frame=3\]/i }),
      )

      expect(await screen.findByText(/no attempts/i)).toBeInTheDocument()
    })

    it('refetches an expanded task row attempts when a WS task event arrives for it', async () => {
      const task = makeTask({ id: 't3', name: 'render[Frame=4]', status: 'running' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockImplementation((url) => {
        const u = String(url)
        if (u.includes('/attempts')) return Promise.resolve(okJson({ items: [] }))
        if (u.includes('/workers')) return Promise.resolve(okJson(makeWorkerListResponse([])))
        return Promise.resolve(okJson(makeTaskListResponse([task])))
      })

      render(<JobDetail />, { wrapper: Wrapper })

      fireEvent.click(
        await screen.findByRole('button', { name: /attempts for render\[Frame=4\]/i }),
      )

      const attemptCalls = () =>
        fetchMock.mock.calls.filter(([url]) => String(url).includes('/attempts')).length

      await waitFor(() => expect(attemptCalls()).toBe(1))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs/job-aabbccdd-eeff/tasks',
          payload: {
            job_id: 'job-aabbccdd-eeff',
            task_id: 't3',
            status: 'failed',
            updated_at: '2024-01-15T10:10:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() => expect(attemptCalls()).toBeGreaterThan(1))
    })
  })

  describe('retry button', () => {
    it('shows Retry button for failed tasks', async () => {
      const task = makeTask({ status: 'failed', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Retry task task.0'))
      expect(screen.getByLabelText('Retry task task.0')).toBeInTheDocument()
    })

    it('shows Retry button for canceled tasks', async () => {
      const task = makeTask({ status: 'canceled', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Retry task task.0'))
      expect(screen.getByLabelText('Retry task task.0')).toBeInTheDocument()
    })

    it('does not show Retry button for succeeded tasks', async () => {
      const task = makeTask({ status: 'succeeded', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByRole('link', { name: /view logs/i }))
      expect(screen.queryByLabelText('Retry task task.0')).not.toBeInTheDocument()
    })

    it('does not show Retry button for running tasks', async () => {
      const task = makeTask({ status: 'running', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByRole('link', { name: /view logs/i }))
      expect(screen.queryByLabelText('Retry task task.0')).not.toBeInTheDocument()
    })

    it('calls the retryTask mutation when Retry is clicked', async () => {
      const task = makeTask({ id: 'task-retryable', status: 'failed', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      // retryTask POST response
      fetchMock.mockResolvedValueOnce(okJson({ task_id: 'task-retryable', status: 'ready' }))
      // invalidation refetches
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Retry task task.0'))
      fireEvent.click(screen.getByLabelText('Retry task task.0'))

      await waitFor(() => {
        const urls = fetchMock.mock.calls.flat() as string[]
        expect(urls.some((u) => typeof u === 'string' && u.includes('task-retryable'))).toBe(true)
      })
    })

    it('shows pending status optimistically while retry is in flight', async () => {
      const task = makeTask({ status: 'failed', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValueOnce(okJson(makeWorkerListResponse([])))

      // Slow retry — never resolves during this test
      let resolveRetry!: (v: Response) => void
      fetchMock.mockImplementationOnce(
        () =>
          new Promise<Response>((resolve) => {
            resolveRetry = resolve
          }),
      )
      // Catchall for any background refetches: keep the task visible
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Retry task task.0'))
      fireEvent.click(screen.getByLabelText('Retry task task.0'))

      // While in-flight, the task row's status badge shows pending (optimistic update).
      // There may also be a step with pending status, so use getAllByLabelText.
      await waitFor(() => screen.getAllByLabelText('Status: Pending'))
      expect(screen.getAllByLabelText('Status: Pending').length).toBeGreaterThan(0)

      // Clean up by resolving the pending request
      resolveRetry(okJson({ task_id: task.id, status: 'ready' }))
    })

    it('does NOT show retry button for a canceled task whose upstream step has not completed', async () => {
      // step-2 (ComposeVideo) depends_on step-1 (RenderFrames), and step-1 is
      // 'failed' (not 'completed'), so depsSatisfied = false for step-2.
      const job = makeJob({
        steps: [
          {
            id: 'step-1',
            name: 'RenderFrames',
            step_order: 0,
            status: 'failed',
            created_at: '2024-01-15T10:00:00Z',
            updated_at: '2024-01-15T10:05:00Z',
          },
          {
            id: 'step-2',
            name: 'ComposeVideo',
            step_order: 1,
            status: 'canceled',
            depends_on: ['RenderFrames'],
            created_at: '2024-01-15T10:00:00Z',
            updated_at: '2024-01-15T10:05:00Z',
          },
        ],
      })
      const cascadeCanceled = makeTask({
        id: 'task-cascade',
        step_id: 'step-2',
        status: 'canceled',
        name: 'task.cascade',
      })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([cascadeCanceled])))

      render(<JobDetail />, { wrapper: Wrapper })

      // Wait for table to render (logs link is always present)
      await waitFor(() => screen.getAllByRole('link', { name: /view logs/i }))
      // The cascade-canceled dep-blocked task must NOT have a retry button.
      expect(screen.queryByLabelText('Retry task task.cascade')).not.toBeInTheDocument()
      // And it must not have a checkbox (not selectable).
      expect(
        screen.queryByRole('checkbox', { name: 'Select task task.cascade' }),
      ).not.toBeInTheDocument()
    })

    it('does NOT include dep-blocked canceled tasks in bulk selectedRetryable', async () => {
      // step-1 is failed → step-2's dep (RenderFrames) is not completed.
      const job = makeJob({
        steps: [
          {
            id: 'step-1',
            name: 'RenderFrames',
            step_order: 0,
            status: 'failed',
            created_at: '',
            updated_at: '',
          },
          {
            id: 'step-2',
            name: 'ComposeVideo',
            step_order: 1,
            status: 'canceled',
            depends_on: ['RenderFrames'],
            created_at: '',
            updated_at: '',
          },
        ],
      })
      // A cancelable running task in step-1 (selectable via CANCELABLE path).
      const runningTask = makeTask({
        id: 'task-running-dep',
        step_id: 'step-1',
        status: 'running',
        name: 'task.run',
      })
      // A cascade-canceled task in step-2 — not selectable because deps unsatisfied.
      const blockedTask = makeTask({
        id: 'task-blocked-dep',
        step_id: 'step-2',
        status: 'canceled',
        name: 'task.blocked',
      })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([runningTask, blockedTask])))

      render(<JobDetail />, { wrapper: Wrapper })

      // The running task is cancelable → checkbox present.
      await screen.findByRole('checkbox', { name: 'Select task task.run' })
      // The blocked task is NOT selectable → no checkbox.
      expect(
        screen.queryByRole('checkbox', { name: 'Select task task.blocked' }),
      ).not.toBeInTheDocument()
    })

    it('shows retry button for a failed task in a step whose deps ARE satisfied', async () => {
      // step-1 completed → step-2 deps ARE satisfied → retry button visible for failed task.
      const job = makeJob({
        steps: [
          {
            id: 'step-1',
            name: 'RenderFrames',
            step_order: 0,
            status: 'completed',
            created_at: '',
            updated_at: '',
          },
          {
            id: 'step-2',
            name: 'ComposeVideo',
            step_order: 1,
            status: 'failed',
            depends_on: ['RenderFrames'],
            created_at: '',
            updated_at: '',
          },
        ],
      })
      const failedTask = makeTask({
        id: 'task-deps-ok',
        step_id: 'step-2',
        status: 'failed',
        name: 'task.depsfail',
      })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([failedTask])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Retry task task.depsfail'))
      expect(screen.getByLabelText('Retry task task.depsfail')).toBeInTheDocument()
    })

    it('shows inline error and reverts status when retry fails', async () => {
      const task = makeTask({ status: 'failed', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValueOnce(okJson(makeWorkerListResponse([])))
      // Retry returns 409 Conflict
      fetchMock.mockResolvedValueOnce(problemJson(409, 'task cannot be retried'))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('Retry task task.0'))
      fireEvent.click(screen.getByLabelText('Retry task task.0'))

      await waitFor(() => screen.getByText(/Retry failed/))
      expect(screen.getByText(/task cannot be retried/)).toBeInTheDocument()

      // After failure, the original failed status should be restored
      await waitFor(() => screen.getByLabelText('Status: Failed'))
      expect(screen.getByLabelText('Status: Failed')).toBeInTheDocument()
    })
  })

  describe('view logs link', () => {
    it('renders a Logs link on every task row', async () => {
      const tasks = [
        makeTask({ id: 'task-a1', status: 'running', name: 'task.0' }),
        makeTask({ id: 'task-b2', status: 'succeeded', name: 'task.1' }),
      ]
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse(tasks)))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByRole('link', { name: /view logs/i }))
      const logsLinks = screen.getAllByRole('link', { name: /view logs/i })
      expect(logsLinks).toHaveLength(2)
    })

    it('Logs link points to the correct route for each task', async () => {
      const task = makeTask({ id: 'task-loglink99', name: 'task.0', status: 'succeeded' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ id: 'job-aabbccdd-eeff' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByLabelText('View logs for task task.0'))
      const logsLink = screen.getByLabelText('View logs for task task.0')
      expect(logsLink).toHaveAttribute('href', '/jobs/job-aabbccdd-eeff/tasks/task-loglink99/logs')
    })
  })

  describe('edge cases', () => {
    it('renders "No tasks for this step" when a step has no tasks', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByText('No tasks for this step.'))
      // Both steps should show the empty message since no tasks were returned
      expect(screen.getAllByText('No tasks for this step.').length).toBeGreaterThan(0)
    })

    it('renders "No steps found" when the job has no steps', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ steps: [] })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('No steps found.'))
      expect(screen.getByText('No steps found.')).toBeInTheDocument()
    })

    it('renders muted dash for task with no parameters', async () => {
      const task = makeTask()
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('task-112'))
      // The Parameters column should show a dash for tasks with no params
      const taskTable = screen.getByRole('table', { name: /Tasks for step RenderFrames/ })
      expect(taskTable).toBeInTheDocument()
    })

    it('renders step aggregate counts correctly', async () => {
      const tasks = [
        makeTask({ id: 't1', status: 'running' }),
        makeTask({ id: 't2', status: 'succeeded' }),
        makeTask({ id: 't3', status: 'failed' }),
      ]
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse(tasks)))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('table', { name: /Tasks for step RenderFrames/ }))
      const stepSection = screen.getByRole('region', { name: 'Step: RenderFrames' })
      // 3 total, 1 running, 1 succeeded, 1 failed
      expect(stepSection).toHaveTextContent('3')
      expect(stepSection).toHaveTextContent('1')
    })
  })

  // ── WS task-level updates ───────────────────────────────────────────

  describe('websocket task-level updates', () => {
    it('updates task status badge in place when a TaskEvent arrives', async () => {
      const task = makeTask({
        id: 'task-ws-update',
        status: 'running',
        name: 'task.0',
        job_id: 'job-aabbccdd-eeff',
        step_id: 'step-1',
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      // Background refetch after debounced invalidation
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Running'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs/job-aabbccdd-eeff/tasks',
          payload: {
            job_id: 'job-aabbccdd-eeff',
            task_id: 'task-ws-update',
            status: 'succeeded',
            updated_at: '2024-01-15T10:10:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() =>
        expect(screen.getAllByLabelText('Status: Succeeded').length).toBeGreaterThan(0),
      )
    })

    it('updates the job header status badge when a JobEvent arrives', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ status: 'running' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Running'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: {
            job_id: 'job-aabbccdd-eeff',
            status: 'completed',
            updated_at: '2024-01-15T10:20:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() =>
        expect(screen.getAllByLabelText('Status: Completed').length).toBeGreaterThan(0),
      )
    })

    it('patches assigned_worker_id when a TaskEvent includes worker_id', async () => {
      // No assigned_worker_id on initial load — the WS event should set it.
      const task = makeTask({
        id: 'task-ws-worker',
        status: 'running',
        name: 'task.0',
        job_id: 'job-aabbccdd-eeff',
        step_id: 'step-1',
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Running'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs/job-aabbccdd-eeff/tasks',
          payload: {
            job_id: 'job-aabbccdd-eeff',
            task_id: 'task-ws-worker',
            status: 'running',
            worker_id: 'worker-newnode99',
            updated_at: '2024-01-15T10:10:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() => expect(screen.getByText('worker-n')).toBeInTheDocument())
    })

    it('silently ignores a TaskEvent for an unknown task_id (idx === -1)', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getByText('Test Render Job'))

      // Sending an event for a task not in the cache must not throw or corrupt state.
      expect(() => {
        act(() => {
          wsInstance(0).simulateOpen()
          wsInstance(0).simulateMessage({
            type: 'push',
            subject: 'jobs/job-aabbccdd-eeff/tasks',
            payload: {
              job_id: 'job-aabbccdd-eeff',
              task_id: 'task-nonexistent',
              status: 'succeeded',
              updated_at: '2024-01-15T10:10:00Z',
            },
            seq: 1,
          })
        })
      }).not.toThrow()

      // DOM should be unchanged (no spurious rows or crash)
      expect(screen.queryByText('task-none')).not.toBeInTheDocument()
    })

    it('ignores JobEvents for other jobs on the jobs subject', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob({ status: 'running' })))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Running'))

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs',
          payload: {
            job_id: 'some-other-job',
            status: 'completed',
            updated_at: '2024-01-15T10:20:00Z',
          },
          seq: 1,
        })
      })

      // The current job's status should remain Running
      expect(screen.getAllByLabelText('Status: Running').length).toBeGreaterThan(0)
    })
  })

  // ── Last-updated timestamp ──────────────────────────────────────────

  describe('last-updated timestamp', () => {
    it('shows a last-updated label after data loads', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText(/Updated/))
      expect(screen.getByText(/Updated/)).toBeInTheDocument()
    })
  })

  // ── Task cancel ──────────────────────────────────────────────────────

  describe('task cancel', () => {
    it('shows a Cancel button for a running task and calls the cancel endpoint', async () => {
      // Arrange: a running task is present in the mocked tasks response.
      const task = makeTask({ id: 'task-cancelable', status: 'running', name: 'task.running' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValueOnce(okJson({ task_id: task.id, status: 'canceled' }))
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      const cancelBtn = await screen.findByRole('button', { name: /Cancel task/i })
      expect(cancelBtn).toBeInTheDocument()

      fireEvent.click(cancelBtn)

      await waitFor(() => {
        expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/cancel'))).toBe(true)
      })
    })

    it('does not show a Cancel button for a succeeded task', async () => {
      const task = makeTask({ status: 'succeeded', name: 'task.done' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByRole('link', { name: /view logs/i }))
      // The succeeded-task row exposes Retry/Logs but not Cancel.
      expect(screen.queryByRole('button', { name: /Cancel task/i })).not.toBeInTheDocument()
    })
  })

  // ── Task bulk operations ─────────────────────────────────────────────────

  describe('task bulk operations', () => {
    it('selecting a running task enables Cancel selected and triggers cancel', async () => {
      const task = makeTask({ status: 'running', name: 'task.0' })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValueOnce(okJson(makeWorkerListResponse([])))
      fetchMock.mockResolvedValueOnce(okJson({ task_id: task.id, status: 'canceled' }))
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      const checkbox = await screen.findByRole('checkbox', { name: /Select task/i })
      fireEvent.click(checkbox)

      const bulkCancel = screen.getByRole('button', { name: /Cancel selected/i })
      expect(bulkCancel).toBeEnabled()
      fireEvent.click(bulkCancel)

      await waitFor(() => {
        expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/cancel'))).toBe(true)
      })
    })

    it('bulk-cancel removes acted tasks from selection but keeps non-acted tasks selected', async () => {
      // Select a cancelable (running) task and a retryable (failed) task.
      // After bulk-cancel, only the cancelable task should be deselected;
      // the failed task must remain selected so the user can retry it next.
      const runningTask = makeTask({ id: 'task-running-x', status: 'running', name: 'task.run' })
      const failedTask = makeTask({ id: 'task-failed-x', status: 'failed', name: 'task.fail' })
      // The cancel mutation invalidates both tasks.all and jobs.all, triggering
      // concurrent refetches. Use mockImplementation (URL-routed) so each call
      // gets a fresh Response body rather than reusing a spent one.
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([runningTask, failedTask])))
      fetchMock.mockResolvedValueOnce(okJson(makeWorkerListResponse([])))
      fetchMock.mockResolvedValueOnce(okJson({ task_id: runningTask.id, status: 'canceled' }))
      fetchMock.mockImplementation((url) => {
        const u = String(url)
        if (u.includes('/workers')) return Promise.resolve(okJson(makeWorkerListResponse([])))
        if (u.includes('/tasks'))
          return Promise.resolve(okJson(makeTaskListResponse([runningTask, failedTask])))
        return Promise.resolve(okJson(makeJob()))
      })

      render(<JobDetail />, { wrapper: Wrapper })

      // Select both tasks individually (the "Select all" header checkbox would
      // also work, but per-task selection is more explicit for this scenario).
      fireEvent.click(await screen.findByRole('checkbox', { name: 'Select task task.run' }))
      fireEvent.click(screen.getByRole('checkbox', { name: 'Select task task.fail' }))

      // Bulk-cancel should show count = 1 (only the running task is cancelable).
      const bulkCancel = screen.getByRole('button', { name: /Cancel selected \(1\)/i })
      expect(bulkCancel).toBeEnabled()
      fireEvent.click(bulkCancel)

      // Wait for the cancel network call to complete.
      await waitFor(() => {
        expect(fetchMock.mock.calls.some(([url]) => String(url).endsWith('/cancel'))).toBe(true)
      })

      // The failed task's checkbox must still be checked — it was not acted on.
      await waitFor(() => {
        expect(screen.getByRole('checkbox', { name: 'Select task task.fail' })).toBeChecked()
      })

      // The running task's checkbox must be unchecked — it was removed from selection.
      await waitFor(() => {
        expect(screen.getByRole('checkbox', { name: 'Select task task.run' })).not.toBeChecked()
      })
    })
  })

  // ── Manual refresh ───────────────────────────────────────────────────

  describe('manual refresh button', () => {
    it('renders a Refresh button after data loads', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('button', { name: 'Refresh job data' }))
      expect(screen.getByRole('button', { name: 'Refresh job data' })).toBeInTheDocument()
    })

    it('clicking Refresh triggers a re-fetch of both job detail and task list', async () => {
      const job = makeJob()
      const tasks = makeTaskListResponse([])
      // The component fires three initial queries: job, tasks, workers. Workers is mocked
      // separately so subsequent mocks are consumed in the expected order.
      fetchMock.mockResolvedValueOnce(okJson(job)) // initial job
      fetchMock.mockResolvedValueOnce(okJson(tasks)) // initial tasks
      fetchMock.mockResolvedValueOnce(okJson(makeWorkerListResponse([]))) // initial workers
      fetchMock.mockResolvedValueOnce(okJson(job)) // refetch job
      fetchMock.mockResolvedValueOnce(okJson(tasks)) // refetch tasks

      render(<JobDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getByRole('button', { name: 'Refresh job data' }))

      const callsBefore = fetchMock.mock.calls.length
      fireEvent.click(screen.getByRole('button', { name: 'Refresh job data' }))

      // Both the job detail endpoint and the task list endpoint must be called.
      await waitFor(() => {
        const newUrls = fetchMock.mock.calls.slice(callsBefore).map((c) => c[0] as string)
        const jobId = 'job-aabbccdd-eeff'
        const hitJob = newUrls.some((u) => u.includes(`/jobs/${jobId}`) && !u.includes('/tasks'))
        const hitTasks = newUrls.some((u) => u.includes(`/jobs/${jobId}/tasks`))
        expect(hitJob).toBe(true)
        expect(hitTasks).toBe(true)
      })
    })
  })

  // ── Unschedulable badge ───────────────────────────────────────────────────

  describe('unschedulable badge', () => {
    it('renders an Unschedulable badge with the reason for a ready task that has one', async () => {
      const task = makeTask({
        id: 'task-unschedulable',
        status: 'ready',
        name: 'task.blocked',
        unschedulable_reason: 'no online workers',
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      const badge = await screen.findByText('Unschedulable')
      expect(badge).toBeInTheDocument()
      // The reason must be exposed via an accessible name or title.
      const hasTitle = badge.getAttribute('title') === 'no online workers'
      const hasAriaLabel = badge.getAttribute('aria-label')?.includes('no online workers') ?? false
      expect(hasTitle || hasAriaLabel).toBe(true)
    })

    it('does not render an Unschedulable badge for a ready task without a reason', async () => {
      const task = makeTask({
        id: 'task-ready-no-reason',
        status: 'ready',
        name: 'task.ready',
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByLabelText('Status: Ready'))
      expect(screen.queryByText('Unschedulable')).not.toBeInTheDocument()
    })

    it('sets the reason via a WS task-update event and clears it when absent', async () => {
      const task = makeTask({
        id: 'task-ws-unschedulable',
        status: 'ready',
        name: 'task.wsblocked',
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))
      fetchMock.mockResolvedValue(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })
      await waitFor(() => screen.getAllByLabelText('Status: Ready'))
      expect(screen.queryByText('Unschedulable')).not.toBeInTheDocument()

      act(() => {
        wsInstance(0).simulateOpen()
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs/job-aabbccdd-eeff/tasks',
          payload: {
            job_id: 'job-aabbccdd-eeff',
            task_id: 'task-ws-unschedulable',
            status: 'ready',
            unschedulable_reason: 'no matching capabilities',
            updated_at: '2024-01-15T10:10:00Z',
          },
          seq: 1,
        })
      })

      await waitFor(() => screen.getByText('Unschedulable'))
      expect(screen.getByText('Unschedulable')).toHaveAttribute('title', 'no matching capabilities')

      // Field absent on a subsequent event means "clear".
      act(() => {
        wsInstance(0).simulateMessage({
          type: 'push',
          subject: 'jobs/job-aabbccdd-eeff/tasks',
          payload: {
            job_id: 'job-aabbccdd-eeff',
            task_id: 'task-ws-unschedulable',
            status: 'ready',
            updated_at: '2024-01-15T10:11:00Z',
          },
          seq: 2,
        })
      })

      await waitFor(() => expect(screen.queryByText('Unschedulable')).not.toBeInTheDocument())
    })
  })

  // ── Task search ───────────────────────────────────────────────────────────

  describe('task search', () => {
    it('filters tasks and steps by name', async () => {
      const task1 = makeTask({
        id: 'task-filter-001',
        step_id: 'step-1',
        name: 'frame-001',
        status: 'running',
      })
      const task2 = makeTask({
        id: 'task-filter-099',
        step_id: 'step-2',
        name: 'frame-099',
        status: 'running',
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task1, task2])))

      render(<JobDetail />, { wrapper: Wrapper })

      const input = await screen.findByLabelText('Search tasks')
      fireEvent.change(input, { target: { value: 'frame-001' } })

      expect(screen.getByLabelText('View logs for task frame-001')).toBeInTheDocument()
      expect(screen.queryByLabelText('View logs for task frame-099')).not.toBeInTheDocument()
    })
  })

  // ── Auto-park banner ─────────────────────────────────────────────────────

  describe('auto-park banner', () => {
    it('shows an auto-park banner with a resume action when park_reason is set', async () => {
      fetchMock.mockResolvedValueOnce(
        okJson(makeJob({ status: 'paused', park_reason: 'failure limit reached (25)' })),
      )
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      expect(await screen.findByText(/failure limit reached \(25\)/i)).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /resume/i })).toBeInTheDocument()
    })

    it('does not render the banner when park_reason is absent', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Test Render Job'))
      expect(screen.queryByRole('button', { name: /resume/i })).not.toBeInTheDocument()
    })

    it('calls PATCH with action=resume when Resume is clicked', async () => {
      const job = makeJob({ status: 'paused', park_reason: 'failure limit reached (5)' })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))
      fetchMock.mockResolvedValueOnce(okJson(makeWorkerListResponse([])))
      fetchMock.mockResolvedValue(okJson({ ...job, status: 'running', park_reason: '' }))

      render(<JobDetail />, { wrapper: Wrapper })

      const resumeBtn = await screen.findByRole('button', { name: /resume/i })
      fireEvent.click(resumeBtn)

      await waitFor(() => {
        const patchCall = fetchMock.mock.calls.find(
          ([, init]) => (init as RequestInit | undefined)?.method === 'PATCH',
        )
        expect(patchCall).toBeDefined()
        expect(String(patchCall?.[0])).toContain(`/jobs/${job.id}`)
        expect(JSON.parse((patchCall?.[1] as RequestInit).body as string)).toEqual({
          action: 'resume',
        })
      })
    })

    it('shows an inline error when resume fails', async () => {
      const job = makeJob({ status: 'paused', park_reason: 'failure limit reached (5)' })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))
      fetchMock.mockResolvedValueOnce(okJson(makeWorkerListResponse([])))
      fetchMock.mockResolvedValueOnce(problemJson(409, 'job is not paused'))

      render(<JobDetail />, { wrapper: Wrapper })

      const resumeBtn = await screen.findByRole('button', { name: /resume/i })
      fireEvent.click(resumeBtn)

      await waitFor(() => screen.getByText(/job is not paused/i))
      expect(screen.getByText(/job is not paused/i)).toBeInTheDocument()
    })
  })

  // ── Retry policy display ─────────────────────────────────────────────────

  describe('retry policy', () => {
    it('shows configured retry policy values', async () => {
      const job = makeJob({ max_attempts: 7, retry_delay_seconds: 45, failure_limit: 12 })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await screen.findByText('Test Render Job')
      expect(screen.getByText('7')).toBeInTheDocument()
      expect(screen.getByText('45s')).toBeInTheDocument()
      expect(screen.getByText('12')).toBeInTheDocument()
    })

    it('shows "inherited" for unset retry policy fields', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await screen.findByText('Test Render Job')
      expect(screen.getAllByText('inherited')).toHaveLength(3)
    })

    it('shows the resolved effective retry policy when present', async () => {
      const job = makeJob({
        effective_retry: { max_attempts: 3, retry_delay_seconds: 30, failure_limit: 5 },
      })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await screen.findByText('Test Render Job')
      expect(screen.getByText('Retries')).toBeInTheDocument()
      expect(screen.getByText('3 attempts · 30s delay · limit 5')).toBeInTheDocument()
    })

    it('shows "limit off" when the effective failure limit is 0', async () => {
      const job = makeJob({
        effective_retry: { max_attempts: 1, retry_delay_seconds: 0, failure_limit: 0 },
      })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await screen.findByText('Test Render Job')
      expect(screen.getByText('1 attempt · 0s delay · limit off')).toBeInTheDocument()
    })

    it('renders no effective policy row when effective_retry is absent', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await screen.findByText('Test Render Job')
      expect(screen.queryByText('Retries')).not.toBeInTheDocument()
      expect(screen.queryByText(/limit off/)).not.toBeInTheDocument()
    })
  })

  // ── Task attempt indicator and retry countdown ───────────────────────────

  describe('task attempt indicator and retry countdown', () => {
    it('shows an attempt indicator for a task that has genuinely failed before', async () => {
      const task = makeTask({ status: 'ready', failed_attempts: 1 })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      expect(await screen.findByText('attempt 2')).toBeInTheDocument()
    })

    it('does not show an attempt indicator for a task that has never failed', async () => {
      const task = makeTask({ status: 'running', failed_attempts: 0 })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByLabelText('Status: Running'))
      expect(screen.queryByText(/^attempt /)).not.toBeInTheDocument()
    })

    it('shows a "retrying in Ns" hint for a ready task backing off with a future retry_after', async () => {
      const future = new Date(Date.now() + 30_000).toISOString()
      const task = makeTask({ status: 'ready', retry_after: future, failed_attempts: 1 })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      expect(await screen.findByText(/retrying in \d+s/)).toBeInTheDocument()
    })

    it('does not show a retrying hint once retry_after has passed', async () => {
      const past = new Date(Date.now() - 5_000).toISOString()
      const task = makeTask({ status: 'ready', retry_after: past })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByLabelText('Status: Ready'))
      expect(screen.queryByText(/retrying in/)).not.toBeInTheDocument()
    })

    it('does not show a retrying hint for a non-ready task even with a future retry_after', async () => {
      const future = new Date(Date.now() + 30_000).toISOString()
      const task = makeTask({ status: 'pending', retry_after: future })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getAllByLabelText('Status: Pending'))
      expect(screen.queryByText(/retrying in/)).not.toBeInTheDocument()
    })
  })

  // ── Failure banner and per-task failure reason ───────────────────────────

  describe('failure banner', () => {
    it('shows a failure banner with the dominant reason and count', async () => {
      const job = makeJob({
        status: 'failed',
        failure_summary: {
          failed_count: 50,
          dominant_reason: 'worker not configured for staging',
          distinct_reasons: 1,
        },
      })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      const banner = await screen.findByRole('alert')
      expect(banner).toHaveTextContent(/50 tasks failed/i)
      expect(banner).toHaveTextContent(/worker not configured for staging/i)
    })

    it('notes multiple reasons when they differ', async () => {
      const job = makeJob({
        status: 'failed',
        failure_summary: { failed_count: 50, dominant_reason: 'staging', distinct_reasons: 3 },
      })
      fetchMock.mockResolvedValueOnce(okJson(job))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      const banner = await screen.findByRole('alert')
      expect(banner).toHaveTextContent(/3 reasons/i)
    })

    it('does not render the banner when failure_summary is absent', async () => {
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([])))

      render(<JobDetail />, { wrapper: Wrapper })

      await waitFor(() => screen.getByText('Test Render Job'))
      expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    })

    it("shows a failed task's own failure reason", async () => {
      const task = makeTask({
        status: 'failed',
        failure_reason: 'execution timeout after 120s',
      })
      fetchMock.mockResolvedValueOnce(okJson(makeJob()))
      fetchMock.mockResolvedValueOnce(okJson(makeTaskListResponse([task])))

      render(<JobDetail />, { wrapper: Wrapper })

      expect(await screen.findByText(/execution timeout after 120s/i)).toBeInTheDocument()
    })
  })
})
