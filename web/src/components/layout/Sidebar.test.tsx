// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { MemoryRouter } from 'react-router'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { WebSocketProvider } from '@/ws/context'
import { ThemeProvider } from '@/theme/context'
import { AuthProvider } from '@/auth/context'
import Sidebar from '@/components/layout/Sidebar'
import { installLocalStorageMock, setMatchMedia, resetThemeDom } from '@/theme/test-utils'
import {
  ADMIN_PERMISSIONS,
  ALL_PERMISSIONS,
  OPERATOR_PERMISSIONS,
  READ_ONLY_PERMISSIONS,
} from '@/test/principals'

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
  permissions: OPERATOR_PERMISSIONS,
}
const ANONYMOUS_PRINCIPAL = {
  subject: 'anonymous',
  display_name: 'Anonymous',
  roles: [],
  kind: 'anonymous',
  permissions: ALL_PERMISSIONS,
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
  it('renders all Phase 1 nav links', async () => {
    renderSidebar()
    // Nav items are now permission-gated on the resolved principal (async via
    // /auth/me), so wait for one gated item before asserting the rest.
    expect(await screen.findByRole('link', { name: 'Submit' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Dashboard' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Jobs' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Workers' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Admin' })).toBeInTheDocument()
  })

  it('Phase 1 links point to correct paths', async () => {
    renderSidebar()
    expect((await screen.findByRole('link', { name: 'Submit' })).getAttribute('href')).toBe(
      '/submit',
    )
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
      (screen.getByRole('link', { name: 'Admin' }) as HTMLAnchorElement).getAttribute('href'),
    ).toBe('/admin')
  })

  it('does not show the management links moved under Admin', async () => {
    renderSidebar()
    await screen.findByRole('link', { name: 'Submit' })
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

  it('Dashboard link is active at / and inactive at /jobs', async () => {
    renderSidebar('/')
    await screen.findByRole('link', { name: 'Jobs' })
    expect(screen.getByRole('link', { name: 'Dashboard' }).getAttribute('aria-current')).toBe(
      'page',
    )
    expect(screen.getByRole('link', { name: 'Jobs' }).getAttribute('aria-current')).toBeNull()
  })

  it('Dashboard link is not active at /jobs (end semantics)', async () => {
    renderSidebar('/jobs')
    await screen.findByRole('link', { name: 'Jobs' })
    expect(screen.getByRole('link', { name: 'Dashboard' }).getAttribute('aria-current')).toBeNull()
    expect(screen.getByRole('link', { name: 'Jobs' }).getAttribute('aria-current')).toBe('page')
  })

  it('renders no deferred "coming soon" items', async () => {
    const { container } = renderSidebar()
    await screen.findByRole('link', { name: 'Submit' })
    expect(container.querySelectorAll('[data-disabled="true"]').length).toBe(0)
    expect(screen.queryByText('coming soon')).not.toBeInTheDocument()
    expect(screen.queryByText('Presets')).not.toBeInTheDocument()
  })

  it('Admin is an active nav link to /admin', async () => {
    renderSidebar()
    const adminLink = await screen.findByRole('link', { name: 'Admin' })
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

  describe('nav filtering by permission', () => {
    it('read-only principal: Submit absent, Jobs/Workers/Dashboard/Admin present', async () => {
      renderSidebarAs({
        subject: 'u2',
        display_name: 'Read Only User',
        roles: ['read-only'],
        kind: 'user',
        permissions: READ_ONLY_PERMISSIONS,
      })
      // read-only lacks jobs.write, but holds apikeys.self, which is enough
      // for the Admin hub's api-keys card — so Admin should still show.
      expect(await screen.findByRole('link', { name: 'Admin' })).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Dashboard' })).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Jobs' })).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Workers' })).toBeInTheDocument()
      expect(screen.queryByRole('link', { name: 'Submit' })).not.toBeInTheDocument()
    })

    it('admin principal: Submit, Jobs, Workers, Admin all present', async () => {
      renderSidebarAs({
        subject: 'u3',
        display_name: 'Admin User',
        roles: ['admin'],
        kind: 'user',
        permissions: ADMIN_PERMISSIONS,
      })
      expect(await screen.findByRole('link', { name: 'Submit' })).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Jobs' })).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Workers' })).toBeInTheDocument()
      expect(screen.getByRole('link', { name: 'Admin' })).toBeInTheDocument()
    })

    it('principal with no known role: Submit and Admin absent', async () => {
      renderSidebarAs({
        subject: 'u4',
        display_name: 'Unknown Role User',
        roles: ['nonexistent-role'],
        kind: 'user',
        permissions: [],
      })
      // Dashboard is ungated, so wait on it to let /auth/me resolve before
      // asserting the absent items.
      await screen.findByRole('link', { name: 'Dashboard' })
      expect(screen.queryByRole('link', { name: 'Submit' })).not.toBeInTheDocument()
      expect(screen.queryByRole('link', { name: 'Admin' })).not.toBeInTheDocument()
    })
  })

  describe('account link', () => {
    it('is visible when authed as a real user', async () => {
      renderSidebarAs(AUTHED_PRINCIPAL)
      const link = await screen.findByRole('link', { name: 'Account' })
      expect(link).toHaveAttribute('href', '/account')
    })

    // With auth off the anonymous superuser has no account record, and both
    // self-service routes answer 409 — so the link must not be offered.
    it('is absent when the principal is anonymous (auth disabled)', async () => {
      renderSidebarAs(ANONYMOUS_PRINCIPAL)
      // Let /auth/me resolve before asserting absence, otherwise the
      // assertion would trivially pass during the loading state too.
      await waitFor(() => expect(fetchMock).toHaveBeenCalled())
      await waitFor(() =>
        expect(screen.queryByRole('link', { name: 'Account' })).not.toBeInTheDocument(),
      )
    })
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
      // 200 with a JSON body, not 204: since C2 the server answers logout with
      // a LogoutResult, which carries the provider's end-session URL under
      // logout_mode=provider and is `{}` otherwise.
      fetchMock.mockResolvedValueOnce(jsonResponse(200, {}, 'application/json'))
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
