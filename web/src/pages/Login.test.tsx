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
})
afterEach(() => vi.restoreAllMocks())

function jsonResponse(status: number, body: unknown, contentType: string): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': contentType } })
}

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
    fetchMock.mockResolvedValueOnce(
      jsonResponse(
        401,
        { status: 401, detail: 'authentication required' },
        'application/problem+json',
      ),
    )
    renderLogin()
    expect(await screen.findByLabelText(/username/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/password/i)).toBeInTheDocument()
    expect(screen.getByLabelText(/password/i)).toHaveAttribute('type', 'password')
  })

  it('submits credentials and calls POST /auth/login', async () => {
    // /auth/me (anon) then /auth/login (ok) then /auth/me (authed, from invalidation)
    fetchMock
      .mockResolvedValueOnce(
        jsonResponse(401, { status: 401, detail: 'x' }, 'application/problem+json'),
      )
      .mockResolvedValueOnce(
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
      )
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
    fetchMock
      .mockResolvedValueOnce(
        jsonResponse(401, { status: 401, detail: 'x' }, 'application/problem+json'),
      )
      .mockResolvedValueOnce(
        jsonResponse(
          401,
          { status: 401, detail: 'invalid credentials' },
          'application/problem+json',
        ),
      )
    renderLogin()
    fireEvent.change(await screen.findByLabelText(/username/i), { target: { value: 'alice' } })
    fireEvent.change(screen.getByLabelText(/password/i), { target: { value: 'wrong' } })
    fireEvent.click(screen.getByRole('button', { name: /log in/i }))
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/invalid credentials/i)
    // Still on the login page.
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument()
  })
})
