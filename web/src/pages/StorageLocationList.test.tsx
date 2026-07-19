// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import StorageLocationList from './StorageLocationList'
import type { Principal } from '@/api/types'

// ── Auth mock ─────────────────────────────────────────────────────────────────
// StorageLocationList reads useAuth() to gate mutating controls behind
// 'infra.manage'. Mock the auth context directly (as JobList.test.tsx does)
// rather than driving a real AuthProvider through /auth/me, since fetchMock
// in this file is dedicated to the storage-location list/mutation endpoints
// under test.

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

function loc(overrides: Record<string, unknown> = {}) {
  return {
    id: 'loc-1',
    name: 'nas_shows',
    type: 'filesystem',
    roots: { default: '/mnt/nas/shows', windows_workers: 'Z:\\shows' },
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ToastProvider>{(<StorageLocationList />) as ReactNode}</ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('StorageLocationList', () => {
  it('renders locations from the API', async () => {
    fetchMock.mockResolvedValueOnce(ok([loc()]))
    renderPage()
    expect(await screen.findByRole('link', { name: 'nas_shows' })).toBeInTheDocument()
  })

  it('shows the default root and root count', async () => {
    fetchMock.mockResolvedValueOnce(ok([loc()]))
    renderPage()
    expect(await screen.findByText('/mnt/nas/shows')).toBeInTheDocument()
    expect(await screen.findByText('2')).toBeInTheDocument()
  })

  it('shows an em dash when there is no default root', async () => {
    fetchMock.mockResolvedValueOnce(ok([loc({ roots: { windows_workers: 'Z:\\shows' } })]))
    renderPage()
    expect(await screen.findByText('—')).toBeInTheDocument()
  })

  it('shows an educational empty state', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    await waitFor(() => expect(screen.getByText(/no storage locations/i)).toBeInTheDocument())
  })

  it('has a New action linking to /storage-locations/new', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    const link = await screen.findByRole('link', { name: /new storage location/i })
    expect(link.getAttribute('href')).toBe('/storage-locations/new')
  })

  it('calls DELETE and shows success toast when confirmed', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(ok([loc()]))
    renderPage()
    await screen.findByRole('button', { name: /delete storage location nas_shows/i })
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    fireEvent.click(screen.getByRole('button', { name: /delete storage location nas_shows/i }))
    await waitFor(() => {
      const del = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(del).toBeDefined()
    })
    await screen.findByText(/storage location "nas_shows" deleted/i)
  })

  describe('role gating (infra.manage)', () => {
    it('hides New and Delete controls for a read-only principal', async () => {
      setPrincipal(READONLY_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(ok([loc()]))
      renderPage()

      await screen.findByRole('link', { name: 'nas_shows' })
      expect(screen.queryByRole('link', { name: /new storage location/i })).not.toBeInTheDocument()
      expect(
        screen.queryByRole('button', { name: /delete storage location nas_shows/i }),
      ).not.toBeInTheDocument()
    })

    it('shows New and Delete controls for an operator principal', async () => {
      setPrincipal(OPERATOR_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(ok([loc()]))
      renderPage()

      expect(await screen.findByRole('link', { name: /new storage location/i })).toBeInTheDocument()
      expect(
        await screen.findByRole('button', { name: /delete storage location nas_shows/i }),
      ).toBeInTheDocument()
    })
  })
})
