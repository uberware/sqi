// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, it, expect, vi, beforeEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import type { ReactElement } from 'react'
import { ToastProvider } from '@/components/Toast'
import type { Principal } from '@/api/types'
import ProductSubmit from './ProductSubmit'

// ── Auth mock ─────────────────────────────────────────────────────────────────
// ProductSubmit reads useAuth() to gate the Owner field behind 'jobs.submit_as'.
// Mock the auth context directly (as JobList.test.tsx/Admin.test.tsx do) and
// default every test to an operator principal so pre-existing tests, which
// predate permission gating and don't care about the Owner field, keep seeing
// it exactly as before.

const OPERATOR_PRINCIPAL: Principal = {
  subject: 'u-operator',
  display_name: 'Operator',
  roles: ['operator'],
  kind: 'user',
  permissions: [],
}

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(() => ({ principal: OPERATOR_PRINCIPAL, status: 'authed', refresh: () => {} })),
}))
import { useAuth } from '@/auth/context'

const submitMock = vi.fn().mockResolvedValue({ id: 'job-1' })
const navigateMock = vi.fn()

// Parameter data lives in mutable hoisted state so a test can simulate the
// product being edited (its parsed parameters changing) between renders.
const h = vi.hoisted(() => {
  const sceneParam = (def: string | null) => ({
    name: 'Scene',
    type: 'PATH' as const,
    description: '',
    default: def,
    allowed_values: null,
    min_value: null,
    max_value: null,
    min_length: null,
    max_length: null,
    object_type: 'FILE',
    data_flow: '',
    user_interface: null,
  })
  const internalParam = () => ({
    name: 'InternalPath',
    type: 'PATH' as const,
    description: '',
    default: '/internal/default',
    allowed_values: null,
    min_value: null,
    max_value: null,
    min_length: null,
    max_length: null,
    object_type: 'FILE',
    data_flow: '',
    user_interface: {
      control: 'HIDDEN' as const,
      label: '',
      group_label: '',
      decimals: null,
      single_step_removal: null,
    },
  })
  const defaultParams = () => [sceneParam(null), internalParam()]
  const makeJob = (overrides: Partial<Record<string, unknown>> = {}) => ({
    id: 'job-a',
    farm_id: 'f1',
    queue_id: 'q1',
    name: 'Upstream A',
    owner: 'user',
    submitter: 'user',
    priority: 50,
    status: 'running',
    template_format: 'yaml',
    failed_attempts: 0,
    created_at: '2024-01-01T00:00:00Z',
    updated_at: '2024-01-01T00:00:00Z',
    ...overrides,
  })
  return {
    sceneParam,
    internalParam,
    defaultParams,
    makeJob,
    state: { params: defaultParams(), jobs: [makeJob()] },
  }
})

vi.mock('react-router-dom', async (orig) => ({
  ...(await orig<typeof import('react-router-dom')>()),
  useNavigate: () => navigateMock,
}))

vi.mock('@/api/queries', async (orig) => ({
  ...(await orig<typeof import('@/api/queries')>()),
  useProduct: () => ({
    data: {
      name: 'blender',
      title: 'Blender',
      description: '',
      category: 'rendering',
      version: '1.0',
      source: 'builtin' as const,
      template: '',
      format: 'yaml' as const,
    },
    isLoading: false,
    error: null,
  }),
  useProductParameters: () => ({
    data: h.state.params,
    isLoading: false,
    error: null,
  }),
  // Bug #2 corrected: use real FarmWithQueues shape { farm, queues } not { id, name, queues }
  useFarmsWithQueues: () => ({
    data: [
      {
        farm: {
          id: 'f1',
          name: 'Farm',
          max_concurrent_tasks: 10,
          created_at: '2024-01-01T00:00:00Z',
          updated_at: '2024-01-01T00:00:00Z',
        },
        queues: [
          {
            id: 'q1',
            farm_id: 'f1',
            name: 'Queue',
            priority: 50,
            max_concurrent_tasks: 5,
            paused: false,
            created_at: '2024-01-01T00:00:00Z',
            updated_at: '2024-01-01T00:00:00Z',
          },
        ],
      },
    ],
    isLoading: false,
    error: null,
  }),
  useListJobs: () => ({
    data: { items: h.state.jobs, total: h.state.jobs.length, limit: 200, offset: 0 },
    isLoading: false,
    error: null,
  }),
}))

