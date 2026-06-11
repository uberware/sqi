// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import FarmForm from './FarmForm'

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown, status = 200): Response {
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <ToastProvider>
          <Routes>
            <Route path="/farms/new" element={<FarmForm mode="create" />} />
            <Route path="/farms/:id/edit" element={<FarmForm mode="edit" />} />
            <Route path="/farms" element={<div>farm list</div>} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('FarmForm (create)', () => {
  it('submits a POST with the entered values and navigates to /farms', async () => {
    fetchMock.mockResolvedValueOnce(ok({ id: 'farm-1' }))
    renderAt('/farms/new')

    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'render' } })
    fireEvent.click(screen.getByRole('button', { name: /create farm/i }))

    await waitFor(() => expect(screen.getByText('farm list')).toBeInTheDocument())
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toMatchObject({ name: 'render' })
  })

  it('disables submit when name is empty', () => {
    renderAt('/farms/new')
    expect(screen.getByRole('button', { name: /create farm/i })).toBeDisabled()
  })
})

describe('FarmForm (edit)', () => {
  it('prefills from GET and PUTs on save', async () => {
    fetchMock.mockResolvedValueOnce(
      ok({
        id: 'farm-1',
        name: 'render',
        description: 'main',
        max_concurrent_tasks: 4,
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
      }),
    )
    renderAt('/farms/farm-1/edit')

    const nameInput = await screen.findByDisplayValue('render')
    fireEvent.change(nameInput, { target: { value: 'render2' } })

    fetchMock.mockResolvedValueOnce(ok({ id: 'farm-1' }))
    fireEvent.click(screen.getByRole('button', { name: /save/i }))

    await waitFor(() => expect(screen.getByText('farm list')).toBeInTheDocument())
    const putCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'PUT',
    )
    expect(putCall).toBeDefined()
  })
})
