// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import { adminPrincipal, mockAuth } from '@/test/principals'
import UserApiKeys from './UserApiKeys'

vi.mock('@/auth/context', () => ({ useAuth: vi.fn() }))

const KEY = {
  id: 'k1',
  name: 'ci-key',
  prefix: 'sqi_abc12345',
  created_at: '2026-07-01T00:00:00Z',
}

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  mockAuth(adminPrincipal())
})

afterEach(() => {
  vi.restoreAllMocks()
})

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** Serves the key list for GET and 204 for everything else (i.e. DELETE). */
function mockKeys(keys: unknown[]) {
  fetchMock.mockImplementation((_input, init) => {
    if ((init as RequestInit | undefined)?.method === 'DELETE') {
      return Promise.resolve(new Response(null, { status: 204 }))
    }
    return Promise.resolve(jsonResponse(keys))
  })
}

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/users/u-alice/api-keys']}>
        <ToastProvider>
          <Routes>
            <Route path="/users/:id/api-keys" element={<UserApiKeys />} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('UserApiKeys', () => {
  it("lists the target user's keys", async () => {
    mockKeys([KEY])
    renderPage()

    expect(await screen.findByText('ci-key')).toBeInTheDocument()
    expect(screen.getByText('sqi_abc12345')).toBeInTheDocument()
  })

  it('requests the nested per-user path, not the self-scoped one', async () => {
    mockKeys([KEY])
    renderPage()

    await screen.findByText('ci-key')
    expect(String(fetchMock.mock.calls[0]?.[0])).toContain('/users/u-alice/api-keys')
  })

  it('revokes a key', async () => {
    mockKeys([KEY])
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ci-key')
    await user.click(screen.getByRole('button', { name: /revoke api key ci-key/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(call).toBeDefined()
      expect(String(call?.[0])).toContain('/users/u-alice/api-keys/k1')
    })
  })

  it('does not revoke when the confirm is dismissed', async () => {
    mockKeys([KEY])
    vi.spyOn(window, 'confirm').mockReturnValue(false)
    const user = userEvent.setup()
    renderPage()

    await screen.findByText('ci-key')
    await user.click(screen.getByRole('button', { name: /revoke api key ci-key/i }))

    expect(
      fetchMock.mock.calls.find((c) => (c[1] as RequestInit | undefined)?.method === 'DELETE'),
    ).toBeUndefined()
  })

  // Admins list and revoke only — minting a credential another person is
  // accountable for is a different act, and the server offers no route for it.
  it('offers no way to create a key for another user', async () => {
    mockKeys([KEY])
    renderPage()

    await screen.findByText('ci-key')
    expect(screen.queryByRole('button', { name: /new key|create/i })).not.toBeInTheDocument()
  })

  it('renders an empty state when the user has no keys', async () => {
    mockKeys([])
    renderPage()

    expect(await screen.findByText(/no api keys/i)).toBeInTheDocument()
  })
})
