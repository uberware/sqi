// SPDX-License-Identifier: AGPL-3.0-or-later

import { NavLink } from 'react-router-dom'
import ConnectionStatusBadge from '@/components/ConnectionStatusBadge'
import ThemeToggle from '@/components/layout/ThemeToggle'
import { useAuth } from '@/auth/context'
import { useLogout } from '@/api/mutations'
import styles from './Sidebar.module.css'

interface NavItem {
  label: string
  to: string
}

const PHASE1_NAV: NavItem[] = [
  { label: 'Dashboard', to: '/' },
  { label: 'Submit', to: '/submit' },
  { label: 'Jobs', to: '/jobs' },
  { label: 'Workers', to: '/workers' },
  { label: 'Admin', to: '/admin' },
]

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
      <button
        type="button"
        className={styles.logoutBtn}
        onClick={handleLogout}
        disabled={logout.isPending}
      >
        Log out
      </button>
    </div>
  )
}

export default function Sidebar() {
  return (
    <nav className={styles.sidebar} aria-label="Primary navigation">
      <div className={styles.logoArea}>
        <span className={styles.logoText}>sqi</span>
        <div className={styles.statusArea}>
          <ConnectionStatusBadge />
        </div>
      </div>

      <ul className={styles.navSection} role="list">
        {PHASE1_NAV.map((item) => (
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
