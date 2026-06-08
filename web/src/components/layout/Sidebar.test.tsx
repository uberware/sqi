// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { MemoryRouter } from 'react-router-dom'
import Sidebar from '@/components/layout/Sidebar'

function renderSidebar(initialEntry = '/') {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <Sidebar />
    </MemoryRouter>,
  )
}

describe('Sidebar', () => {
  it('renders all Phase 1 nav links', () => {
    renderSidebar()
    expect(screen.getByRole('link', { name: 'Dashboard' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Jobs' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Workers' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Submit' })).toBeInTheDocument()
  })

  it('Phase 1 links point to correct paths', () => {
    renderSidebar()
    expect(
      (screen.getByRole('link', { name: 'Dashboard' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/')
    expect(
      (screen.getByRole('link', { name: 'Jobs' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/jobs')
    expect(
      (screen.getByRole('link', { name: 'Workers' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/workers')
    expect(
      (screen.getByRole('link', { name: 'Submit' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/submit')
  })

  it('Dashboard link is active at / and inactive at /jobs', () => {
    renderSidebar('/')
    expect(screen.getByRole('link', { name: 'Dashboard' }).getAttribute('aria-current')).toBe(
      'page',
    )
    expect(screen.getByRole('link', { name: 'Jobs' }).getAttribute('aria-current')).toBeNull()
  })

  it('Dashboard link is not active at /jobs (end semantics)', () => {
    renderSidebar('/jobs')
    expect(screen.getByRole('link', { name: 'Dashboard' }).getAttribute('aria-current')).toBeNull()
    expect(screen.getByRole('link', { name: 'Jobs' }).getAttribute('aria-current')).toBe('page')
  })

  it('renders deferred Phase 2+ items as non-navigable disabled spans', () => {
    const { container } = renderSidebar()
    const disabledItems = container.querySelectorAll('[data-disabled="true"]')
    expect(disabledItems.length).toBe(5)
    disabledItems.forEach((item) => {
      expect(item.tagName.toLowerCase()).toBe('span')
    })
  })

  it('shows "coming soon" badge for each deferred item', () => {
    renderSidebar()
    const badges = screen.getAllByText('coming soon')
    expect(badges.length).toBe(5)
  })

  it('deferred items include the expected labels', () => {
    renderSidebar()
    expect(screen.getByText('Presets')).toBeInTheDocument()
    expect(screen.getByText('Products')).toBeInTheDocument()
    expect(screen.getByText('Storage')).toBeInTheDocument()
    expect(screen.getByText('License Pools')).toBeInTheDocument()
    expect(screen.getByText('Settings')).toBeInTheDocument()
  })

  it('has accessible navigation landmark', () => {
    renderSidebar()
    expect(screen.getByRole('navigation', { name: 'Primary navigation' })).toBeInTheDocument()
  })
})
