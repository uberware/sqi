// SPDX-License-Identifier: AGPL-3.0-or-later
import { describe, it, expect, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import ProductPicker from './ProductPicker'

vi.mock('@/api/queries', async (orig) => ({
  ...(await orig<typeof import('@/api/queries')>()),
  useProducts: () => ({
    data: [
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

function renderPage() {
  const qc = new QueryClient()
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
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
    expect(screen.getByRole('link', { name: /raw OpenJD/i })).toHaveAttribute('href', '/submit/raw')
  })
})
