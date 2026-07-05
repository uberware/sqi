// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, it, expect, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import { MemoryRouter, useLocation } from 'react-router-dom'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import ProductPicker from './ProductPicker'

vi.mock('@/api/queries', async (orig) => ({
  ...(await orig<typeof import('@/api/queries')>()),
  useProducts: () => ({
    data: [
      {
        name: 'my-tool',
        title: 'My Tool',
        category: 'misc',
        description: 'An internal utility',
        version: '1',
        source: 'custom',
        template: '',
        format: 'yaml',
      },
      {
        name: 'blender',
        title: 'Blender',
        category: 'render',
        description: '',
        version: '1',
        source: 'builtin',
        template: '',
        format: 'yaml',
      },
    ],
    isLoading: false,
    error: null,
  }),
}))

function LocationProbe() {
  const location = useLocation()
  return <div data-testid="location-search">{location.search}</div>
}

function renderPage(route = '/submit') {
  const qc = new QueryClient()
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter initialEntries={[route]}>
        <LocationProbe />
        <ProductPicker />
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('ProductPicker', () => {
  it('lists products with a submit link and an advanced raw link', () => {
    renderPage()
    expect(screen.getByText('Blender')).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Blender/ })).toHaveAttribute(
      'href',
      '/submit/product/blender',
    )
    expect(screen.getByRole('link', { name: /My Tool/ })).toHaveAttribute(
      'href',
      '/submit/product/my-tool',
    )
    expect(screen.getByRole('link', { name: /raw OpenJD/i })).toHaveAttribute('href', '/submit/raw')
  })

  it('groups products by source, builtin first, hiding empty groups', () => {
    renderPage()
    const headings = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)
    // builtin group precedes custom; no 'installed' product → no 'Installed' group.
    expect(headings).toEqual(['Built In', 'Custom'])
    expect(screen.queryByRole('heading', { name: 'Installed' })).not.toBeInTheDocument()
  })

  it('filters products as you type and hides emptied groups', async () => {
    renderPage()
    await userEvent.type(screen.getByRole('searchbox'), 'blender')
    await waitFor(() =>
      expect(screen.queryByRole('link', { name: /My Tool/ })).not.toBeInTheDocument(),
    )
    expect(screen.getByRole('link', { name: /Blender/ })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Custom' })).not.toBeInTheDocument()
  })

  it('updates the URL with ?search= after the debounce settles', async () => {
    renderPage()
    await userEvent.type(screen.getByRole('searchbox'), 'blender')
    await waitFor(() =>
      expect(screen.getByTestId('location-search').textContent).toContain('search=blender'),
    )
  })

  it('applies an initial ?search= from the URL on first render', () => {
    renderPage('/submit?search=my-tool')
    expect(screen.getByRole('searchbox')).toHaveValue('my-tool')
    expect(screen.getByRole('link', { name: /My Tool/ })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /Blender/ })).not.toBeInTheDocument()
  })

  it('matches on description text', () => {
    renderPage('/submit?search=internal utility')
    expect(screen.getByRole('link', { name: /My Tool/ })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /Blender/ })).not.toBeInTheDocument()
  })

  it('shows a no-match message but keeps the advanced raw link', () => {
    renderPage('/submit?search=zzz')
    expect(screen.getByText(/No products match/)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /raw OpenJD/i })).toHaveAttribute('href', '/submit/raw')
  })
})
