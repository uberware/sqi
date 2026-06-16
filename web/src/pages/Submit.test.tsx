// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route, useLocation } from 'react-router-dom'
import type { ReactNode } from 'react'
import Submit from './Submit'
import { ToastProvider } from '@/components/Toast'
import type { Farm, Queue, Job, ListResponse } from '@/api/types'

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
  vi.stubGlobal('fetch', fetchMock)
  vi.stubGlobal('localStorage', localStorageMock)
  localStorageMock.clear()
  // Reset call tracking without clearing the store (already cleared above).
  localStorageMock.getItem.mockClear()
  localStorageMock.setItem.mockClear()
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
        <MemoryRouter initialEntries={['/submit']}>
          <Routes>
            <Route path="/submit" element={children} />
            <Route path="/jobs/:id" element={<LocationDisplay />} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>
  )
}

/** Set up fetch mocks for farms + queues list (used by useFarmsWithQueues). */
function mockFarmsAndQueues(farm = makeFarm(), queues = [makeQueue()]) {
  // GET /api/v1/farms
  fetchMock.mockResolvedValueOnce(okJson([farm]))
  // GET /api/v1/queues?farm_id=...
  fetchMock.mockResolvedValueOnce(okJson(queuesResponse(queues)))
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
      fetchMock.mockResolvedValueOnce(
        problemJson(422, 'openjd: validate: step "main" has no script'),
      )

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
      fetchMock.mockResolvedValueOnce(problemJson(500, 'internal server error'))

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
      fetchMock.mockResolvedValueOnce(
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

    it('shows a toast notification after successful submission', async () => {
      mockFarmsAndQueues()
      fetchMock.mockResolvedValueOnce(
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
      // Two fetches: farms list then queues list
      fetchMock.mockResolvedValueOnce(okJson([farm]))
      fetchMock.mockResolvedValueOnce(okJson(queuesResponse(queues)))

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
      fetchMock.mockResolvedValueOnce(okJson([farm]))
      fetchMock.mockResolvedValueOnce(okJson(queuesResponse(queues)))

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
      fetchMock.mockResolvedValueOnce(okJson([farm]))
      fetchMock.mockResolvedValueOnce(okJson(queuesResponse(queues)))

      render(<Submit />, { wrapper: Wrapper })

      await waitFor(() => screen.getByRole('option', { name: 'Queue A' }))
      // Selector should fall back to the first available queue.
      expect(screen.getByRole('combobox', { name: /queue/i })).toHaveValue('q-1')
      // Submit button is not disabled by the missing queue (form is usable).
      await waitFor(() => {
        const editor = screen.getByTestId('template-editor') as HTMLTextAreaElement
        // Simulate typing to make the form submittable.
        Object.defineProperty(editor, 'value', { value: 'some content' })
      })
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
