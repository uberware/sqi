// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AuthProvider, useAuth } from './context'
import { queryClient as singletonQueryClient } from '@/api/queryClient'
import { ApiError } from '@/api/client'

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function jsonResponse(status: number, body: unknown, contentType: string): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': contentType } })
}

function Probe() {
  const { status, principal } = useAuth()
  return <div>{status === 'authed' ? `hi ${principal?.display_name}` : status}</div>
}

function renderWithAuth(client?: QueryClient) {
  const qc = client ?? new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={qc}>
      <AuthProvider>
        <Probe />
      </AuthProvider>
    </QueryClientProvider>,
  )
}

describe('AuthProvider', () => {
  it('resolves to authed when /auth/me returns a principal', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        200,
        { subject: 'u1', display_name: 'Alice', roles: ['operator'], kind: 'user' },
        'application/json',
      ),
    )
    renderWithAuth()
    expect(await screen.findByText('hi Alice')).toBeInTheDocument()
  })

  it('resolves to anon when /auth/me returns 401', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        401,
        { status: 401, detail: 'authentication required' },
        'application/problem+json',
      ),
    )
    renderWithAuth()
    await waitFor(() => expect(screen.getByText('anon')).toBeInTheDocument())
  })

  it('resolves to authed when /auth/me returns the anonymous principal (auth disabled)', async () => {
    // The auth-off regression: the anonymous authenticator returns 200 with
    // kind: 'anonymous', not a 401 — the web must treat this exactly like any
    // other authed principal so the app shell renders and no login appears.
    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        200,
        { subject: 'anonymous', display_name: 'Anonymous', roles: [], kind: 'anonymous' },
        'application/json',
      ),
    )
    renderWithAuth()
    expect(await screen.findByText('hi Anonymous')).toBeInTheDocument()
  })

  it('exposes a refresh() that re-fetches /auth/me', async () => {
    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        401,
        { status: 401, detail: 'authentication required' },
        'application/problem+json',
      ),
    )
    function ProbeWithRefreshButton() {
      const { status, principal, refresh } = useAuth()
      return (
        <div>
          <span>{status === 'authed' ? `hi ${principal?.display_name}` : status}</span>
          <button onClick={refresh}>refresh</button>
        </div>
      )
    }
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={qc}>
        <AuthProvider>
          <ProbeWithRefreshButton />
        </AuthProvider>
      </QueryClientProvider>,
    )
    await waitFor(() => expect(screen.getByText('anon')).toBeInTheDocument())

    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        200,
        { subject: 'u1', display_name: 'Bob', roles: [], kind: 'user' },
        'application/json',
      ),
    )
    fireEvent.click(screen.getByRole('button', { name: 'refresh' }))
    expect(await screen.findByText('hi Bob')).toBeInTheDocument()
  })

  it('a later 401 from any other query (via the shared queryClient) flips status back to anon', async () => {
    // Uses the real app singleton (not a fresh QueryClient) because the
    // global 401 -> auth.me invalidation is wired on that instance in
    // queryClient.ts, not on QueryClient in general.
    singletonQueryClient.clear()

    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        200,
        { subject: 'u1', display_name: 'Alice', roles: ['operator'], kind: 'user' },
        'application/json',
      ),
    )
    renderWithAuth(singletonQueryClient)
    expect(await screen.findByText('hi Alice')).toBeInTheDocument()

    // Simulate some unrelated query on the shared client failing with a 401 —
    // this should route through the QueryCache onError handler and invalidate
    // auth.me, which triggers a refetch below.
    const unauthorized = new ApiError({
      type: 'about:blank',
      title: 'Unauthorized',
      status: 401,
      detail: 'authentication required',
    })
    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        401,
        { status: 401, detail: 'authentication required' },
        'application/problem+json',
      ),
    )
    await expect(
      singletonQueryClient.fetchQuery({
        queryKey: ['workers', 'list', 'probe-401'],
        queryFn: () => {
          throw unauthorized
        },
        retry: false,
      }),
    ).rejects.toThrow()

    await waitFor(() => expect(screen.getByText('anon')).toBeInTheDocument())

    singletonQueryClient.clear()
  })
})
