// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import UserList from './UserList'

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  })
}

function user(overrides: Record<string, unknown> = {}) {
  return {
    id: 'u1',
    username: 'alice',
    role: 'operator',
    disabled: false,
    created_at: '2026-06-28T00:00:00Z',
    updated_at: '2026-06-28T00:00:00Z',
    ...overrides,
  }
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ToastProvider>{(<UserList />) as ReactNode}</ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('UserList', () => {
  it('renders users from the API', async () => {
    fetchMock.mockResolvedValueOnce(ok([user()]))
    renderPage()
    expect(await screen.findByRole('link', { name: 'alice' })).toBeInTheDocument()
  })

  it('shows the role', async () => {
    fetchMock.mockResolvedValueOnce(ok([user()]))
    renderPage()
    expect(await screen.findByText('operator')).toBeInTheDocument()
  })

  it('shows Disabled status for a disabled user', async () => {
    fetchMock.mockResolvedValueOnce(ok([user({ disabled: true })]))
    renderPage()
    expect(await screen.findByText(/disabled/i)).toBeInTheDocument()
  })

  it('shows Active status for an enabled user', async () => {
    fetchMock.mockResolvedValueOnce(ok([user({ disabled: false })]))
    renderPage()
    expect(await screen.findByText(/active/i)).toBeInTheDocument()
  })

  it('falls back to an em-dash when display_name is absent', async () => {
    fetchMock.mockResolvedValueOnce(ok([user()]))
    renderPage()
    await screen.findByRole('link', { name: 'alice' })
    expect(screen.getByText('—')).toBeInTheDocument()
  })

  it('renders the display_name when present', async () => {
    fetchMock.mockResolvedValueOnce(ok([user({ display_name: 'Alice A.' })]))
    renderPage()
    expect(await screen.findByText('Alice A.')).toBeInTheDocument()
  })

  it('shows an educational empty state', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    await waitFor(() => expect(screen.getByText(/no user accounts/i)).toBeInTheDocument())
  })

  it('has a New action linking to /users/new', async () => {
    fetchMock.mockResolvedValueOnce(ok([]))
    renderPage()
    const link = await screen.findByRole('link', { name: /new user/i })
    expect(link.getAttribute('href')).toBe('/users/new')
  })

  it('username links to /users/:id/edit', async () => {
    fetchMock.mockResolvedValueOnce(ok([user()]))
    renderPage()
    const link = await screen.findByRole('link', { name: 'alice' })
    expect(link.getAttribute('href')).toBe('/users/u1/edit')
  })

  it('confirms before deleting', async () => {
    const confirmSpy = vi.spyOn(window, 'confirm').mockReturnValue(false)
    fetchMock.mockResolvedValueOnce(ok([user()]))
    renderPage()
    await screen.findByRole('button', { name: /delete user alice/i })
    fireEvent.click(screen.getByRole('button', { name: /delete user alice/i }))
    expect(confirmSpy).toHaveBeenCalled()
  })

  it('calls DELETE and shows success toast when confirmed', async () => {
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    fetchMock.mockResolvedValueOnce(ok([user()]))
    renderPage()
    await screen.findByRole('button', { name: /delete user alice/i })
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    fireEvent.click(screen.getByRole('button', { name: /delete user alice/i }))
    await waitFor(() => {
      const del = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'DELETE',
      )
      expect(del).toBeDefined()
    })
    await screen.findByText(/user "alice" deleted/i)
  })
})
