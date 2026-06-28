// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route, useLocation } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import ProductDetail from './ProductDetail'
import type { Product } from '@/api/types'

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

function makeProduct(over: Partial<Product> = {}): Product {
  return {
    name: 'my-render',
    title: 'My Render',
    description: 'desc',
    category: 'Rendering',
    version: '1.0.0',
    source: 'custom',
    template: 'name: my-template',
    format: 'yaml',
    ...over,
  }
}

function LocationDisplay() {
  const loc = useLocation()
  const state = loc.state as { duplicateFrom?: { title?: string; template?: string } } | null
  return (
    <div>
      <div data-testid="location">{loc.pathname}</div>
      <div data-testid="dup-title">{state?.duplicateFrom?.title ?? ''}</div>
      <div data-testid="dup-template">{state?.duplicateFrom?.template ?? ''}</div>
    </div>
  )
}

function renderDetail(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <ToastProvider>
          <Routes>
            <Route path="/products/:name" element={<ProductDetail />} />
            <Route path="/products/:name/edit" element={<LocationDisplay />} />
            <Route path="/products/new" element={<LocationDisplay />} />
            <Route path="/products" element={<LocationDisplay />} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ProductDetail', () => {
  it('renders metadata and the template', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct()))
    renderDetail('/products/my-render')
    // PageHeader applies invertCase to titles: "My Render" → "mY rENDER"
    expect(await screen.findByText('mY rENDER')).toBeInTheDocument()
    expect(screen.getByText('name: my-template')).toBeInTheDocument()
  })

  it('shows Duplicate (and no Edit/Delete) for a built-in', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct({ name: 'script', source: 'builtin' })))
    renderDetail('/products/script')
    expect(await screen.findByRole('button', { name: /duplicate to custom/i })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /edit/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /^delete$/i })).not.toBeInTheDocument()
  })

  it('duplicating a built-in navigates to the create form', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct({ name: 'script', source: 'builtin' })))
    renderDetail('/products/script')
    await userEvent.click(await screen.findByRole('button', { name: /duplicate to custom/i }))
    expect(screen.getByTestId('location')).toHaveTextContent('/products/new')
    // The built-in's title + template are carried as duplicate router state so the
    // create form pre-fills (name is intentionally blank, forcing a fresh slug).
    expect(screen.getByTestId('dup-title')).toHaveTextContent('My Render')
    expect(screen.getByTestId('dup-template')).toHaveTextContent('name: my-template')
  })

  it('shows Edit and Delete for a custom product and deletes after confirm', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(ok(makeProduct()))
    renderDetail('/products/my-render')
    await screen.findByText('name: my-template') // wait for product to load

    expect(screen.getByRole('link', { name: /edit/i })).toBeInTheDocument()
    fetchMock.mockResolvedValueOnce(ok(null, 204)) // DELETE
    await userEvent.click(screen.getByRole('button', { name: /^delete$/i }))

    await waitFor(() => {
      const del = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(del?.[0]).toBe('/api/v1/products/my-render')
    })
    await waitFor(() => expect(screen.getByTestId('location')).toHaveTextContent('/products'))
  })

  it('round-trips a name with a slash segment', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct({ name: 'studio/maya' })))
    renderDetail('/products/studio%2Fmaya')
    await screen.findByText('name: my-template') // wait for product to load
    expect(fetchMock.mock.calls[0]?.[0]).toBe('/api/v1/products/studio%2Fmaya')
  })
})
