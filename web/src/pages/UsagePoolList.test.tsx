// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import UsagePoolList from './UsagePoolList'

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function pool(overrides: Record<string, unknown> = {}) {
  return {
    id: 'pool-1',
    name: 'arnold',
    server_hint: '27000@licsrv',
    max_concurrent: 5,
    in_use: 2,
    available: 3,
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
        <ToastProvider>{(<UsagePoolList />) as ReactNode}</ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('UsagePoolList', () => {
  it('renders pools from the API', async () => {
    fetchMock.mockResolvedValueOnce(ok([pool()]))
    renderPage()
    expect(await screen.findByRole('link', { name: 'arnold' })).toBeInTheDocument()
  })

  it('shows live utilization (in_use / max_concurrent)', async () => {
    fetchMock.mockResolvedValueOnce(ok([pool({ in_use: 2, max_concurrent: 5 })]))
    renderPage()
    expect(await screen.findByText('2 / 5')).toBeInTheDocument()
  })

  it('shows an empty state when there are no pools', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    await waitFor(() => expect(screen.getByText(/no usage pools/i)).toBeInTheDocument())
  })

  it('has a New Usage Pool action linking to /usage-pools/new', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    const link = await screen.findByRole('link', { name: /new usage pool/i })
    expect(link.getAttribute('href')).toBe('/usage-pools/new')
  })

  it('calls DELETE and shows success toast when delete is confirmed', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(ok([pool()]))
    renderPage()

    await screen.findByRole('button', { name: /delete usage pool arnold/i })
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    fireEvent.click(screen.getByRole('button', { name: /delete usage pool arnold/i }))

    await waitFor(() => {
      const deleteCall = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(deleteCall).toBeDefined()
    })
    await screen.findByText(/usage pool "arnold" deleted/i)
  })
})