vi.mock('@/api/mutations', async (orig) => ({
  ...(await orig<typeof import('@/api/mutations')>()),
  useSubmitProductJob: () => ({ mutateAsync: submitMock, isPending: false }),
}))

beforeEach(() => {
  submitMock.mockClear()
  navigateMock.mockClear()
  // Default every test to an operator principal (holds jobs.submit_as) so
  // pre-existing assertions, which predate permission gating, keep seeing the
  // Owner field unchanged; the gating tests below override via renderWithAuth.
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal: OPERATOR_PRINCIPAL,
    status: 'authed',
    refresh: () => {},
  })
  h.state.params = h.defaultParams()
  h.state.jobs = [h.makeJob()]
  // jsdom uses a null origin by default which blocks localStorage access.
  vi.stubGlobal('localStorage', {
    getItem: vi.fn().mockReturnValue(null),
    setItem: vi.fn(),
    removeItem: vi.fn(),
  })
})

function tree(qc: QueryClient, ui: ReactElement = <ProductSubmit />) {
  return (
    <QueryClientProvider client={qc}>
      <ToastProvider>
        <MemoryRouter initialEntries={['/submit/product/blender']}>
          <Routes>
            <Route path="/submit/product/:name" element={ui} />
          </Routes>
        </MemoryRouter>
      </ToastProvider>
    </QueryClientProvider>
  )
}

function renderPage() {
  return render(tree(new QueryClient()))
}

/** Renders `ui` with the mocked useAuth() principal set to the given roles. */
function renderWithAuth(ui: ReactElement, { roles }: { roles: string[] }) {
  const principal: Principal = {
    subject: 'u-test',
    display_name: 'Test User',
    roles,
    kind: 'user',
    permissions: [],
  }
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal,
    status: 'authed',
    refresh: () => {},
  })
  return render(tree(new QueryClient(), ui))
}

