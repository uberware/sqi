import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import Sidebar from '@/components/layout/Sidebar'

describe('Sidebar', () => {
  it('renders all Phase 1 nav links', () => {
    render(<Sidebar />)
    expect(screen.getByRole('link', { name: 'Dashboard' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Jobs' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Workers' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Submit' })).toBeInTheDocument()
  })

  it('Phase 1 links point to correct paths', () => {
    render(<Sidebar />)
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

  it('renders deferred Phase 2+ items as non-navigable disabled spans', () => {
    const { container } = render(<Sidebar />)
    const disabledItems = container.querySelectorAll('[data-disabled="true"]')
    expect(disabledItems.length).toBe(5)
    disabledItems.forEach((item) => {
      expect(item.tagName.toLowerCase()).toBe('span')
    })
  })

  it('shows "coming soon" badge for each deferred item', () => {
    render(<Sidebar />)
    const badges = screen.getAllByText('coming soon')
    expect(badges.length).toBe(5)
  })

  it('deferred items include the expected labels', () => {
    render(<Sidebar />)
    expect(screen.getByText('Presets')).toBeInTheDocument()
    expect(screen.getByText('Products')).toBeInTheDocument()
    expect(screen.getByText('Storage')).toBeInTheDocument()
    expect(screen.getByText('License Pools')).toBeInTheDocument()
    expect(screen.getByText('Settings')).toBeInTheDocument()
  })

  it('has accessible navigation landmark', () => {
    render(<Sidebar />)
    expect(screen.getByRole('navigation', { name: 'Primary navigation' })).toBeInTheDocument()
  })
})
