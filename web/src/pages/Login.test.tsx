// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { AuthProvider } from '@/auth/context'
import Login from './Login'

const fetchMock = vi.fn<typeof fetch>()
beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  // The SSO failure marker lives in window.location.search, so a test that
  // sets it would otherwise leak into every test that runs after it.
  window.history.replaceState({}, '', '/')
})
afterEach(() => vi.restoreAllMocks())

function jsonResponse(status: number, body: unknown, contentType: string): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': contentType } })
}

/** The 401 problem body an anonymous `GET /auth/me` returns. */
function anonMe(): Response {
  return jsonResponse(
    401,
    { status: 401, detail: 'authentication required' },
    'application/problem+json',
  )
}

/**
 * Route the fetch mock by request URL rather than by call order.
 *
 * Login issues two independent queries on mount — `/auth/me` from the
 * AuthProvider and `/auth/providers` for the SSO buttons — and nothing
 * guarantees which lands first. A queue of `mockResolvedValueOnce` values
 * would therefore hand the wrong body to the wrong endpoint, intermittently.
 *
 * Each route is a factory rather than a `Response`, because a `Response` body
 * can only be read once and several of these endpoints are called more than
 * once per test. An unrouted URL answers 404 loudly instead of `undefined`, so
 * a missing route surfaces as a named failure rather than a crash inside
 * `apiFetch`.
 */
function mockEndpoints(routes: Record<string, () => Response>): void {
  fetchMock.mockImplementation((input: RequestInfo | URL) => {
    const url = String(input instanceof Request ? input.url : input)
    for (const [fragment, respond] of Object.entries(routes)) {
      if (url.includes(fragment)) return Promise.resolve(respond())
    }
    return Promise.resolve(
      jsonResponse(
        404,
        { status: 404, detail: `test: no route for ${url}` },
        'application/problem+json',
      ),
    )
  })
}

/** Anonymous `/auth/me` plus the given `/auth/providers` body. */
function mockAuthEndpoints(providers: unknown): void {
  mockEndpoints({
    '/auth/providers': () => jsonResponse(200, providers, 'application/json'),
    '/auth/me': anonMe,
  })
}

/** A deployment offering password login only. */
const passwordOnly = { password: true, sso: [] }

function renderLogin() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <AuthProvider>
          <Login />
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('Login', () => {
  it('renders labeled username and password inputs', async () => {
    mockAuthEndpoints(passwordOnly)
    renderLogin()
    expect(await screen.findByLabelText(/username/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/password/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/password/i)).toHaveAttribute('type', 'password')
  })

  it('submits credentials and calls POST /auth/login', async () => {
    mockEndpoints({
      '/auth/providers': () => jsonResponse(200, passwordOnly, 'application/json'),
      '/auth/login': () =>
        jsonResponse(
          200,
          {
            id: 'u1',
            username: 'alice',
            role: 'operator',
            disabled: false,
            created_at: '',
            updated_at: '',
          },
          'application/json',
        ),
      '/auth/me': anonMe,
    })
    renderLogin()
    fireEvent.change(await screen.findByLabelText(/username/i), { target: { value: 'alice' } })
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: 'pw' } })
    fireEvent.click(screen.getByRole('button', { name: /log in/i }))
    await waitFor(() => {
      const login = fetchMock.mock.calls.find((c) => String(c[0]).endsWith('/auth/login'))
      expect(login).toBeDefined()
    })
    const [, init] = fetchMock.mock.calls.find((c) => String(c[0]).endsWith('/auth/login')) as [
      string,
      RequestInit,
    ]
    expect(init.method).toBe('POST')
    expect(JSON.parse(init.body as string)).toEqual({ username: 'alice', password: 'pw' })
  })

  it('shows the 401 detail on wrong credentials and stays on the login page', async () => {
    mockEndpoints({
      '/auth/providers': () => jsonResponse(200, passwordOnly, 'application/json'),
      '/auth/login': () =>
        jsonResponse(
          401,
          { status: 401, detail: 'invalid credentials' },
          'application/problem+json',
        ),
      '/auth/me': anonMe,
    })
    renderLogin()
    fireEvent.change(await screen.findByLabelText(/username/i), { target: { value: 'alice' } })
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: 'wrong' } })
    fireEvent.click(screen.getByRole('button', { name: /log in/i }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/invalid credentials/i)
    // Still on the login page.
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument()
  })

  it('renders an SSO button when the server advertises one', async () => {
    mockAuthEndpoints({
      password: true,
      sso: [{ id: 'oidc', label: 'Sign in with Okta', login_url: '/api/v1/auth/oidc/login' }],
    })
    renderLogin()

    const link = await screen.findByRole('link', { name: 'Sign in with Okta' })
    // An anchor, not a button with an onClick: the point of the flow is leaving
    // the page. A fetch would follow the redirect in the background and land the
    // provider's login HTML in a response body.
    expect(link).toHaveAttribute('href', '/api/v1/auth/oidc/login')
  })

  it('shows no SSO button when none is advertised', async () => {
    mockAuthEndpoints(passwordOnly)
    renderLogin()

    expect(await screen.findByLabelText('Username')).toBeInTheDocument()
    expect(screen.queryByRole('link')).not.toBeInTheDocument()
  })

  it('surfaces a generic message when the callback redirected with an error', async () => {
    mockAuthEndpoints(passwordOnly)
    window.history.replaceState({}, '', '/?sso_error=1')
    renderLogin()

    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/sign-in failed/i)
    // Generic on purpose: the server refuses to say why, because distinguishing
    // "unknown user" from "no role mapping" would make the callback an
    // enumeration oracle. The web must not invent a reason it was never told.
    expect(alert.textContent ?? '').not.toMatch(/unknown|not found|role|mapping|claim/i)
  })

  it('shows no SSO error when the marker is absent', async () => {
    mockAuthEndpoints(passwordOnly)
    renderLogin()

    expect(await screen.findByLabelText('Username')).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })
})
