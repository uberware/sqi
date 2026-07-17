// SPDX-License-Identifier: AGPL-3.0-or-later

import { render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi, beforeEach, afterEach } from 'vitest'
import { MemoryRouter } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ThemeProvider } from '@/theme/context'
import { AuthProvider } from '@/auth/context'
import App from '@/App'
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

/** Path suffix matcher against whatever shape (string | URL | Request) apiFetch was called with. */
function urlOf(input: Parameters<typeof fetch>[0]): string {
  if (input instanceof URL) return input.toString()
  if (typeof input === 'string') return input
  return input.url
}

function jsonResponse(status: number, body: unknown, contentType: string): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': contentType } })
}

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

/**
 * Every other query the shell fires (jobs, workers, farms, usage pools, …)
 * is left to fail fast with a network-style error — mirroring how these
 * tests behaved before this file mocked fetch at all (real fetch calls to a
 * non-existent server also just failed), and letting the components' own
 * tolerance for failed queries carry the rest.
 */
function stubAuthMe(response: Response) {
  fetchMock.mockImplementation((input) => {
    if (urlOf(input).includes('/auth/me')) return Promise.resolve(response)
    return Promise.reject(new TypeError('Failed to fetch'))
  })
}

beforeEach(() => {
  vi.stubGlobal('WebSocket', MockWebSocket)
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  installLocalStorageMock()
  resetThemeDom()
  setMatchMedia(false)
})

afterEach(() => {
  vi.restoreAllMocks()
  vi.unstubAllGlobals()
})

function renderApp() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <AuthProvider>
          <ThemeProvider>
            <App />
          </ThemeProvider>
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('App', () => {
  describe('authenticated (a real user principal)', () => {
    beforeEach(() => {
      stubAuthMe(jsonResponse(200, AUTHED_PRINCIPAL, 'application/json'))
    })

    it('renders the primary navigation sidebar', async () => {
      renderApp()
      expect(
        await screen.findByRole('navigation', { name: 'Primary navigation' }),
      ).toBeInTheDocument()
    })

    it('renders the main content area', async () => {
      renderApp()
      expect(await screen.findByRole('main')).toBeInTheDocument()
    })

    it('renders the dashboard at the root path', async () => {
      renderApp()
      expect(await screen.findByRole('heading', { name: 'dASHBOARD' })).toBeInTheDocument()
    })

    it('does not render the login form', async () => {
      renderApp()
      await screen.findByRole('navigation', { name: 'Primary navigation' })
      expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument()
    })
  })

  describe('unauthenticated (/auth/me returns 401)', () => {
    beforeEach(() => {
      stubAuthMe(
        jsonResponse(
          401,
          { status: 401, detail: 'authentication required' },
          'application/problem+json',
        ),
      )
    })

    it('renders the login page instead of the app shell', async () => {
      renderApp()
      expect(await screen.findByLabelText(/username/i)).toBeInTheDocument()
      expect(
        screen.queryByRole('navigation', { name: 'Primary navigation' }),
      ).not.toBeInTheDocument()
    })
  })

  describe('auth-off regression (/auth/me returns 200 with the anonymous principal)', () => {
    beforeEach(() => {
      stubAuthMe(jsonResponse(200, ANONYMOUS_PRINCIPAL, 'application/json'))
    })

    it('renders the app shell, not the login page — auth-off must never show a login screen', async () => {
      renderApp()
      expect(
        await screen.findByRole('navigation', { name: 'Primary navigation' }),
      ).toBeInTheDocument()
      expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument()
    })

    it('shows no login/logout UI at all', async () => {
      renderApp()
      await screen.findByRole('navigation', { name: 'Primary navigation' })
      expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument()
      expect(screen.queryByLabelText(/password/i)).not.toBeInTheDocument()
      expect(screen.queryByRole('button', { name: /log ?out/i })).not.toBeInTheDocument()
    })
  })

  it('shows a loading state before /auth/me resolves', async () => {
    let resolveAuthMe!: (r: Response) => void
    fetchMock.mockImplementation((input) => {
      if (urlOf(input).includes('/auth/me')) {
        return new Promise<Response>((resolve) => {
          resolveAuthMe = resolve
        })
      }
      return Promise.reject(new TypeError('Failed to fetch'))
    })
    renderApp()
    expect(screen.queryByRole('navigation')).not.toBeInTheDocument()
    expect(screen.queryByLabelText(/username/i)).not.toBeInTheDocument()
    resolveAuthMe(jsonResponse(200, AUTHED_PRINCIPAL, 'application/json'))
    await waitFor(() => expect(screen.getByRole('navigation')).toBeInTheDocument())
  })
})
