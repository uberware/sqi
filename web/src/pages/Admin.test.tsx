// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import Admin from '@/pages/Admin'

vi.mock('@/api/queries', () => ({
  useVersion: () => ({
    data: {
      version: 'v0.1.0',
      commit: 'abc1234',
      build_date: '2026-06-22T00:00:00Z',
      go_version: 'go1.26.3',
    },
  }),
}))

describe('Admin hub', () => {
  const cases: Array<[string, string]> = [
    ['Farms', '/farms'],
    ['Queues', '/queues'],
    ['Usage Pools', '/usage-pools'],
    ['Storage', '/storage-locations'],
    ['Compute', '/compute-locations'],
    ['Products', '/products'],
    ['Server Log', '/server-log'],
  ]

  it.each(cases)('renders a %s card linking to %s', (label, href) => {
    render(
      <MemoryRouter>
        <Admin />
      </MemoryRouter>,
    )
    const card = screen.getByRole('link', { name: new RegExp(label, 'i') })
    expect(card.getAttribute('href')).toBe(href)
  })

  it('shows the server version and commit', () => {
    render(
      <MemoryRouter>
        <Admin />
      </MemoryRouter>,
    )
    expect(screen.getByLabelText('Server version')).toHaveTextContent('sqi-server v0.1.0 (abc1234)')
  })
})
