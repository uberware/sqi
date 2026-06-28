// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route, useLocation } from 'react-router-dom'
import { ToastProvider } from '@/components/Toast'
import ProductForm from './ProductForm'
import type { ProductDuplicateState } from './ProductForm'
import type { Product } from '@/api/types'

// CodeMirror needs DOM APIs jsdom lacks; replace with a plain textarea.
vi.mock('@/components/CodeEditor', () => ({
  default: ({
    value,
    onChange,
    'aria-label': ariaLabel,
    'data-testid': testId,
  }: {
    value: string
    onChange: (v: string) => void
    'aria-label'?: string
    'data-testid'?: string
  }) => (
    <textarea
      value={value}
      onChange={(e) => onChange(e.target.value)}
      aria-label={ariaLabel}
      data-testid={testId}
    />
  ),
}))

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
function problem(status: number, detail: string): Response {
  return new Response(JSON.stringify({ type: 'about:blank', title: 'Error', status, detail }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
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
    template: 'name: x',
    format: 'yaml',
    ...over,
  }
}

function LocationDisplay() {
  const loc = useLocation()
  return <div data-testid="location">{loc.pathname}</div>
}

function renderForm(
  initialEntries: Array<string | { pathname: string; state: ProductDuplicateState }>,
) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={initialEntries}>
        <ToastProvider>
          <Routes>
            <Route path="/products/new" element={<ProductForm mode="create" />} />
            <Route path="/products/:name/edit" element={<ProductForm mode="edit" />} />
            <Route path="/products/:name" element={<LocationDisplay />} />
            <Route path="/products" element={<div>product list</div>} />
          </Routes>
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ProductForm (create)', () => {
  it('disables submit until name, title, and template are filled', async () => {
    renderForm(['/products/new'])
    const submit = screen.getByRole('button', { name: /create product/i })
    expect(submit).toBeDisabled()

    await userEvent.type(screen.getByLabelText(/^name$/i), 'my-render')
    await userEvent.type(screen.getByLabelText(/^title$/i), 'My Render')
    await userEvent.type(screen.getByTestId('template-editor'), 'name: x')
    expect(submit).toBeEnabled()
  })

  it('shows a slug error and blocks submit for an invalid name', async () => {
    renderForm(['/products/new'])
    await userEvent.type(screen.getByLabelText(/^name$/i), 'Bad Name')
    expect(screen.getByRole('alert')).toHaveTextContent(/lowercase slug/i)
    expect(screen.getByRole('button', { name: /create product/i })).toBeDisabled()
  })

  it('POSTs the product with auto-detected format and navigates to its detail', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct(), 201))
    renderForm(['/products/new'])

    await userEvent.type(screen.getByLabelText(/^name$/i), 'my-render')
    await userEvent.type(screen.getByLabelText(/^title$/i), 'My Render')
    await userEvent.type(screen.getByTestId('template-editor'), 'name: x')
    await userEvent.click(screen.getByRole('button', { name: /create product/i }))

    await waitFor(() =>
      expect(screen.getByTestId('location')).toHaveTextContent('/products/my-render'),
    )
    const [, init] = fetchMock.mock.calls[0] as [string, RequestInit]
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toMatchObject({
      name: 'my-render',
      title: 'My Render',
      template: 'name: x',
      format: 'yaml',
    })
  })

  it('surfaces a server validation error in the error block', async () => {
    fetchMock.mockResolvedValueOnce(problem(400, 'openjd: validate: step "main" has no script'))
    renderForm(['/products/new'])

    await userEvent.type(screen.getByLabelText(/^name$/i), 'my-render')
    await userEvent.type(screen.getByLabelText(/^title$/i), 'My Render')
    await userEvent.type(screen.getByTestId('template-editor'), 'name: x')
    await userEvent.click(screen.getByRole('button', { name: /create product/i }))

    expect(await screen.findByText(/step "main" has no script/i)).toBeInTheDocument()
  })

  it('pre-fills from duplicate router state and issues a create (POST)', async () => {
    const state: ProductDuplicateState = {
      duplicateFrom: {
        name: '',
        title: 'My Render',
        description: 'desc',
        category: 'Rendering',
        version: '1.0.0',
        template: 'name: copied',
      },
    }
    fetchMock.mockResolvedValueOnce(ok(makeProduct({ name: 'forked' }), 201))
    renderForm([{ pathname: '/products/new', state }])

    expect(screen.getByLabelText(/^title$/i)).toHaveValue('My Render')
    expect(screen.getByTestId('template-editor')).toHaveValue('name: copied')

    await userEvent.type(screen.getByLabelText(/^name$/i), 'forked')
    await userEvent.click(screen.getByRole('button', { name: /create product/i }))

    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      expect(post?.[0]).toBe('/api/v1/products')
    })
  })
})

describe('ProductForm (edit)', () => {
  it('prefills from GET and PUTs to the original name on save', async () => {
    fetchMock.mockResolvedValueOnce(ok(makeProduct())) // GET /products/my-render
    renderForm(['/products/my-render/edit'])

    const title = await screen.findByDisplayValue('My Render')
    await userEvent.clear(title)
    await userEvent.type(title, 'Renamed')

    fetchMock.mockResolvedValueOnce(ok(makeProduct({ title: 'Renamed' }))) // PUT
    fetchMock.mockResolvedValueOnce(ok(makeProduct({ title: 'Renamed' }))) // detail GET
    await userEvent.click(screen.getByRole('button', { name: /save/i }))

    await waitFor(() => {
      const put = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'PUT',
      )
      expect(put?.[0]).toBe('/api/v1/products/my-render')
    })
  })
})
