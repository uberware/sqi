// SPDX-License-Identifier: AGPL-3.0-or-later

import { Link } from 'react-router-dom'
import { useVersion } from '@/api/queries'
import styles from './Admin.module.css'

/** Shows the running server's version and commit beneath the page heading. */
function ServerVersion() {
  const { data } = useVersion()
  if (!data) return null
  const commit = data.commit && data.commit !== 'unknown' ? ` (${data.commit})` : ''
  return (
    <p className={styles.version} aria-label="Server version">
      sqi-server {data.version}
      {commit}
    </p>
  )
}

interface AdminLink {
  id: string
  label: string
  description: string
  to: string
}

// Extensible registry — add future admin destinations (settings, licensing, …) here.
const ADMIN_LINKS: AdminLink[] = [
  { id: 'farms', label: 'Farms', description: 'Render farms and their defaults', to: '/farms' },
  {
    id: 'queues',
    label: 'Queues',
    description: 'Job queues and scheduling priority',
    to: '/queues',
  },
  {
    id: 'usage',
    label: 'Usage Pools',
    description: 'License / concurrency usage pools',
    to: '/usage-pools',
  },
  {
    id: 'storage',
    label: 'Storage',
    description: 'Named storage locations and roots',
    to: '/storage-locations',
  },
  {
    id: 'compute',
    label: 'Locations',
    description: 'Compute locations and worker affinity',
    to: '/compute-locations',
  },
  {
    id: 'products',
    label: 'Products',
    description: 'Product definitions and presets',
    to: '/products',
  },
  {
    id: 'presets',
    label: 'Preset Library',
    description: 'Browse and install community presets',
    to: '/presets',
  },
  { id: 'log', label: 'Server Log', description: 'Live server diagnostic log', to: '/server-log' },
  { id: 'users', label: 'Users', description: 'User accounts and roles', to: '/users' },
]

export default function Admin() {
  return (
    <div className={styles.page}>
      <h1 className={styles.heading}>Admin</h1>
      <ServerVersion />
      <nav className={styles.grid} aria-label="Admin sections">
        {ADMIN_LINKS.map((link) => (
          <Link key={link.id} to={link.to} className={styles.card}>
            <span className={styles.cardLabel}>{link.label}</span>
            <span className={styles.cardDescription}>{link.description}</span>
          </Link>
        ))}
      </nav>
    </div>
  )
}
