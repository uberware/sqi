// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import ApiKeyList from './ApiKeyList'

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown, status = 200): Response {
  return new Response(body === undefined ? null : JSON.stringify(body), {
    status,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
  })
}

function apiKey(overrides: Record<string, unknown> = {}) {
  return {
    id: 'k1',
    name: 'ci-runner',
    prefix: 'sk_ab12',
    created_at: '2026-06-28T00:00:00Z',
    ...overrides,
  }
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ToastProvider>{(<ApiKeyList />) as ReactNode}</ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ApiKeyList', () => {
  it('renders an API key row (name + prefix) from the API', async () => {
    fetchMock.mockResolvedValueOnce(ok([apiKey()]))
    renderPage()
    expect(await screen.findByText('ci-runner')).toBeInTheDocument()
    expect(screen.getByText('sk_ab12')).toBeInTheDocument()
  })

  it('shows an educational empty state', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    await waitFor(() => expect(screen.getByText(/no api keys/i)).toBeInTheDocument())
  })

  it('never exposes a secret for keys returned by the list endpoint', async () => {
    // Defense in depth: even if the list endpoint were to leak a `secret`
    // field, the page must never render it for existing rows (only the
    // create response's secret is ever shown, and only once).
    fetchMock.mockResolvedValueOnce(ok([apiKey({ secret: 'sk_live_leaked_value' })]))
    renderPage()
    await screen.findByText('ci-runner')
    expect(screen.queryByText('sk_live_leaked_value')).not.toBeInTheDocument()
  })

  it('has a New key control that reveals a create form', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    const toggle = await screen.findByRole('button', { name: /new key/i })
    expect(screen.queryByLabelText(/^name$/i)).not.toBeInTheDocument()
    fireEvent.click(toggle)
    expect(screen.getByLabelText(/^name$/i)).toBeInTheDocument()
  })

  it('creates a key and shows the returned secret once in a copy callout', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()

    const toggle = await screen.findByRole('button', { name: /new key/i })
    fireEvent.click(toggle)

    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: 'ci-runner' } })

    fetchMock.mockResolvedValueOnce(ok(apiKey({ secret: 'sk_live_abcdef1234567890' }), 201))
    fetchMock.mockResolvedValueOnce(ok([apiKey()])) // refetch after invalidation

    fireEvent.click(screen.getByRole('button', { name: /create key/i }))

    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      expect(post).toBeDefined()
    })

    const secretMatches = await screen.findAllByText('sk_live_abcdef1234567890')
    expect(secretMatches).toHaveLength(1)
    expect(screen.getByText(/won.t be able to see it again/i)).toBeInTheDocument()
  })

  it('clears the secret from state when the callout is dismissed', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()

    fireEvent.click(await screen.findByRole('button', { name: /new key/i }))
    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: 'ci-runner' } })

    fetchMock.mockResolvedValueOnce(ok(apiKey({ secret: 'sk_live_once_only' }), 201))
    fetchMock.mockResolvedValueOnce(ok([apiKey()]))

    fireEvent.click(screen.getByRole('button', { name: /create key/i }))
    await screen.findByText('sk_live_once_only')

    fireEvent.click(screen.getByRole('button', { name: /done/i }))
    expect(screen.queryByText('sk_live_once_only')).not.toBeInTheDocument()
  })

  it('confirms before revoking', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    fetchMock.mockResolvedValueOnce(ok([apiKey()]))
    renderPage()
    await screen.findByRole('button', { name: /revoke api key ci-runner/i })
    fireEvent.click(screen.getByRole('button', { name: /revoke api key ci-runner/i }))
    expect(confirmSpy).toHaveBeenCalled()

    // Declining the confirm must short-circuit before any DELETE is fired.
    const del = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
    )
    expect(del).toBeUndefined()
  })

  it('calls DELETE and removes the row after refetch when revoke is confirmed', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(ok([apiKey()]))
    renderPage()
    await screen.findByRole('button', { name: /revoke api key ci-runner/i })

    fetchMock.mockResolvedValueOnce(ok(undefined, 204))
    fetchMock.mockResolvedValueOnce(ok([])) // refetch after invalidation

    fireEvent.click(screen.getByRole('button', { name: /revoke api key ci-runner/i }))

    await waitFor(() => {
      const del = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(del).toBeDefined()
    })

    await waitFor(() => expect(screen.queryByText('ci-runner')).not.toBeInTheDocument())
  })
})
