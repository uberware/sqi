// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor, fireEvent } from '@testing-library/react'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter, Routes, Route } from 'react-router-dom'
import type { ReactNode } from 'react'
import { ToastProvider } from '@/components/Toast'
import UserForm from './UserForm'

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
})
afterEach(() => vi.restoreAllMocks())

function ok(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
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

function renderCreate() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={['/users/new']}>
        <ToastProvider>
          {
            (
              <Routes>
                <Route path="/users/new" element={<UserForm mode="create" />} />
                <Route path="/users" element={<div>list page</div>} />
              </Routes>
            ) as ReactNode
          }
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

function renderEdit(id = 'u1') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[`/users/${id}/edit`]}>
        <ToastProvider>
          {
            (
              <Routes>
                <Route path="/users/:id/edit" element={<UserForm mode="edit" />} />
                <Route path="/users" element={<div>list page</div>} />
              </Routes>
            ) as ReactNode
          }
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('UserForm (create)', () => {
  it('defaults role to user', () => {
    renderCreate()
    expect((screen.getByLabelText(/^role/i) as HTMLSelectElement).value).toBe('user')
  })

  it('disables submit when username or password is empty', () => {
    renderCreate()
    expect(screen.getByRole('button', { name: /create user/i })).toBeDisabled()
  })

  it('POSTs username, password, and role on valid submit', async () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/^username/i), { target: { value: 'bob' } })
    fireEvent.change(screen.getByLabelText(/^password/i), { target: { value: 'hunter2!!' } })
    fireEvent.change(screen.getByLabelText(/^role/i), { target: { value: 'admin' } })
    fetchMock.mockResolvedValueOnce(ok(user({ username: 'bob', role: 'admin' })))
    fireEvent.click(screen.getByRole('button', { name: /create user/i }))
    await waitFor(() => {
      const post = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'POST',
      )
      expect(post).toBeDefined()
      const [, init] = post as [string, RequestInit]
      const body = JSON.parse(init.body as string) as Record<string, unknown>
      expect(body['username']).toBe('bob')
      expect(body['password']).toBe('hunter2!!')
      expect(body['role']).toBe('admin')
    })
    await screen.findByText('list page')
  })

  it('surfaces a useful message on a duplicate-username 409', async () => {
    renderCreate()
    fireEvent.change(screen.getByLabelText(/^username/i), { target: { value: 'alice' } })
    fireEvent.change(screen.getByLabelText(/^password/i), { target: { value: 'hunter2!!' } })
    fetchMock.mockResolvedValueOnce(
      new Response(
        JSON.stringify({
          type: 'about:blank',
          title: 'Conflict',
          status: 409,
          detail: 'username "alice" already exists',
        }),
        { status: 409, headers: { 'Content-Type': 'application/problem+json' } },
      ),
    )
    fireEvent.click(screen.getByRole('button', { name: /create user/i }))
    expect(await screen.findByText(/already exists/i)).toBeInTheDocument()
  })
})

describe('UserForm (edit)', () => {
  it('shows username read-only', async () => {
    fetchMock.mockResolvedValueOnce(ok(user()))
    renderEdit()
    const usernameField = await screen.findByLabelText(/^username/i)
    expect(usernameField).toHaveAttribute('readonly')
    expect(usernameField).toHaveValue('alice')
  })

  it('PATCHes display_name, role, and disabled on submit', async () => {
    fetchMock.mockResolvedValueOnce(ok(user()))
    renderEdit()
    await screen.findByLabelText(/^username/i)
    fireEvent.change(screen.getByLabelText(/display name/i), { target: { value: 'Alice A.' } })
    fireEvent.change(screen.getByLabelText(/^role/i), { target: { value: 'admin' } })
    fireEvent.click(screen.getByLabelText(/disabled/i))
    fetchMock.mockResolvedValueOnce(
      ok(user({ display_name: 'Alice A.', role: 'admin', disabled: true })),
    )
    fireEvent.click(screen.getByRole('button', { name: /^save$/i }))
    await waitFor(() => {
      const patch = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'PATCH',
      )
      expect(patch).toBeDefined()
      const [, init] = patch as [string, RequestInit]
      const body = JSON.parse(init.body as string) as Record<string, unknown>
      expect(body['display_name']).toBe('Alice A.')
      expect(body['role']).toBe('admin')
      expect(body['disabled']).toBe(true)
    })
    await screen.findByText('list page')
  })

  it('sets a new password via a separate action from the PATCH', async () => {
    fetchMock.mockResolvedValueOnce(ok(user()))
    renderEdit()
    await screen.findByLabelText(/^username/i)
    fireEvent.change(screen.getByLabelText(/new password/i), { target: { value: 'newpass123' } })
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    fireEvent.click(screen.getByRole('button', { name: /set password/i }))
    await waitFor(() => {
      const put = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'PUT',
      )
      expect(put).toBeDefined()
      const [url, init] = put as [string, RequestInit]
      expect(url).toContain('/users/u1/password')
      const body = JSON.parse(init.body as string) as Record<string, unknown>
      expect(body['password']).toBe('newpass123')
    })
    await screen.findByText(/password (updated|set)/i)
    // Only one PATCH-worthy call (the password PUT) happened — no PATCH fired alongside it.
    const patchCalls = fetchMock.mock.calls.filter(
      (c) => (c[1] as RequestInit | undefined)?.method === 'PATCH',
    )
    expect(patchCalls.length).toBe(0)
  })

  it('disables the role control for a directory-managed account', async () => {
    fetchMock.mockResolvedValueOnce(
      ok({
        id: 'u1',
        username: 'alice',
        display_name: 'Alice',
        role: 'user',
        auth_source: 'ldap',
        disabled: false,
        created_at: '2026-07-19T00:00:00Z',
        updated_at: '2026-07-19T00:00:00Z',
      }),
    )
    renderEdit('u1')
    const select = await screen.findByLabelText('Role')
    expect(select).toBeDisabled()
    expect(screen.getByText(/managed by the directory/i)).toBeInTheDocument()
  })

  it('leaves the role control enabled for a local account', async () => {
    fetchMock.mockResolvedValueOnce(
      ok({
        id: 'u2',
        username: 'bob',
        display_name: 'Bob',
        role: 'user',
        auth_source: 'local',
        disabled: false,
        created_at: '2026-07-19T00:00:00Z',
        updated_at: '2026-07-19T00:00:00Z',
      }),
    )
    renderEdit('u2')
    expect(await screen.findByLabelText('Role')).toBeEnabled()
  })

  it('omits role from the PATCH body for a directory-managed account', async () => {
    fetchMock.mockResolvedValueOnce(
      ok({
        id: 'u1',
        username: 'alice',
        display_name: 'Alice',
        role: 'user',
        auth_source: 'ldap',
        disabled: false,
        created_at: '2026-07-19T00:00:00Z',
        updated_at: '2026-07-19T00:00:00Z',
      }),
    )
    renderEdit('u1')
    await screen.findByLabelText('Role')
    fetchMock.mockResolvedValueOnce(ok({}))
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    // Located by method rather than a fixed call index: a successful PATCH
    // invalidates the user-detail query too, which refetches it as a third
    // fetch call (see the GET/PATCH/GET sequence elsewhere in this file).
    const patch = await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'PATCH',
      )
      expect(call).toBeDefined()
      return call as [string, RequestInit]
    })
    const body = JSON.parse(String(patch[1].body ?? '{}')) as Record<string, unknown>
    // Sending role would earn a 409 from the server; the client must not ask.
    expect(body).not.toHaveProperty('role')
    expect(body).toHaveProperty('display_name')
  })
})
