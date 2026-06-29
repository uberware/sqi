// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { MemoryRouter } from 'react-router-dom'
import { WebSocketProvider } from '@/ws/context'
import { ThemeProvider } from '@/theme/context'
import Sidebar from '@/components/layout/Sidebar'
import { installLocalStorageMock, setMatchMedia, resetThemeDom } from '@/theme/test-utils'

class MockWebSocket {
  static readonly OPEN = 1
  onopen: null = null
  onclose: null = null
  onerror: null = null
  onmessage: null = null
  send(): void {}
  close(): void {}
}

beforeEach(() => {
  vi.stubGlobal('WebSocket', MockWebSocket)
  installLocalStorageMock()
  resetThemeDom()
  setMatchMedia(false)
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function renderSidebar(initialEntry = '/') {
  return render(
    <MemoryRouter initialEntries={[initialEntry]}>
      <WebSocketProvider url="ws://test">
        <ThemeProvider>
          <Sidebar />
        </ThemeProvider>
      </WebSocketProvider>
    </MemoryRouter>,
  )
}

describe('Sidebar', () => {
  it('renders all Phase 1 nav links', () => {
    renderSidebar()
    expect(screen.getByRole('link', { name: 'Dashboard' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Submit' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Jobs' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Workers' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Admin' })).toBeInTheDocument()
  })

  it('Phase 1 links point to correct paths', () => {
    renderSidebar()
    expect(
      (screen.getByRole('link', { name: 'Dashboard' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/')
    expect(
      (screen.getByRole('link', { name: 'Submit' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/submit')
    expect(
      (screen.getByRole('link', { name: 'Jobs' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/jobs')
    expect(
      (screen.getByRole('link', { name: 'Workers' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/workers')
    expect(
      (screen.getByRole('link', { name: 'Admin' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/admin')
  })

  it('does not show the management links moved under Admin', () => {
    renderSidebar()
    for (const label of ['Farms', 'Queues', 'Usage Pools', 'Storage', 'Compute', 'Products']) {
      expect(screen.queryByRole('link', { name: label })).not.toBeInTheDocument()
    }
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
    expect(disabledItems.length).toBe(1)
    disabledItems.forEach((item) => {
      expect(item.tagName.toLowerCase()).toBe('span')
    })
  })

  it('shows "coming soon" badge for each deferred item', () => {
    renderSidebar()
    const badges = screen.getAllByText('coming soon')
    expect(badges.length).toBe(1)
  })

  it('deferred items include the expected labels', () => {
    renderSidebar()
    expect(screen.getByText('Presets')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Products' })).not.toBeInTheDocument()
    expect(screen.queryByText('Settings')).not.toBeInTheDocument()
  })

  it('Admin is an active nav link to /admin', () => {
    renderSidebar()
    const adminLink = screen.getByRole('link', { name: 'Admin' })
    expect(adminLink.getAttribute('href')).toBe('/admin')
  })

  it('has accessible navigation landmark', () => {
    renderSidebar()
    expect(screen.getByRole('navigation', { name: 'Primary navigation' })).toBeInTheDocument()
  })

  it('renders the theme toggle switch in the footer', () => {
    renderSidebar()
    expect(screen.getByRole('switch', { name: /dark mode/i })).toBeInTheDocument()
  })
})
