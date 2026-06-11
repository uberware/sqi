// SPDX-License-Identifier: AGPL-3.0-or-later

import { NavLink } from 'react-router-dom'
import styles from './Sidebar.module.css'

interface NavItem {
  label: string
  to: string
}

const PHASE1_NAV: NavItem[] = [
  { label: 'Dashboard', to: '/' },
  { label: 'Jobs', to: '/jobs' },
  { label: 'Workers', to: '/workers' },
  { label: 'Farms', to: '/farms' },
  { label: 'Queues', to: '/queues' },
  { label: 'Submit', to: '/submit' },
]

// Labels only — inert "coming soon" stubs for views not yet implemented.
const DEFERRED_LABELS = ['Presets', 'Products', 'Storage', 'License Pools', 'Settings', 'Admin']

export default function Sidebar() {
  return (
    <nav className={styles.sidebar} aria-label="Primary navigation">
      <div className={styles.logoArea}>
        <span className={styles.logoText}>sqi</span>
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

      <div className={styles.divider} />

      <ul className={styles.navSection} role="list">
        {DEFERRED_LABELS.map((label) => (
          <li key={label}>
            <span className={styles.deferredItem} data-disabled="true">
              {label}
              <span className={styles.comingSoonBadge}>coming soon</span>
            </span>
          </li>
        ))}
      </ul>
    </nav>
  )
}
