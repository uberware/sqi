// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import StorageLocationForm from './StorageLocationForm'

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
      <MemoryRouter initialEntries={['/storage-locations/new']}>
        <ToastProvider>
          {
            (
              <Routes>
                <Route
                  path="/storage-locations/new"
                  element={<StorageLocationForm mode="create" />}
                />
                <Route path="/storage-locations" element={<div>list page</div>} />
              </Routes>
            ) as ReactNode
          }
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('StorageLocationForm (create)', () => {
  it('seeds a default root row', () => {
    renderCreate()
    expect(screen.getByDisplayValue('default')).toBeInTheDocument()
  })

  it('disables submit until name and a root path are provided', () => {
    renderCreate()
    const submit = screen.getByRole('button', { name: /create storage location/i })
    expect(submit).toBeDisabled()
  })

  it('reveals name help text when the name field is focused', () => {
    renderCreate()
    expect(screen.queryByText(/letters, numbers/i)).not.toBeInTheDocument()
    fireEvent.focus(screen.getByLabelText(/name/i))
    expect(screen.getByText(/letters, numbers/i)).toBeInTheDocument()
  })

  it('rejects an invalid name with an error and disables submit', () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'nas shows' } })
    const pathInputs = screen.getAllByLabelText(/root path/i)
    fireEvent.change(pathInputs[0] as HTMLElement, { target: { value: '/mnt/a' } })
    expect(screen.getByText(/must not contain/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /create storage location/i })).toBeDisabled()
  })

  it('blocks save on duplicate root keys', async () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'nas_shows' } })
    const pathInputs = screen.getAllByLabelText(/root path/i)
    fireEvent.change(pathInputs[0] as HTMLElement, { target: { value: '/mnt/a' } })
    fireEvent.click(screen.getByRole('button', { name: /add root/i }))
    const keyInputs = screen.getAllByLabelText(/location key/i)
    fireEvent.change(keyInputs[1] as HTMLElement, { target: { value: 'default' } })
    const valInputs = screen.getAllByLabelText(/root path/i)
    fireEvent.change(valInputs[1] as HTMLElement, { target: { value: '/mnt/b' } })
    expect(await screen.findByText(/duplicate/i)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /create storage location/i })).toBeDisabled()
  })

  it('POSTs the serialized roots map and navigates on success', async () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'nas_shows' } })
    const pathInputs = screen.getAllByLabelText(/root path/i)
    fireEvent.change(pathInputs[0] as HTMLElement, { target: { value: '/mnt/nas/shows' } })
    fetchMock.mockResolvedValueOnce(ok({ id: 'loc-1', name: 'nas_shows' }))
    fireEvent.click(screen.getByRole('button', { name: /create storage location/i }))
    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      expect(post).toBeDefined()
      const [, init] = post as [string, RequestInit]
      const body = JSON.parse(init.body as string)
      expect(body.roots.default).toBe('/mnt/nas/shows')
      expect(body.type).toBeUndefined()
    })
    await screen.findByText('list page')
  })

  it('omits roots whose path is blank from the POST body', async () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/name/i), { target: { value: 'nas_shows' } })
    // Seeded default row gets a path:
    const pathInputs = screen.getAllByLabelText(/root path/i)
    fireEvent.change(pathInputs[0] as HTMLElement, { target: { value: '/mnt/nas/shows' } })
    // Add a second row with a key but NO path:
    fireEvent.click(screen.getByRole('button', { name: /add root/i }))
    const keyInputs = screen.getAllByLabelText(/location key/i)
    fireEvent.change(keyInputs[1] as HTMLElement, { target: { value: 'windows_workers' } })
    // leave its path blank
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ id: 'loc-1', name: 'nas_shows' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    fireEvent.click(screen.getByRole('button', { name: /create storage location/i }))
    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      expect(post).toBeDefined()
      const [, init] = post as [string, RequestInit]
      const body = JSON.parse(init.body as string)
      expect(body.roots).toEqual({ default: '/mnt/nas/shows' })
      expect(body.roots.windows_workers).toBeUndefined()
    })
  })
})
