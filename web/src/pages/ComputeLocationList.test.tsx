// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import ComputeLocationList from './ComputeLocationList'

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
})
