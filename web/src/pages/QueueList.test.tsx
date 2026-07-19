// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import QueueList from './QueueList'
import type { Principal } from '@/api/types'

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

/** A farm fixture used in several tests. */
const farm = {
  id: 'farm-1',
  name: 'render',
  max_concurrent_tasks: 0,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

/** A queue fixture used in several tests. */
const queue = {
  id: 'queue-1',
  farm_id: 'farm-1',
  name: 'lighting',
  priority: 50,
  max_concurrent_tasks: 0,
  paused: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
}

function mockFarmsAndQueues(queues = [queue]) {
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
        <ToastProvider>
          <QueueList />
        </ToastProvider>
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

  it('does not call DELETE when the confirm dialog is cancelled', async () => {
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
})
