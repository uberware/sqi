// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, fireEvent, waitFor } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import UsagePoolForm from './UsagePoolForm'

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
            <Route path="/usage-pools/new" element={<UsagePoolForm mode="create" />} />
            <Route path="/usage-pools/:id/edit" element={<UsagePoolForm mode="edit" />} />
            <Route path="/usage-pools" element={<div>pool list</div>} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('UsagePoolForm (create)', () => {
  it('submits a POST with the entered values and navigates to /usage-pools', async () => {
    fetchMock.mockResolvedValueOnce(ok({ id: 'pool-1' }))
    renderAt('/usage-pools/new')

    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: 'arnold' } })
    fireEvent.change(screen.getByLabelText(/max concurrent/i), { target: { value: '8' } })
    fireEvent.click(screen.getByRole('button', { name: /create usage pool/i }))

    await waitFor(() => expect(screen.getByText('pool list')).toBeInTheDocument())
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toMatchObject({
      name: 'arnold',
      max_concurrent: 8,
    })
  })

  it('disables submit when name is empty', () => {
    renderAt('/usage-pools/new')
    expect(screen.getByRole('button', { name: /create usage pool/i })).toBeDisabled()
  })

  it('disables submit when max concurrent is below 1', () => {
    renderAt('/usage-pools/new')
    fireEvent.change(screen.getByLabelText(/^name$/i), { target: { value: 'arnold' } })
    fireEvent.change(screen.getByLabelText(/max concurrent/i), { target: { value: '0' } })
    expect(screen.getByRole('button', { name: /create usage pool/i })).toBeDisabled()
  })
})

describe('UsagePoolForm (edit)', () => {
  it('prefills from GET and PUTs on save', async () => {
    fetchMock.mockResolvedValueOnce(
      ok({
        id: 'pool-1',
        name: 'arnold',
        server_hint: '27000@licsrv',
        max_concurrent: 5,
        in_use: 0,
        available: 5,
        created_at: '2026-01-01T00:00:00Z',
        updated_at: '2026-01-01T00:00:00Z',
      }),
    )
    renderAt('/usage-pools/pool-1/edit')

    const nameInput = await screen.findByDisplayValue('arnold')
    fireEvent.change(nameInput, { target: { value: 'arnold2' } })

    fetchMock.mockResolvedValueOnce(ok({ id: 'pool-1' }))
    fireEvent.click(screen.getByRole('button', { name: /save/i }))

    await waitFor(() => expect(screen.getByText('pool list')).toBeInTheDocument())
    const putCall = fetchMock.mock.calls.find(
      (c) => (c[1] as RequestInit | undefined)?.method === 'PUT',
    )
    expect(putCall).toBeDefined()
  })
})
