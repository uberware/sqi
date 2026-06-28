// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import ComputeLocationForm from './ComputeLocationForm'

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function renderCreate() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/compute-locations/new']}>
        <ToastProvider>
          {
            (
              <Routes>
                <Route
                  path="/compute-locations/new"
                  element={<ComputeLocationForm mode="create" />}
                />
                <Route path="/compute-locations" element={<div>list page</div>} />
              </Routes>
            ) as ReactNode
          }
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ComputeLocationForm (create)', () => {
  it('disables submit when name is empty', () => {
    renderCreate()
    const submit = screen.getByRole('button', { name: /create compute location/i })
    expect(submit).toBeDisabled()
  })

  it('rejects an invalid name with an error and disables submit', () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/^name/i), { target: { value: 'on prem' } })
    expect(screen.getByText(/must not contain/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /create compute location/i })).toBeDisabled()
  })

  it('reveals name help text when the name field is focused', () => {
    renderCreate()
    expect(screen.queryByText(/letters, numbers/i)).not.toBeInTheDocument()
    fireEvent.focus(screen.getByLabelText(/^name/i))
    expect(screen.getByText(/letters, numbers/i)).toBeInTheDocument()
  })

  it('POSTs name and description on valid submit', async () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/^name/i), { target: { value: 'on-prem' } })
    fireEvent.change(screen.getByLabelText(/description/i), {
      target: { value: 'On-premise rack' },
    })
    fetchMock.mockResolvedValueOnce(
      ok({
        id: 'cl-1',
        name: 'on-prem',
        worker_count: 0,
        created_at: '2026-06-28T00:00:00Z',
        updated_at: '2026-06-28T00:00:00Z',
      }),
    )
    fireEvent.click(screen.getByRole('button', { name: /create compute location/i }))
    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      expect(post).toBeDefined()
      const [, init] = post as [string, RequestInit]
      const body = JSON.parse(init.body as string) as Record<string, unknown>
      expect(body['name']).toBe('on-prem')
      expect(body['description']).toBe('On-premise rack')
    })
    await screen.findByText('list page')
  })

  it('omits description from POST body when blank', async () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/^name/i), { target: { value: 'on-prem' } })
    fetchMock.mockResolvedValueOnce(
      ok({
        id: 'cl-1',
        name: 'on-prem',
        worker_count: 0,
        created_at: '2026-06-28T00:00:00Z',
        updated_at: '2026-06-28T00:00:00Z',
      }),
    )
    fireEvent.click(screen.getByRole('button', { name: /create compute location/i }))
    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      expect(post).toBeDefined()
      const [, init] = post as [string, RequestInit]
      const body = JSON.parse(init.body as string) as Record<string, unknown>
      expect(body['name']).toBe('on-prem')
      expect(body['description']).toBeUndefined()
    })
  })
})
