// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import ProductList from './ProductList'
import type { Principal, Product } from '@/api/types'

// ── Auth mock ─────────────────────────────────────────────────────────────────
// ProductList reads useAuth() to gate mutating controls behind
// 'products.manage'. Mock the auth context directly (as JobList.test.tsx
// does) rather than driving a real AuthProvider through /auth/me, since
// fetchMock in this file is dedicated to the product list/mutation endpoints
// under test.

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(),
}))
import { useAuth } from '@/auth/context'

const OPERATOR_PRINCIPAL: Principal = {
  subject: 'u-operator',
  display_name: 'Operator',
  roles: ['operator'],
  kind: 'user',
}
const READONLY_PRINCIPAL: Principal = {
  subject: 'u-readonly',
  display_name: 'Read Only',
  roles: ['read-only'],
  kind: 'user',
}

/** Sets the principal returned by the mocked useAuth() for the next render. */
function setPrincipal(principal: Principal) {
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal,
    status: 'authed',
    refresh: () => {},
  })
}

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  // Default every test to an operator principal so pre-existing control
  // assertions keep working unchanged; the read-only gating test overrides
  // this via setPrincipal(READONLY_PRINCIPAL).
  setPrincipal(OPERATOR_PRINCIPAL)
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

function renderList(route = '/products') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[route]}>
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

  it('filters rows as you type and shows the filtered count in the subtitle', async () => {
    fetchMock.mockResolvedValueOnce(
      ok([
        makeProduct({ name: 'blender', title: 'Blender' }),
        makeProduct({ name: 'ffmpeg', title: 'FFmpeg' }),
      ]),
    )
    renderList()
    await screen.findByRole('link', { name: 'blender' })
    expect(screen.getByText('2 products')).toBeInTheDocument()

    await userEvent.type(screen.getByRole('searchbox'), 'blender')
    await waitFor(() =>
      expect(screen.queryByRole('link', { name: 'ffmpeg' })).not.toBeInTheDocument(),
    )
    expect(screen.getByRole('link', { name: 'blender' })).toBeInTheDocument()
    expect(screen.getByText('1 of 2 products')).toBeInTheDocument()
  })

  it('matches on category from an initial ?search=', async () => {
    fetchMock.mockResolvedValueOnce(
      ok([
        makeProduct({ name: 'a', category: 'Rendering' }),
        makeProduct({ name: 'b', category: 'Simulation' }),
      ]),
    )
    renderList('/products?search=rendering')
    expect(await screen.findByRole('link', { name: 'a' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'b' })).not.toBeInTheDocument()
  })

  it('shows a no-match row distinct from the onboarding empty state', async () => {
    fetchMock.mockResolvedValueOnce(ok([makeProduct()]))
    renderList('/products?search=zzz')
    expect(await screen.findByText(/No products match/)).toBeInTheDocument()
    expect(screen.queryByText(/No products yet/)).not.toBeInTheDocument()
  })

  describe('role gating (products.manage)', () => {
    it('hides New Product and Delete controls for a read-only principal', async () => {
      setPrincipal(READONLY_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(ok([makeProduct({ name: 'my-render' })]))
      renderList()

      await screen.findByRole('link', { name: 'my-render' })
      expect(screen.queryByRole('link', { name: /new product/i })).not.toBeInTheDocument()
      expect(
        screen.queryByRole('button', { name: /delete product my-render/i }),
      ).not.toBeInTheDocument()
    })

    it('shows New Product and Delete controls for an operator principal', async () => {
      setPrincipal(OPERATOR_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(ok([makeProduct({ name: 'my-render' })]))
      renderList()

      expect(await screen.findByRole('link', { name: /new product/i })).toBeInTheDocument()
      expect(
        await screen.findByRole('button', { name: /delete product my-render/i }),
      ).toBeInTheDocument()
    })
  })
})
