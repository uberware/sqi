// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route, useLocation } from 'react-router-dom'
import type { ReactNode } from 'react'
import Submit from './Submit'
import { ToastProvider } from '@/components/Toast'
import type { Farm, Queue, Job, ListResponse, Principal } from '@/api/types'

// ── Auth mock ─────────────────────────────────────────────────────────────────
// Submit reads useAuth() to gate the Owner field behind 'jobs.submit_as', the
// same pattern ProductSubmit.test.tsx already uses. Default every test to an
// operator principal (holds jobs.submit_as) so every pre-existing assertion,
// which predates permission gating, keeps seeing the Owner field unchanged;
// the gating tests below override via renderWithAuth.

const OPERATOR_PRINCIPAL: Principal = {
  subject: 'u-operator',
  display_name: 'Operator',
  roles: ['operator'],
  kind: 'user',
  permissions: OPERATOR_PERMISSIONS,
}

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(() => ({ principal: OPERATOR_PRINCIPAL, status: 'authed', refresh: () => {} })),
}))
import { useAuth } from '@/auth/context'
import { ALL_PERMISSIONS, OPERATOR_PERMISSIONS, USER_PERMISSIONS } from '@/test/principals'
import type { Permission } from '@/auth/policy'

// ── Mock CodeEditor ───────────────────────────────────────────────────────────
// CodeMirror requires DOM APIs (ResizeObserver, getComputedStyle internals)
// not fully supported in jsdom. Replace with a plain textarea for testing.

vi.mock('@/components/CodeEditor', () => ({
  default: ({
    value,
    onChange,
    'aria-label': ariaLabel,
    'data-testid': testId,
  }: {
    value: string
    onChange: (v: string) => void
    'aria-label'?: string
    'data-testid'?: string
  }) => (
    <textarea
      value={value}
      onChange={(e) => onChange(e.target.value)}
      aria-label={ariaLabel}
      data-testid={testId}
    />
  ),
}))

// ── Fixtures ──────────────────────────────────────────────────────────────────

