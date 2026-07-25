// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router'
import Admin from '@/pages/Admin'
import type { Principal } from '@/api/types'

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

// Admin filters its cards by permission — default the mocked principal to
// admin (holds every permission the cards check) so pre-existing assertions
// keep working unchanged. Role-gating itself is covered by focused tests below.
const ADMIN_PRINCIPAL: Principal = {
  subject: 'u-admin',
  display_name: 'Admin',
  roles: ['admin'],
  kind: 'user',
  permissions: ADMIN_PERMISSIONS,
}

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(() => ({ principal: ADMIN_PRINCIPAL, status: 'authed', refresh: () => {} })),
}))
import { useAuth } from '@/auth/context'
import { ADMIN_PERMISSIONS, OPERATOR_PERMISSIONS, READ_ONLY_PERMISSIONS } from '@/test/principals'

function setPrincipal(principal: Principal) {
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal,
    status: 'authed',
    refresh: () => {},
  })
}

describe('Admin hub', () => {
  const cases: Array<[string, string]> = [
    ['Farms', '/farms'],
    ['Queues', '/queues'],
    ['Usage Pools', '/usage-pools'],
    ['Storage', '/storage-locations'],
    ['Locations', '/compute-locations'],
    ['Products', '/products'],
    ['Server Log', '/server-log'],
  ]

  it.each(cases)('renders a %s card linking to %s', (label, href) => {
    render(
      <MemoryRouter>
        <Admin />
      </MemoryRouter>,
    )
    // Anchor at the start so a label (e.g. "Locations") doesn't also match
    // another card whose description happens to contain the word.
    const card = screen.getByRole('link', { name: new RegExp('^' + label, 'i') })
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

  describe('role gating', () => {
    it('hides every card for a read-only principal', () => {
      setPrincipal({
        subject: 's',
        display_name: 'n',
        roles: ['read-only'],
        kind: 'user',
        permissions: READ_ONLY_PERMISSIONS,
      })
      render(
        <MemoryRouter>
          <Admin />
        </MemoryRouter>,
      )
      for (const [label] of cases) {
        expect(
          screen.queryByRole('link', { name: new RegExp('^' + label, 'i') }),
        ).not.toBeInTheDocument()
      }
      // read-only does hold apikeys.self, so the API Keys card still shows.
      expect(screen.getByRole('link', { name: /^API Keys/i })).toBeInTheDocument()
    })

    it('hides the Users card for an operator (users.read is admin-only)', () => {
      setPrincipal({
        subject: 's',
        display_name: 'n',
        roles: ['operator'],
        kind: 'user',
        permissions: OPERATOR_PERMISSIONS,
      })
      render(
        <MemoryRouter>
          <Admin />
        </MemoryRouter>,
      )
      expect(screen.queryByRole('link', { name: /^Users/i })).not.toBeInTheDocument()
      // ...but operator does hold infra.manage, so e.g. Farms still shows.
      expect(screen.getByRole('link', { name: /^Farms/i })).toBeInTheDocument()
    })
  })
})