describe('ProductSubmit', () => {
  it('blocks submit when a required field is empty', async () => {
    renderPage()
    fireEvent.click(screen.getByRole('button', { name: /submit/i }))
    expect(await screen.findByText(/required/i)).toBeInTheDocument()
    expect(submitMock).not.toHaveBeenCalled()
  })

  it('submits with name + parameters and navigates to the job', async () => {
    renderPage()
    fireEvent.change(screen.getByLabelText(/Scene/), { target: { value: '/proj/a.blend' } })
    fireEvent.click(screen.getByRole('button', { name: /submit/i }))
    await waitFor(() => expect(submitMock).toHaveBeenCalled())
    const arg = submitMock.mock.calls[0]?.[0] as Record<string, unknown>
    expect(arg).toMatchObject({
      productName: 'blender',
      farmId: 'f1',
      queueId: 'q1',
      parameters: { Scene: '/proj/a.blend' },
    })
    expect(arg.name as string).toMatch(/^Blender /)
    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith('/jobs/job-1'))
  })

  it('omits HIDDEN params from the submitted parameters payload', async () => {
    renderPage()
    fireEvent.change(screen.getByLabelText(/Scene/), { target: { value: '/proj/a.blend' } })
    fireEvent.click(screen.getByRole('button', { name: /submit/i }))
    await waitFor(() => expect(submitMock).toHaveBeenCalled())
    const arg = submitMock.mock.calls[0]?.[0] as Record<string, unknown>
    const parameters = arg.parameters as Record<string, string>
    expect(parameters['Scene']).toBe('/proj/a.blend')
    expect(parameters['InternalPath']).toBeUndefined()
  })

  it('re-seeds an untouched field when the product parameters change', async () => {
    h.state.params = [h.sceneParam('/old/scene.blend')]
    const qc = new QueryClient()
    const { rerender } = render(tree(qc))
    expect(await screen.findByLabelText(/Scene/)).toHaveValue('/old/scene.blend')

    // Simulate the product being edited: its parameter default changes.
    h.state.params = [h.sceneParam('/new/scene.blend')]
    rerender(tree(qc))
    await waitFor(() => expect(screen.getByLabelText(/Scene/)).toHaveValue('/new/scene.blend'))
  })

  it('preserves a field the user has edited when parameters change', async () => {
    h.state.params = [h.sceneParam('/old/scene.blend')]
    const qc = new QueryClient()
    const { rerender } = render(tree(qc))
    const input = await screen.findByLabelText(/Scene/)
    fireEvent.change(input, { target: { value: '/user/typed.blend' } })

    h.state.params = [h.sceneParam('/new/scene.blend')]
    rerender(tree(qc))
    await waitFor(() => expect(screen.getByLabelText(/Scene/)).toHaveValue('/user/typed.blend'))
  })

  it('submits retry overrides from the Advanced section', async () => {
    renderPage()
    const summary = screen.getByText(/advanced \(job overrides\)/i)
    const details = summary.closest('details') as HTMLDetailsElement
    expect(details).not.toBeNull()
    expect(details.open).toBe(false) // collapsed by default

    fireEvent.change(screen.getByLabelText(/Scene/), { target: { value: '/proj/a.blend' } })
    fireEvent.click(summary) // expand (cosmetic in jsdom; inputs are reachable regardless)
    fireEvent.change(screen.getByLabelText(/max attempts/i), { target: { value: '5' } })
    fireEvent.change(screen.getByLabelText(/failure limit/i), { target: { value: '3' } })
    fireEvent.click(screen.getByRole('button', { name: /submit/i }))

    await waitFor(() => expect(submitMock).toHaveBeenCalled())
    const arg = submitMock.mock.calls[0]?.[0] as Record<string, unknown>
    expect(arg).toMatchObject({ maxAttempts: 5, failureLimit: 3 })
    expect(arg).not.toHaveProperty('retryDelaySeconds') // left blank -> omitted
  })

  describe('Dependencies', () => {
    it('includes dependsOn in the submitted payload when an upstream job is selected', async () => {
      const user = userEvent.setup()
      renderPage()
      const summary = screen.getByText(/advanced \(job overrides\)/i)
      fireEvent.click(summary.closest('details') as HTMLDetailsElement)

      fireEvent.change(screen.getByLabelText(/Scene/), { target: { value: '/proj/a.blend' } })
      await user.click(screen.getByRole('checkbox', { name: /Upstream A/ }))
      fireEvent.click(screen.getByRole('button', { name: /submit/i }))

      await waitFor(() => expect(submitMock).toHaveBeenCalled())
      const arg = submitMock.mock.calls[0]?.[0] as Record<string, unknown>
      expect(arg).toMatchObject({ dependsOn: ['job-a'] })
    })

    it('omits dependsOn when no upstream job is selected', async () => {
      renderPage()
      fireEvent.change(screen.getByLabelText(/Scene/), { target: { value: '/proj/a.blend' } })
      fireEvent.click(screen.getByRole('button', { name: /submit/i }))

      await waitFor(() => expect(submitMock).toHaveBeenCalled())
      const arg = submitMock.mock.calls[0]?.[0] as Record<string, unknown>
      expect(arg).not.toHaveProperty('dependsOn')
    })
  })
})

describe('owner field permission gating', () => {
  it('hides the Owner input without jobs.submit_as', () => {
    renderWithAuth(<ProductSubmit />, { roles: ['user'] })
    expect(screen.queryByLabelText('Owner')).not.toBeInTheDocument()
  })

  it('shows the Owner input with jobs.submit_as', () => {
    renderWithAuth(<ProductSubmit />, { roles: ['operator'] })
    expect(screen.getByLabelText('Owner')).toBeInTheDocument()
  })

  it('omits owner from the submitted payload without jobs.submit_as', async () => {
    renderWithAuth(<ProductSubmit />, { roles: ['user'] })
    fireEvent.change(screen.getByLabelText(/Scene/), { target: { value: '/proj/a.blend' } })
    fireEvent.click(screen.getByRole('button', { name: /submit/i }))

    await waitFor(() => expect(submitMock).toHaveBeenCalled())
    const arg = submitMock.mock.calls[0]?.[0] as Record<string, unknown>
    expect(arg).not.toHaveProperty('owner')
    expect(arg).toMatchObject({ maySubmitAs: false })
  })
})
