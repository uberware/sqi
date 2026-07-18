// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import ComputeLocationList from './ComputeLocationList'
import type { Principal } from '@/api/types'

// ── Auth mock ─────────────────────────────────────────────────────────────────
// ComputeLocationList reads useAuth() to gate mutating controls behind
// 'infra.manage'. Mock the auth context directly (as JobList.test.tsx does)
// rather than driving a real AuthProvider through /auth/me, since fetchMock
// in this file is dedicated to the compute-location list/mutation endpoints
// under test.

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(),
}))
import { useAuth } from '@/auth/context'

const OPERATOR_PRINCIPAL: Principal = {
  subject: 'u-operator',
  display_name: 'Operator',
  roles: ['operator'],
  kind: 'user',
  permissions: [],
}
const READONLY_PRINCIPAL: Principal = {
  subject: 'u-readonly',
  display_name: 'Read Only',
  roles: ['read-only'],
  kind: 'user',
  permissions: [],
}

/** Sets the principal returned by the mocked useAuth() for the next render. */
function setPrincipal(principal: Principal) {
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal,
    status: 'authed',
    refresh: () => {},
  })
}

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  // Default every test to an operator principal so pre-existing control
  // assertions keep working unchanged; the read-only gating test overrides
  // this via setPrincipal(READONLY_PRINCIPAL).
  setPrincipal(OPERATOR_PRINCIPAL)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function computeLoc(overrides: Record<string, unknown> = {}) {
  return {
    id: 'cl-1',
    name: 'on-prem',
    worker_count: 3,
    created_at: '2026-06-28T00:00:00Z',
    updated_at: '2026-06-28T00:00:00Z',
    ...overrides,
  }
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ToastProvider>{(<ComputeLocationList />) as ReactNode}</ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ComputeLocationList', () => {
  it('renders compute locations from the API', async () => {
    fetchMock.mockResolvedValueOnce(ok([computeLoc()]))
    renderPage()
    expect(await screen.findByRole('link', { name: 'on-prem' })).toBeInTheDocument()
  })

  it('shows the worker_count', async () => {
    fetchMock.mockResolvedValueOnce(ok([computeLoc()]))
    renderPage()
    expect(await screen.findByText('3')).toBeInTheDocument()
  })

  it('shows an educational empty state', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    await waitFor(() => expect(screen.getByText(/no compute locations/i)).toBeInTheDocument())
  })

  it('has a New action linking to /compute-locations/new', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    const link = await screen.findByRole('link', { name: /new compute location/i })
    expect(link.getAttribute('href')).toBe('/compute-locations/new')
  })

  it('warns when deleting a location with workers', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    fetchMock.mockResolvedValueOnce(ok([computeLoc({ worker_count: 3 })]))
    renderPage()
    await screen.findByRole('button', { name: /delete compute location on-prem/i })
    fireEvent.click(screen.getByRole('button', { name: /delete compute location on-prem/i }))
    expect(confirmSpy).toHaveBeenCalledWith(expect.stringMatching(/3 online worker/i))
  })

  it('calls DELETE and shows success toast when confirmed', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(ok([computeLoc({ worker_count: 0 })]))
    renderPage()
    await screen.findByRole('button', { name: /delete compute location on-prem/i })
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    fireEvent.click(screen.getByRole('button', { name: /delete compute location on-prem/i }))
    await waitFor(() => {
      const del = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(del).toBeDefined()
    })
    await screen.findByText(/compute location "on-prem" deleted/i)
  })

  describe('role gating (infra.manage)', () => {
    it('hides New and Delete controls for a read-only principal', async () => {
      setPrincipal(READONLY_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(ok([computeLoc()]))
      renderPage()

      await screen.findByRole('link', { name: 'on-prem' })
      expect(screen.queryByRole('link', { name: /new compute location/i })).not.toBeInTheDocument()
      expect(
        screen.queryByRole('button', { name: /delete compute location on-prem/i }),
      ).not.toBeInTheDocument()
    })

    it('shows New and Delete controls for an operator principal', async () => {
      setPrincipal(OPERATOR_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(ok([computeLoc()]))
      renderPage()

      expect(await screen.findByRole('link', { name: /new compute location/i })).toBeInTheDocument()
      expect(
        await screen.findByRole('button', { name: /delete compute location on-prem/i }),
      ).toBeInTheDocument()
    })
  })
})
