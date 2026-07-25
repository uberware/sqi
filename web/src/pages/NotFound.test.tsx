// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MemoryRouter } from 'react-router'
import NotFound from '@/pages/NotFound'

function renderNotFound() {
  return render(
    <MemoryRouter>
      <NotFound />
    </MemoryRouter>,
  )
}

describe('NotFound', () => {
  it('renders the "Page not found" heading', () => {
    renderNotFound()
    expect(screen.getByRole('heading', { name: /page not found/i })).toBeInTheDocument()
  })

  it('renders a descriptive message', () => {
    renderNotFound()
    expect(screen.getByText(/the page you.*doesn.*t exist/i)).toBeInTheDocument()
  })

  it('renders a link back to the dashboard', () => {
    renderNotFound()
    const link = screen.getByRole('link', { name: /back to dashboard/i })
    expect(link).toBeInTheDocument()
    expect((link as HTMLAnchorElement).getAttribute('href')).toBe('/')
  })
})