function makeFarm(overrides: Partial<Farm> = {}): Farm {
  return {
    id: 'farm-1',
    name: 'Main Farm',
    description: '',
    max_concurrent_tasks: 10,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

function makeQueue(overrides: Partial<Queue> = {}): Queue {
  return {
    id: 'queue-1',
    farm_id: 'farm-1',
    name: 'Default',
    priority: 50,
    max_concurrent_tasks: 5,
    paused: false,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

function makeJob(overrides: Partial<Job> = {}): Job {
  return {
    id: 'job-abc123',
    farm_id: 'farm-1',
    queue_id: 'queue-1',
    name: 'Test Job',
    owner: 'user',
    submitter: 'user',
    priority: 50,
    status: 'pending',
    template_format: 'yaml',
    failed_attempts: 0,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  }
}

// ── Response helpers ──────────────────────────────────────────────────────────

function okJson(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function problemJson(status: number, detail: string): Response {
  return new Response(JSON.stringify({ type: 'about:blank', title: 'Error', status, detail }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

function queuesResponse(queues: Queue[]): ListResponse<Queue> {
  return { items: queues, total: queues.length, limit: 100, offset: 0 }
}

function jobsResponse(jobs: Job[]): ListResponse<Job> {
  return { items: jobs, total: jobs.length, limit: 200, offset: 0 }
}

/**
 * Set (or cleared in `beforeEach`) by {@link mockJobSubmit} to answer the
 * `POST /jobs` submission call. Routed by method rather than call order
 * because the Submit page now fires the farms+queues query and the
 * depends-on candidate-jobs query independently — their relative timing
 * isn't guaranteed, so a strictly-ordered `mockResolvedValueOnce` queue could
 * have an early GET consume the response meant for the POST.
 */
let jobSubmitResponder: (() => Promise<Response>) | null = null

/** Queue the response for the next `POST /jobs` submission call. */
function mockJobSubmit(response: Response) {
  jobSubmitResponder = () => Promise.resolve(response)
}

/**
 * URL/method-routed fetch responder for farms/queues/job-list GETs and the
 * job-submission POST, used in place of strictly-ordered `mockResolvedValueOnce`
 * chaining (see {@link jobSubmitResponder}).
 */
function routeFetch(farm: Farm, queues: Queue[], jobs: Job[] = []) {
  return (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = String(input)
    const method = init?.method ?? 'GET'
    if (method === 'POST' && url.includes('/jobs')) {
      if (jobSubmitResponder) return jobSubmitResponder()
      return Promise.reject(new Error(`POST /jobs called without a mocked response: ${url}`))
    }
    if (url.includes('/queues')) return Promise.resolve(okJson(queuesResponse(queues)))
    if (url.includes('/farms')) return Promise.resolve(okJson([farm]))
    if (url.includes('/jobs')) {
      // Mirror the real server's farm_id filter so tests can exercise
      // farm-scoping of the depends-on candidate list.
      const farmId = new URL(url, 'http://localhost').searchParams.get('farm_id')
      const filtered = farmId ? jobs.filter((j) => j.farm_id === farmId) : jobs
      return Promise.resolve(okJson(jobsResponse(filtered)))
    }
    return Promise.reject(new Error(`unmocked fetch: ${method} ${url}`))
  }
}

// ── Test setup ────────────────────────────────────────────────────────────────

// jsdom uses a null origin by default, which blocks localStorage access.
// Replace it with a simple in-memory mock so Submit's queue persistence works.
const localStorageMock = (() => {
  const store = new Map<string, string>()
  return {
    getItem: vi.fn((key: string): string | null => store.get(key) ?? null),
    setItem: vi.fn((key: string, value: string): void => {
      store.set(key, value)
    }),
    removeItem: vi.fn((key: string): void => {
      store.delete(key)
    }),
    clear: vi.fn((): void => {
      store.clear()
    }),
    get length() {
      return store.size
    },
    key: vi.fn((index: number): string | null => [...store.keys()][index] ?? null),
  }
})()

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  jobSubmitResponder = null
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('localStorage', localStorageMock)
  localStorageMock.clear()
  // Reset call tracking without clearing the store (already cleared above).
  localStorageMock.getItem.mockClear()
  localStorageMock.setItem.mockClear()
  // Default every test to an operator principal (holds jobs.submit_as) so
  // pre-existing assertions, which predate permission gating, keep seeing the
  // Owner field unchanged; the gating tests below override via renderWithAuth.
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal: OPERATOR_PRINCIPAL,
    status: 'authed',
    refresh: () => {},
  })
})

afterEach(() => {
  vi.restoreAllMocks()
})

function makeQueryClient() {
  return new QueryClient({
    defaultOptions: {
      queries: { retry: false, gcTime: 0 },
      mutations: { retry: false },
    },
  })
}

/** Helper component that shows the current path — used to assert navigation. */
function LocationDisplay() {
  const location = useLocation()
  return <div data-testid="location">{location.pathname}</div>
}

function Wrapper({ children }: { children: ReactNode }) {
  return (
    <QueryClientProvider client={makeQueryClient()}>
      <ToastProvider>
        <MemoryRouter initialEntries={['/submit/raw']}>
          <Routes>
            <Route path="/submit/raw" element={children} />
            <Route path="/jobs/:id" element={<LocationDisplay />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>
  )
}

/**
 * Set up fetch mocks for farms + queues list (used by useFarmsWithQueues) and
 * the depends-on candidate-jobs list (used by the Dependencies section).
 */
function mockFarmsAndQueues(farm = makeFarm(), queues = [makeQueue()], jobs: Job[] = []) {
  fetchMock.mockImplementation(routeFetch(farm, queues, jobs))
}

/** Renders <Submit /> with the mocked useAuth() principal holding `permissions`. */
function renderWithAuth({ permissions }: { permissions: Permission[] }) {
  const principal: Principal = {
    subject: 'u-test',
    display_name: 'Test User',
    roles: [],
    kind: 'user',
    permissions,
  }
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal,
    status: 'authed',
    refresh: () => {},
  })
  return render(<Submit />, { wrapper: Wrapper })
}

// ── Tests ─────────────────────────────────────────────────────────────────────

describe('Submit page', () => {
  describe('queue selector', () => {
    it('renders queues grouped by farm', async () => {
      mockFarmsAndQueues()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => {
        expect(screen.getByRole('option', { name: 'Default' })).toBeInTheDocument()
      })

      // The optgroup label should match the farm name.
      const select = screen.getByRole('combobox', { name: /queue/i })
      expect(select).toBeInTheDocument()
    })

    it('shows loading state while queues are fetching', () => {
      // Never resolves during this test
      fetchMock.mockReturnValue(new Promise(() => {}))
      render(<Submit />, { wrapper: Wrapper })
      expect(screen.getByRole('option', { name: 'Loading queues…' })).toBeInTheDocument()
    })
  })

  describe('Load example dropdown', () => {
    it('populates the editor with the shell example on selection', async () => {
      mockFarmsAndQueues()
      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      const select = screen.getByRole('combobox', { name: /load example/i })
      await user.selectOptions(select, 'shell')

      const editor = screen.getByTestId('template-editor') as HTMLTextAreaElement
      expect(editor.value).toContain('specificationVersion')
      expect(editor.value).toContain('hello-world')
      // Select resets to placeholder after picking.
      expect(select).toHaveValue('')
    })

    it('populates the editor with the parameter-space example on selection', async () => {
      mockFarmsAndQueues()
      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      const select = screen.getByRole('combobox', { name: /load example/i })
      await user.selectOptions(select, 'parameter-space')

      const editor = screen.getByTestId('template-editor') as HTMLTextAreaElement
      expect(editor.value).toContain('frame-render')
      expect(editor.value).toContain('taskParameterDefinitions')
    })

    it('populates the editor with the usage-limit example on selection', async () => {
      mockFarmsAndQueues()
      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      const select = screen.getByRole('combobox', { name: /load example/i })
      await user.selectOptions(select, 'usage-limit')

      const editor = screen.getByTestId('template-editor') as HTMLTextAreaElement
      expect(editor.value).toContain('amount.worker.usagepool.arnold')
      expect(editor.value).toContain('hostRequirements')
    })
  })

  describe('validation error display', () => {
    it('shows the server detail string below the editor when submission fails', async () => {
      mockFarmsAndQueues()
      // POST /api/v1/jobs returns a 422 validation error
      mockJobSubmit(problemJson(422, 'openjd: validate: step "main" has no script'))

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      // Wait for queues to load
      await waitFor(() => screen.getByRole('option', { name: 'Default' }))

      // Fill in the template editor
      const editor = screen.getByTestId('template-editor')
      await user.type(editor, 'specificationVersion: "jobtemplate-2023-09"')

      // Submit the form
      const submitBtn = screen.getByRole('button', { name: /submit job/i })
      await user.click(submitBtn)

      await waitFor(() => {
        expect(screen.getByRole('alert')).toBeInTheDocument()
      })
      expect(screen.getByText(/step "main" has no script/i)).toBeInTheDocument()
    })

    it('shows a generic error message for unexpected failures', async () => {
      mockFarmsAndQueues()
      mockJobSubmit(problemJson(500, 'internal server error'))

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))

      const editor = screen.getByTestId('template-editor')
      await user.type(editor, 'specificationVersion: "jobtemplate-2023-09"')

      await user.click(screen.getByRole('button', { name: /submit job/i }))

      await waitFor(() => {
        expect(screen.getByRole('alert')).toBeInTheDocument()
      })
      expect(screen.getByText(/internal server error/i)).toBeInTheDocument()
    })
  })

  describe('successful submission', () => {
    it('redirects to the job detail page after a successful submit', async () => {
      mockFarmsAndQueues()
      // POST /api/v1/jobs → 201 with the new job
      mockJobSubmit(
        new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        }),
      )

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))

      const editor = screen.getByTestId('template-editor')
      await user.type(editor, 'specificationVersion: "jobtemplate-2023-09"')

      await user.click(screen.getByRole('button', { name: /submit job/i }))

      // After success, the router should navigate to the job detail page.
      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
      })
    })

    it('sends entered retry-policy overrides as query params', async () => {
      mockFarmsAndQueues()
      mockJobSubmit(
        new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        }),
      )

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))

      await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
      await user.type(screen.getByLabelText(/max attempts per task/i), '5')
      await user.type(screen.getByLabelText(/retry delay/i), '30')
      await user.type(screen.getByLabelText(/failure limit/i), '10')
      await user.click(screen.getByRole('button', { name: /submit job/i }))

      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
      })
      const postCall = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      if (!postCall) throw new Error('expected a POST call')
      const url = String(postCall[0])
      expect(url).toContain('max_attempts=5')
      expect(url).toContain('retry_delay_seconds=30')
      expect(url).toContain('failure_limit=10')
    })

    it('omits retry-policy query params when left blank', async () => {
      mockFarmsAndQueues()
      mockJobSubmit(
        new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        }),
      )

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))

      await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
      await user.click(screen.getByRole('button', { name: /submit job/i }))

      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
      })
      const postCall = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      if (!postCall) throw new Error('expected a POST call')
      const url = String(postCall[0])
      expect(url).not.toContain('max_attempts=')
      expect(url).not.toContain('retry_delay_seconds=')
      expect(url).not.toContain('failure_limit=')
    })

    it('shows a toast notification after successful submission', async () => {
      mockFarmsAndQueues()
      mockJobSubmit(
        new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        }),
      )

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))

      await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
      await user.click(screen.getByRole('button', { name: /submit job/i }))

      await waitFor(() => {
        expect(screen.getByRole('status')).toHaveTextContent(/job submitted/i)
      })
    })
  })

  describe('localStorage queue persistence', () => {
    it('persists the selected queue ID to localStorage on change', async () => {
      const farm = makeFarm()
      const queues = [
        makeQueue({ id: 'q-1', name: 'Queue A' }),
        makeQueue({ id: 'q-2', name: 'Queue B' }),
      ]
      mockFarmsAndQueues(farm, queues)

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Queue B' }))
      const queueSelect = screen.getByRole('combobox', { name: /queue/i })
      await user.selectOptions(queueSelect, 'q-2')

      expect(localStorageMock.setItem).toHaveBeenCalledWith('sqi:submit:last-queue-id', 'q-2')
    })

    it('pre-selects the stored queue on mount when it is still in the list', async () => {
      // Pre-populate the store before rendering
      localStorageMock.getItem.mockImplementation((key: string) =>
        key === 'sqi:submit:last-queue-id' ? 'q-2' : null,
      )

      const farm = makeFarm()
      const queues = [
        makeQueue({ id: 'q-1', name: 'Queue A' }),
        makeQueue({ id: 'q-2', name: 'Queue B' }),
      ]
      mockFarmsAndQueues(farm, queues)

      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Queue B' }))
      expect(screen.getByRole('combobox', { name: /queue/i })).toHaveValue('q-2')
    })

    it('falls back to the first queue when the stored ID is no longer in the list', async () => {
      // Stored ID references a queue that no longer exists
      localStorageMock.getItem.mockImplementation((key: string) =>
        key === 'sqi:submit:last-queue-id' ? 'q-deleted' : null,
      )

      const farm = makeFarm()
      const queues = [makeQueue({ id: 'q-1', name: 'Queue A' })]
      mockFarmsAndQueues(farm, queues)

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Queue A' }))
      // Selector should fall back to the first available queue.
      expect(screen.getByRole('combobox', { name: /queue/i })).toHaveValue('q-1')

      // Submit button is not disabled by the missing queue (form is usable).
      await user.type(screen.getByTestId('template-editor'), 'some content')
      expect(screen.getByRole('button', { name: /submit job/i })).toBeEnabled()
    })
  })

  describe('Dependencies section', () => {
    it('lists non-terminal jobs from the selected farm as depends-on candidates', async () => {
      const farm = makeFarm()
      const queues = [makeQueue()]
      const jobs = [
        makeJob({ id: 'job-a', name: 'Upstream A', status: 'running' }),
        makeJob({ id: 'job-b', name: 'Upstream B', status: 'completed' }),
        makeJob({ id: 'job-c', name: 'Other Farm Job', farm_id: 'farm-2' }),
      ]
      mockFarmsAndQueues(farm, queues, jobs)

      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))
      await waitFor(() => {
        expect(screen.getByRole('checkbox', { name: /Upstream A/ })).toBeInTheDocument()
      })
      // Terminal (completed) job and a job from a different farm are excluded.
      expect(screen.queryByRole('checkbox', { name: /Upstream B/ })).not.toBeInTheDocument()
      expect(screen.queryByRole('checkbox', { name: /Other Farm Job/ })).not.toBeInTheDocument()
    })

    it('submits selected depends-on jobs as repeated depends_on params', async () => {
      const farm = makeFarm()
      const queues = [makeQueue()]
      const jobs = [
        makeJob({ id: 'job-a', name: 'Upstream A', status: 'running' }),
        makeJob({ id: 'job-b', name: 'Upstream B', status: 'pending' }),
      ]
      mockFarmsAndQueues(farm, queues, jobs)
      mockJobSubmit(
        new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        }),
      )

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))
      await waitFor(() => screen.getByRole('checkbox', { name: /Upstream A/ }))

      await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
      await user.click(screen.getByRole('checkbox', { name: /Upstream A/ }))
      await user.click(screen.getByRole('checkbox', { name: /Upstream B/ }))
      await user.click(screen.getByRole('button', { name: /submit job/i }))

      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
      })
      const postCall = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      if (!postCall) throw new Error('expected a POST call')
      const url = String(postCall[0])
      expect(url).toContain('depends_on=job-a')
      expect(url).toContain('depends_on=job-b')
    })

    it('omits depends_on when no upstream jobs are selected', async () => {
      mockFarmsAndQueues()
      mockJobSubmit(
        new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        }),
      )

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))
      await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
      await user.click(screen.getByRole('button', { name: /submit job/i }))

      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
      })
      const postCall = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      if (!postCall) throw new Error('expected a POST call')
      expect(String(postCall[0])).not.toContain('depends_on')
    })

    it('clears selected depends-on jobs when switching to a queue in a different farm', async () => {
      const farmA = makeFarm({ id: 'farm-a', name: 'Farm A' })
      const farmB = makeFarm({ id: 'farm-b', name: 'Farm B' })
      const queueA = makeQueue({ id: 'q-a', farm_id: 'farm-a', name: 'Queue A' })
      const queueB = makeQueue({ id: 'q-b', farm_id: 'farm-b', name: 'Queue B' })
      const jobs = [
        makeJob({ id: 'job-a', name: 'Upstream A', farm_id: 'farm-a', status: 'running' }),
      ]

      fetchMock.mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
        const url = String(input)
        const method = init?.method ?? 'GET'
        if (method === 'POST' && url.includes('/jobs')) {
          return jobSubmitResponder
            ? jobSubmitResponder()
            : Promise.reject(new Error(`POST /jobs called without a mocked response: ${url}`))
        }
        if (url.includes('/queues')) {
          const farmId = new URL(url, 'http://localhost').searchParams.get('farm_id')
          return Promise.resolve(
            okJson(queuesResponse([queueA, queueB].filter((q) => q.farm_id === farmId))),
          )
        }
        if (url.includes('/farms')) return Promise.resolve(okJson([farmA, farmB]))
        if (url.includes('/jobs')) {
          const farmId = new URL(url, 'http://localhost').searchParams.get('farm_id')
          const filtered = farmId ? jobs.filter((j) => j.farm_id === farmId) : jobs
          return Promise.resolve(okJson(jobsResponse(filtered)))
        }
        return Promise.reject(new Error(`unmocked fetch: ${method} ${url}`))
      })
      mockJobSubmit(
        new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
          status: 201,
          headers: { 'Content-Type': 'application/json' },
        }),
      )

      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('checkbox', { name: /Upstream A/ }))
      await user.click(screen.getByRole('checkbox', { name: /Upstream A/ }))

      const queueSelect = screen.getByRole('combobox', { name: /queue/i })
      await user.selectOptions(queueSelect, 'q-b')

      // The now-irrelevant candidate drops out of the picker once its farm's
      // jobs are no longer in scope.
      await waitFor(() => {
        expect(screen.queryByRole('checkbox', { name: /Upstream A/ })).not.toBeInTheDocument()
      })

      await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
      await user.click(screen.getByRole('button', { name: /submit job/i }))

      await waitFor(() => {
        expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
      })
      const postCall = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      if (!postCall) throw new Error('expected a POST call')
      // The stale farm-A selection must not silently ride along on the farm-B submission.
      expect(String(postCall[0])).not.toContain('depends_on')
    })
  })

  describe('submit button state', () => {
    it('is disabled when template is empty', async () => {
      mockFarmsAndQueues()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))

      expect(screen.getByRole('button', { name: /submit job/i })).toBeDisabled()
    })

    it('is enabled when a queue is selected and template is non-empty', async () => {
      mockFarmsAndQueues()
      const user = userEvent.setup()
      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Default' }))

      await user.type(screen.getByTestId('template-editor'), 'some template content')
      expect(screen.getByRole('button', { name: /submit job/i })).toBeEnabled()
    })
  })
})

