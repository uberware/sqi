// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import ProductList from './ProductList'
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
    description: '',
    category: 'Rendering',
    version: '1.0.0',
    source: 'custom',
    template: 'name: x',
    format: 'yaml',
    ...over,
  }
}

function renderList() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/products']}>
        <ToastProvider>
          <Routes>
            <Route path="/products" element={<ProductList />} />
            <Route path="/products/new" element={<div>new product</div>} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ProductList', () => {
  it('renders products with their source badges', async () => {
    fetchMock.mockResolvedValueOnce(
      ok([makeProduct({ name: 'script', source: 'builtin' }), makeProduct({ name: 'my-render' })]),
    )
    renderList()
    expect(await screen.findByRole('link', { name: 'script' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'my-render' })).toBeInTheDocument()
    expect(screen.getByText('builtin')).toBeInTheDocument()
    expect(screen.getByText('custom')).toBeInTheDocument()
  })

  it('shows an empty state when there are no products', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderList()
    expect(await screen.findByText(/no products yet/i)).toBeInTheDocument()
  })

  it('does not show a delete control for built-ins', async () => {
    fetchMock.mockResolvedValueOnce(ok([makeProduct({ name: 'script', source: 'builtin' })]))
    renderList()
    await screen.findByRole('link', { name: 'script' })
    expect(screen.queryByRole('button', { name: /delete product script/i })).not.toBeInTheDocument()
  })

  it('deletes a custom product after confirmation', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(ok([makeProduct({ name: 'my-render' })]))
    renderList()
    await screen.findByRole('link', { name: 'my-render' })

    fetchMock.mockResolvedValueOnce(ok(null, 204)) // DELETE
    fetchMock.mockResolvedValueOnce(ok([])) // refetch after invalidate
    await userEvent.click(screen.getByRole('button', { name: /delete product my-render/i }))

    await waitFor(() => {
      const del = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(del?.[0]).toBe('/api/v1/products/my-render')
    })
  })
})
