// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router'
import { ToastProvider } from '@/components/Toast'
import { userPrincipal, mockAuth } from '@/test/principals'
import Account from './Account'

vi.mock('@/auth/context', () => ({ useAuth: vi.fn() }))

const ALICE = { ...userPrincipal(), subject: 'u-alice', username: 'alice', display_name: 'Alice' }

const fetchMock = vi.fn<typeof fetch>()

beforeEach(() => {
  fetchMock.mockReset()
  vi.stubGlobal('fetch', fetchMock)
  mockAuth(ALICE)
})

afterEach(() => {
  vi.restoreAllMocks()
})

function problemResponse(status: number, detail: string): Response {
  return new Response(JSON.stringify({ title: 'error', status, detail }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter>
        <ToastProvider>
          <Account />
        </ToastProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  )
}

describe('Account', () => {
  it('submits a password change', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    const user = userEvent.setup()
    renderPage()

    await user.type(screen.getByLabelText(/current password/i), 'old-pw')
    await user.type(screen.getByLabelText(/^new password/i), 'new-pw')
    await user.click(screen.getByRole('button', { name: /change password/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'PUT',
      )
      expect(call).toBeDefined()
      expect(String(call?.[0])).toContain('/auth/password')
      expect(JSON.parse(String((call?.[1] as RequestInit).body))).toEqual({
        current_password: 'old-pw',
        new_password: 'new-pw',
      })
    })
  })

  it('clears both password fields after a successful change', async () => {
    fetchMock.mockResolvedValueOnce(new Response(null, { status: 204 }))
    const user = userEvent.setup()
    renderPage()

    const current = screen.getByLabelText(/current password/i)
    const next = screen.getByLabelText(/^new password/i)
    await user.type(current, 'old-pw')
    await user.type(next, 'new-pw')
    await user.click(screen.getByRole('button', { name: /change password/i }))

    await waitFor(() => {
      expect(current).toHaveValue('')
      expect(next).toHaveValue('')
    })
  })

  // A wrong current password is a correctable input mistake, so it belongs
  // beside the field — not in a transient toast the user may miss.
  it('shows a 403 as an inline field error', async () => {
    fetchMock.mockResolvedValueOnce(problemResponse(403, 'current password is incorrect'))
    const user = userEvent.setup()
    renderPage()

    await user.type(screen.getByLabelText(/current password/i), 'wrong')
    await user.type(screen.getByLabelText(/^new password/i), 'new-pw')
    await user.click(screen.getByRole('button', { name: /change password/i }))

    expect(await screen.findByText(/current password is incorrect/i)).toBeInTheDocument()
  })

  it('keeps the typed password on screen after a failed attempt', async () => {
    fetchMock.mockResolvedValueOnce(problemResponse(403, 'current password is incorrect'))
    const user = userEvent.setup()
    renderPage()

    await user.type(screen.getByLabelText(/current password/i), 'wrong')
    await user.type(screen.getByLabelText(/^new password/i), 'new-pw')
    await user.click(screen.getByRole('button', { name: /change password/i }))

    await screen.findByText(/current password is incorrect/i)
    expect(screen.getByLabelText(/^new password/i)).toHaveValue('new-pw')
  })

  it('disables the password submit until both fields are filled', async () => {
    const user = userEvent.setup()
    renderPage()

    const submit = screen.getByRole('button', { name: /change password/i })
    expect(submit).toBeDisabled()

    await user.type(screen.getByLabelText(/current password/i), 'old-pw')
    expect(submit).toBeDisabled()

    await user.type(screen.getByLabelText(/^new password/i), 'new-pw')
    expect(submit).toBeEnabled()
  })

  it('submits a display-name change', async () => {
    fetchMock.mockResolvedValueOnce(
      new Response(JSON.stringify({ ...ALICE, display_name: 'Alice A.' }), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
    )
    const user = userEvent.setup()
    renderPage()

    const field = screen.getByLabelText(/display name/i)
    await user.clear(field)
    await user.type(field, 'Alice A.')
    await user.click(screen.getByRole('button', { name: /save profile/i }))

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(
        (c) => (c[1] as RequestInit | undefined)?.method === 'PATCH',
      )
      expect(call).toBeDefined()
      expect(String(call?.[0])).toContain('/auth/me')
      expect(JSON.parse(String((call?.[1] as RequestInit).body))).toEqual({
        display_name: 'Alice A.',
      })
    })
  })

  it('prefills the display name from the current principal', () => {
    renderPage()
    expect(screen.getByLabelText(/display name/i)).toHaveValue('Alice')
  })
})
