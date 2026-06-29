// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import ServerLog from '@/pages/ServerLog'

vi.mock('@/ws/context', () => ({ useWebSocket: () => {} }))
vi.mock('@/api/diagnostics', () => ({
  useDiagnosticsLogs: () => ({
    data: {
      records: [
        { ts: '2026-06-17T12:00:00Z', component: 'server', level: 'INFO', msg: 'server up' },
      ],
    },
    isLoading: false,
    isError: false,
  }),
}))

describe('ServerLog page', () => {
  it('renders the server diagnostic log', async () => {
    render(
      <MemoryRouter>
        <ServerLog />
      </MemoryRouter>,
    )
    expect(await screen.findByText('server up')).toBeInTheDocument()
  })

  it('has a back link to the Admin hub', () => {
    render(
      <MemoryRouter>
        <ServerLog />
      </MemoryRouter>,
    )
    const back = screen.getByRole('link', { name: /admin/i })
    expect(back.getAttribute('href')).toBe('/admin')
  })
})