describe('owner field permission gating', () => {
  it('hides the Owner input without jobs.submit_as', async () => {
    mockFarmsAndQueues()
    renderWithAuth({ permissions: USER_PERMISSIONS })

    await waitFor(() => screen.getByRole('option', { name: 'Default' }))
    expect(screen.queryByLabelText('Owner')).not.toBeInTheDocument()
  })

  it('shows the Owner input with jobs.submit_as', async () => {
    mockFarmsAndQueues()
    renderWithAuth({ permissions: OPERATOR_PERMISSIONS })

    await waitFor(() => screen.getByRole('option', { name: 'Default' }))
    expect(screen.getByLabelText('Owner')).toBeInTheDocument()
  })

  it('omits owner from the submitted request without jobs.submit_as', async () => {
    mockFarmsAndQueues()
    mockJobSubmit(
      new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
        status: 201,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    const user = userEvent.setup()
    renderWithAuth({ permissions: USER_PERMISSIONS })

    await waitFor(() => screen.getByRole('option', { name: 'Default' }))
    await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
    await user.click(screen.getByRole('button', { name: /submit job/i }))

    await waitFor(() => {
      expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
    })
    const postCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
    )
    if (!postCall) throw new Error('expected a POST call')
    expect(String(postCall[0])).not.toContain('owner=')
  })

  it('sends the entered owner in the request with jobs.submit_as', async () => {
    mockFarmsAndQueues()
    mockJobSubmit(
      new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
        status: 201,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    const user = userEvent.setup()
    renderWithAuth({ permissions: OPERATOR_PERMISSIONS })

    await waitFor(() => screen.getByRole('option', { name: 'Default' }))
    await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
    await user.type(screen.getByLabelText('Owner'), 'alice')
    await user.click(screen.getByRole('button', { name: /submit job/i }))

    await waitFor(() => {
      expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
    })
    const postCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
    )
    if (!postCall) throw new Error('expected a POST call')
    expect(String(postCall[0])).toContain('owner=alice')
  })

  it('keeps the Owner field visible and functional with auth off (anonymous principal)', async () => {
    mockFarmsAndQueues()
    mockJobSubmit(
      new Response(JSON.stringify(makeJob({ id: 'abc-def-123' })), {
        status: 201,
        headers: { 'Content-Type': 'application/json' },
      }),
    )

    const anonymousPrincipal: Principal = {
      subject: 'anonymous',
      display_name: 'Anonymous',
      roles: [],
      kind: 'anonymous',
      permissions: ALL_PERMISSIONS,
    }
    ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
      principal: anonymousPrincipal,
      status: 'authed',
      refresh: () => {},
    })

    const user = userEvent.setup()
    render(<Submit />, { wrapper: Wrapper })

    await waitFor(() => screen.getByRole('option', { name: 'Default' }))
    expect(screen.getByLabelText('Owner')).toBeInTheDocument()

    await user.type(screen.getByTestId('template-editor'), 'specificationVersion: test')
    await user.type(screen.getByLabelText('Owner'), 'bob')
    await user.click(screen.getByRole('button', { name: /submit job/i }))

    await waitFor(() => {
      expect(screen.getByTestId('location')).toHaveTextContent('/jobs/abc-def-123')
    })
    const postCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
    )
    if (!postCall) throw new Error('expected a POST call')
    expect(String(postCall[0])).toContain('owner=bob')
  })
})
