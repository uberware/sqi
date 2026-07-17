// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { WebSocketProvider } from '@/ws/context'
import { ThemeProvider } from '@/theme/context'
import { AuthProvider } from '@/auth/context'
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

const fetchMock = vi.fn<typeof fetch>()

const AUTHED_PRINCIPAL = {
  subject: 'u1',
  display_name: 'Test User',
  roles: ['operator'],
  kind: 'user',
}
const ANONYMOUS_PRINCIPAL = {
  subject: 'anonymous',
  display_name: 'Anonymous',
  roles: [],
  kind: 'anonymous',
}

function jsonResponse(status: number, body: unknown, contentType: string): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': contentType } })
}

/** Path suffix matcher against whatever shape (string | URL | Request) apiFetch was called with. */
function urlOf(input: Parameters<typeof fetch>[0]): string {
  if (input instanceof URL) return input.toString()
  if (typeof input === 'string') return input
  return input.url
}

beforeEach(() => {
  vi.stubGlobal('WebSocket', MockWebSocket)
  installLocalStorageMock()
  resetThemeDom()
  setMatchMedia(false)
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  // Default every test to a real authed principal so the pre-existing nav
  // assertions (which render synchronously, before any auth resolution)
  // keep working unchanged; tests exercising the logout control itself
  // override this via renderSidebarAs.
  fetchMock.mockImplementation((input) => {
    if (urlOf(input).includes('/auth/me')) {
      return Promise.resolve(jsonResponse(200, AUTHED_PRINCIPAL, 'application/json'))
    }
    return Promise.reject(new TypeError('Failed to fetch'))
  })
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function renderSidebar(initialEntry = '/') {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[initialEntry]}>
        <AuthProvider>
          <WebSocketProvider url="ws://test">
            <ThemeProvider>
              <Sidebar />
            </ThemeProvider>
          </WebSocketProvider>
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

/** Renders Sidebar with /auth/me stubbed to return the given principal. */
function renderSidebarAs(principal: unknown, initialEntry = '/') {
  fetchMock.mockImplementation((input) => {
    if (urlOf(input).includes('/auth/me')) {
      return Promise.resolve(jsonResponse(200, principal, 'application/json'))
    }
    return Promise.reject(new TypeError('Failed to fetch'))
  })
  return renderSidebar(initialEntry)
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
    for (const label of [
      'Farms',
      'Queues',
      'Usage Pools',
      'Storage',
      'Compute',
      'Products',
      'Presets',
    ]) {
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

  it('renders no deferred "coming soon" items', () => {
    const { container } = renderSidebar()
    expect(container.querySelectorAll('[data-disabled="true"]').length).toBe(0)
    expect(screen.queryByText('coming soon')).not.toBeInTheDocument()
    expect(screen.queryByText('Presets')).not.toBeInTheDocument()
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

  describe('logout control', () => {
    it('is visible when authed as a real user', async () => {
      renderSidebarAs(AUTHED_PRINCIPAL)
      expect(await screen.findByRole('button', { name: /log ?out/i })).toBeInTheDocument()
    })

    it('is absent when the principal is anonymous (auth disabled)', async () => {
      renderSidebarAs(ANONYMOUS_PRINCIPAL)
      // Let /auth/me resolve before asserting absence, otherwise the
      // assertion would trivially pass during the loading state too.
      await waitFor(() => expect(fetchMock).toHaveBeenCalled())
      await waitFor(() =>
        expect(screen.queryByRole('button', { name: /log ?out/i })).not.toBeInTheDocument(),
      )
    })

    it('calls POST /auth/logout and flips auth state to anonymous when clicked', async () => {
      renderSidebarAs(AUTHED_PRINCIPAL)
      const logoutBtn = await screen.findByRole('button', { name: /log ?out/i })
      fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
      // After logout, /auth/me is re-queried and must resolve to unauthenticated.
      fetchMock.mockResolvedValueOnce(
        jsonResponse(
          401,
          { status: 401, detail: 'authentication required' },
          'application/problem+json',
        ),
      )
      fireEvent.click(logoutBtn)
      await waitFor(() => {
        const logoutCall = fetchMock.mock.calls.find(
          (c) =>
            urlOf(c[0]).includes('/auth/logout') &&
            (c[1] as RequestInit | undefined)?.method === 'POST',
        )
        expect(logoutCall).toBeDefined()
      })
      await waitFor(() =>
        expect(screen.queryByRole('button', { name: /log ?out/i })).not.toBeInTheDocument(),
      )
    })
  })
})
