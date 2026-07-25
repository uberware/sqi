// SPDX-License-Identifier: AGPL-3.0-or-later

import { NavLink } from 'react-router'
import ConnectionStatusBadge from '@/components/ConnectionStatusBadge'
import ThemeToggle from '@/components/layout/ThemeToggle'
import { useAuth } from '@/auth/context'
import { can, type Permission } from '@/auth/policy'
import { hasAnyAdminAccess } from '@/pages/Admin'
import { useLogout } from '@/api/mutations'
import type { Principal } from '@/api/types'
import styles from './Sidebar.module.css'

interface NavItem {
  label: string
  to: string
  permission?: Permission
}

const PHASE1_NAV: NavItem[] = [
  { label: 'Dashboard', to: '/' },
  { label: 'Submit', to: '/submit', permission: 'jobs.write' },
  { label: 'Jobs', to: '/jobs', permission: 'jobs.read' },
  { label: 'Workers', to: '/workers', permission: 'workers.read' },
  { label: 'Admin', to: '/admin' },
]

/** Nav items visible to `principal` — permission-gated, plus Admin iff any admin card is visible. */
function visibleNavItems(principal: Principal | null): NavItem[] {
  return PHASE1_NAV.filter((item) => {
    if (item.to === '/admin') return hasAnyAdminAccess(principal)
    return !item.permission || can(principal, item.permission)
  })
}

/**
 * Signed-in identity + logout action. Hidden entirely when the resolved
 * principal is the anonymous one auth-off deployments return from
 * `GET /auth/me` — that's the binding auth-off regression guarantee: an
 * auth-disabled server must render the sidebar exactly as it did before
 * this feature, with no logout control to click.
 */
function AccountSection() {
  const { principal, status } = useAuth()
  const logout = useLogout()

  // `status !== 'authed'` is checked alongside the principal itself because
  // a background refetch of /auth/me (e.g. right after logout invalidates
  // it) can fail while the query still returns its last-known-good `data` —
  // see the comment in auth/context.tsx. Relying on `status` here avoids a
  // stale logout button lingering for the instant between the 401 landing
  // and the app shell swapping to the login page.
  if (status !== 'authed' || !principal || principal.kind === 'anonymous') return null

  // useLogout's onSuccess already invalidates the auth.me query, which
  // re-triggers AuthProvider's active useAuthMe query and flips `status` to
  // 'anon' once it resolves — sufficient on its own to swap the app back to
  // the login page, so no extra useAuth().refresh() call is needed here.
  const handleLogout = () => {
    logout.mutate()
  }

  return (
    <div className={styles.accountSection}>
      <span className={styles.accountName}>{principal.display_name}</span>
      <div className={styles.accountActions}>
        {/*
         * Every authenticated role may manage its own account, so this link
         * is ungated. It inherits the anonymous check above: with auth off
         * the whole section is absent, matching the self-service routes,
         * which return 409 when there is no real account to change.
         */}
        <NavLink to="/account" className={styles.accountLink ?? ''}>
          Account
        </NavLink>
        <button
          type="button"
          className={styles.logoutBtn}
          onClick={handleLogout}
          disabled={logout.isPending}
        >
          Log out
        </button>
      </div>
    </div>
  )
}

export default function Sidebar() {
  const { principal } = useAuth()
  const navItems = visibleNavItems(principal)
  return (
    <nav className={styles.sidebar} aria-label="Primary navigation">
      <div className={styles.logoArea}>
        <span className={styles.logoText}>sqi</span>
        <div className={styles.statusArea}>
          <ConnectionStatusBadge />
        </div>
      </div>

      <ul className={styles.navSection} role="list">
        {navItems.map((item) => (
          <li key={item.to}>
            <NavLink
              to={item.to}
              end={item.to === '/'}
              className={({ isActive }) =>
                [styles.navLink, isActive && styles.activeLink]
                  .filter((c): c is string => Boolean(c))
                  .join(' ')
              }
            >
              {item.label}
            </NavLink>
          </li>
        ))}
      </ul>

      <div className={styles.spacer} />

      <AccountSection />

      <div className={styles.divider} />

      <ThemeToggle />
    </nav>
  )
}
