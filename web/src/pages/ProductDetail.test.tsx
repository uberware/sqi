// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route, useLocation } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import ProductDetail from './ProductDetail'
import type { Principal, Product } from '@/api/types'

// ── Auth mock ─────────────────────────────────────────────────────────────────
// ProductDetail reads useAuth() to gate mutating controls behind
// 'products.manage'. Mock the auth context directly (as JobList.test.tsx
// does) rather than driving a real AuthProvider through /auth/me, since
// fetchMock in this file is dedicated to the product detail/mutation
// endpoints under test.

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(),
}))
import { useAuth } from '@/auth/context'
import { OPERATOR_PERMISSIONS, READ_ONLY_PERMISSIONS } from '@/test/principals'

const OPERATOR_PRINCIPAL: Principal = {
  subject: 'u-operator',
  display_name: 'Operator',
  roles: ['operator'],
  kind: 'user',
  permissions: OPERATOR_PERMISSIONS,
}
const READONLY_PRINCIPAL: Principal = {
  subject: 'u-readonly',
  display_name: 'Read Only',
  roles: ['read-only'],
  kind: 'user',
  permissions: READ_ONLY_PERMISSIONS,
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
  // assertions keep working unchanged; the read-only gating tests override
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
    description: 'desc',
    readme: '',
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
    // Block-font header reads "Product Details"; the product name renders plain
    // (sans-serif, not invertCase) as a level-2 heading below it.
    expect(await screen.findByRole('heading', { level: 2, name: 'My Render' })).toBeInTheDocument()
    expect(screen.getByText('name: my-template')).toBeInTheDocument()
  })

  it('links back to the products list', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct()))
    renderDetail('/products/my-render')
    const back = await screen.findByRole('link', { name: /← products/i })
    expect(back).toHaveAttribute('href', '/products')
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

  it('shows Duplicate to custom on a custom product (alongside Edit and Delete)', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct()))
    renderDetail('/products/my-render')
    await screen.findByText('name: my-template')
    expect(screen.getByRole('button', { name: /duplicate to custom/i })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /edit/i })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^delete$/i })).toBeInTheDocument()
  })

  it('installed product is read-only but duplicable and shows Uninstall', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(ok(makeProduct({ source: 'installed' })))
    renderDetail('/products/my-render')
    await screen.findByText('name: my-template')

    expect(screen.getByRole('button', { name: /duplicate to custom/i })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /edit/i })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /uninstall/i })).toBeInTheDocument()

    fetchMock.mockResolvedValueOnce(ok(null, 204)) // DELETE
    await userEvent.click(screen.getByRole('button', { name: /uninstall/i }))

    await waitFor(() => {
      const del = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(del?.[0]).toBe('/api/v1/products/my-render')
    })
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

  it('renders the readme as markdown', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct({ readme: '# Usage\n\nRun it with **care**.' })))
    renderDetail('/products/my-render')
    expect(await screen.findByText('Usage')).toBeInTheDocument()
    expect(screen.getByText('Usage').tagName).toBe('H3')
    expect(screen.getByText('care').tagName).toBe('STRONG')
  })

  it('renders nothing for an absent readme', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct({ readme: '' })))
    renderDetail('/products/my-render')
    await screen.findByText('My Render')
    expect(screen.queryByRole('heading', { level: 3 })).not.toBeInTheDocument()
  })

  describe('role gating (products.manage)', () => {
    it('hides Duplicate, Edit, and Delete controls for a read-only principal', async () => {
      setPrincipal(READONLY_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(ok(makeProduct()))
      renderDetail('/products/my-render')

      await screen.findByText('name: my-template')
      expect(screen.queryByRole('button', { name: /duplicate to custom/i })).not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: /edit/i })).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: /^delete$/i })).not.toBeInTheDocument()
    })

    it('shows Duplicate, Edit, and Delete controls for an operator principal', async () => {
      setPrincipal(OPERATOR_PRINCIPAL)
      fetchMock.mockResolvedValueOnce(ok(makeProduct()))
      renderDetail('/products/my-render')

      await screen.findByText('name: my-template')
      expect(
        await screen.findByRole('button', { name: /duplicate to custom/i }),
      ).toBeInTheDocument()
      expect(screen.getByRole('link', { name: /edit/i })).toBeInTheDocument()
      expect(screen.getByRole('button', { name: /^delete$/i })).toBeInTheDocument()
    })
  })
})
