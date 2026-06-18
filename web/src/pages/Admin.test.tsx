// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import Admin from '@/pages/Admin'

vi.mock('@/ws/context', () => ({ useWebSocket: () => {} }))
vi.mock('@/api/diagnostics', () => ({
  useDiagnosticsLogs: () => ({
    data: { records: [{ ts: '2026-06-17T12:00:00Z', component: 'server', level: 'INFO', msg: 'server up' }] },
    isLoading: false,
    isError: false,
  }),
}))

describe('Admin page', () => {
  it('renders the server log section', async () => {
    render(
      <MemoryRouter>
        <Admin />
      </MemoryRouter>,
    )
    expect(screen.getByRole('heading', { name: /admin/i })).toBeInTheDocument()
    expect(await screen.findByText('server up')).toBeInTheDocument()
  })
})
