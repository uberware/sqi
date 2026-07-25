// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import QueueForm from './QueueForm'

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

const farms = [
  {
    id: 'farm-1',
    name: 'render',
    max_concurrent_tasks: 0,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
  },
]

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <ToastProvider>
          <Routes>
            <Route path="/queues/new" element={<QueueForm mode="create" />} />
            <Route path="/queues/:id/edit" element={<QueueForm mode="edit" />} />
            <Route path="/queues" element={<div>queue list</div>} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('QueueForm (create)', () => {
  it('POSTs a queue with the selected farm and entered name', async () => {
    fetchMock.mockResolvedValueOnce(ok(farms)) // GET /farms for the selector
    renderAt('/queues/new')

    await screen.findByRole('option', { name: 'render' })
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'lighting' } })

    fetchMock.mockResolvedValueOnce(ok({ id: 'queue-1' }))
    fireEvent.click(screen.getByRole('button', { name: /create queue/i }))

    await waitFor(() => expect(screen.getByText('queue list')).toBeInTheDocument())
    const postCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
    )
    if (!postCall) throw new Error('expected a POST call')
    expect(JSON.parse((postCall[1] as RequestInit).body as string)).toMatchObject({
      farm_id: 'farm-1',
      name: 'lighting',
    })
  })

  it('includes retry-policy overrides in the POST body when entered', async () => {
    fetchMock.mockResolvedValueOnce(ok(farms)) // GET /farms for the selector
    renderAt('/queues/new')

    await screen.findByRole('option', { name: 'render' })
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'lighting' } })
    fireEvent.change(screen.getByLabelText(/max attempts per task/i), { target: { value: '5' } })
    fireEvent.change(screen.getByLabelText(/retry delay/i), { target: { value: '30' } })
    fireEvent.change(screen.getByLabelText(/failure limit/i), { target: { value: '10' } })

    fetchMock.mockResolvedValueOnce(ok({ id: 'queue-1' }))
    fireEvent.click(screen.getByRole('button', { name: /create queue/i }))

    await waitFor(() => expect(screen.getByText('queue list')).toBeInTheDocument())
    const postCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
    )
    if (!postCall) throw new Error('expected a POST call')
    expect(JSON.parse((postCall[1] as RequestInit).body as string)).toMatchObject({
      max_attempts: 5,
      retry_delay_seconds: 30,
      failure_limit: 10,
    })
  })

  it('omits retry-policy fields from the POST body when left blank', async () => {
    fetchMock.mockResolvedValueOnce(ok(farms)) // GET /farms for the selector
    renderAt('/queues/new')

    await screen.findByRole('option', { name: 'render' })
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'lighting' } })

    fetchMock.mockResolvedValueOnce(ok({ id: 'queue-1' }))
    fireEvent.click(screen.getByRole('button', { name: /create queue/i }))

    await waitFor(() => expect(screen.getByText('queue list')).toBeInTheDocument())
    const postCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
    )
    if (!postCall) throw new Error('expected a POST call')
    const body: Record<string, unknown> = JSON.parse((postCall[1] as RequestInit).body as string)
    expect(body).not.toHaveProperty('max_attempts')
    expect(body).not.toHaveProperty('retry_delay_seconds')
    expect(body).not.toHaveProperty('failure_limit')
  })
})
