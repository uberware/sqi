// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import FarmList from './FarmList'
import type { Principal } from '@/api/types'

// ── Auth mock ─────────────────────────────────────────────────────────────────
// FarmList reads useAuth() to gate mutating controls behind 'infra.manage'. Mock
// the auth context directly (as JobList.test.tsx does) rather than driving a
// real AuthProvider through /auth/me, since fetchMock in this file is
// dedicated to the farm list/mutation endpoints under test.

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

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ToastProvider>{(<FarmList />) as ReactNode}</ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('FarmList', () => {
  it('renders farms from the API', async () => {
    fetchMock.mockResolvedValueOnce(
      ok([
        {
          id: 'farm-1',
          name: 'render',
          description: 'main',
          max_concurrent_tasks: 10,
          created_at: '2026-01-01T00:00:00Z',
          updated_at: '2026-01-01T00:00:00Z',
        },
      ]),
    )
    renderPage()
    expect(await screen.findByRole('link', { name: 'render' })).toBeInTheDocument()
  })

  it('shows an empty state when there are no farms', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    await waitFor(() => expect(screen.getByText(/no farms/i)).toBeInTheDocument())
  })

  it('has a New Farm action linking to /farms/new', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    const link = await screen.findByRole('link', { name: /new farm/i })
    expect(link.getAttribute('href')).toBe('/farms/new')
  })

  it('calls DELETE and shows success toast when delete is confirmed', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(
      ok([
        { id: 'farm-1', name: 'render', max_concurrent_tasks: 0, created_at: '', updated_at: '' },
      ]),
    )
    renderPage()

    await screen.findByRole('button', { name: /delete farm render/i })
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    fireEvent.click(screen.getByRole('button', { name: /delete farm render/i }))

    await waitFor(() => {
      const deleteCall = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(deleteCall).toBeDefined()
    })
    await screen.findByText(/farm "render" deleted/i)
  })

  describe('role gating (infra.manage)', () => {
    it('hides New Farm and Delete controls for a read-only principal', async () => {
      setPrincipal(READONLY_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(
        ok([
          { id: 'farm-1', name: 'render', max_concurrent_tasks: 0, created_at: '', updated_at: '' },
        ]),
      )
      renderPage()

      await screen.findByRole('link', { name: 'render' })
      expect(screen.queryByRole('link', { name: /new farm/i })).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: /delete farm render/i })).not.toBeInTheDocument()
    })

    it('shows New Farm and Delete controls for an operator principal', async () => {
      setPrincipal(OPERATOR_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(
        ok([
          { id: 'farm-1', name: 'render', max_concurrent_tasks: 0, created_at: '', updated_at: '' },
        ]),
      )
      renderPage()

      expect(await screen.findByRole('link', { name: /new farm/i })).toBeInTheDocument()
      expect(await screen.findByRole('button', { name: /delete farm render/i })).toBeInTheDocument()
    })
  })
})
