// SPDX-License-Identifier: AGPL-3.0-or-later

import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import RequireRole from './RequireRole'
import type { Principal } from '@/api/types'

vi.mock('@/auth/context', () => ({
  useAuth: vi.fn(),
}))
import { useAuth } from '@/auth/context'

function setPrincipal(p: Principal | null) {
  ;(useAuth as unknown as ReturnType<typeof vi.fn>).mockReturnValue({
    principal: p,
    status: p ? 'authed' : 'anon',
    refresh: () => {},
  })
}

describe('RequireRole', () => {
  it('renders children when permitted', () => {
    setPrincipal({ subject: 's', display_name: 'n', roles: ['admin'], kind: 'user' } as Principal)
    render(
      <RequireRole permission="users.manage">
        <div>secret</div>
      </RequireRole>,
    )
    expect(screen.getByText('secret')).toBeInTheDocument()
  })

  it('renders a 403 when denied', () => {
    setPrincipal({ subject: 's', display_name: 'n', roles: ['user'], kind: 'user' } as Principal)
    render(
      <RequireRole permission="users.manage">
        <div>secret</div>
      </RequireRole>,
    )
    expect(screen.queryByText('secret')).not.toBeInTheDocument()
    expect(screen.getByText(/not authorized|forbidden|403/i)).toBeInTheDocument()
  })
})
