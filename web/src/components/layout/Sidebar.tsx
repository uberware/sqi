// SPDX-License-Identifier: AGPL-3.0-or-later

import { NavLink } from 'react-router-dom'
import ConnectionStatusBadge from '@/components/ConnectionStatusBadge'
import ThemeToggle from '@/components/layout/ThemeToggle'
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
  { label: 'Farms', to: '/farms' },
  { label: 'Queues', to: '/queues' },
  { label: 'Usage Pools', to: '/usage-pools' },
  { label: 'Storage', to: '/storage-locations' },
  { label: 'Compute', to: '/compute-locations' },
  { label: 'Products', to: '/products' },
  { label: 'Admin', to: '/admin' },
]

// Labels only — inert "coming soon" stubs for views not yet implemented.
const DEFERRED_LABELS = ['Presets']

function DeferredItem({ label }: { label: string }) {
  return (
    <span className={styles.deferredItem} data-disabled="true">
      {label}
      <span className={styles.comingSoonBadge}>coming soon</span>
    </span>
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

      <div className={styles.divider} />

      <ul className={styles.navSection} role="list">
        {DEFERRED_LABELS.map((label) => (
          <li key={label}>
            <DeferredItem label={label} />
          </li>
        ))}
      </ul>

      <div className={styles.spacer} />

      <div className={styles.divider} />

      <ThemeToggle />
    </nav>
  )
}
