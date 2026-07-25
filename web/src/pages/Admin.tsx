// SPDX-License-Identifier: AGPL-3.0-or-later
/* eslint-disable react-refresh/only-export-components -- also exports the
   visibleAdminLinks/hasAnyAdminAccess helpers Sidebar reuses to keep its Admin
   nav entry in lockstep with this page's card visibility */

import { Link } from 'react-router'
import { useVersion } from '@/api/queries'
import { useAuth } from '@/auth/context'
import { can, type Permission } from '@/auth/policy'
import type { Principal } from '@/api/types'
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
  permission: Permission
}

// Extensible registry — add future admin destinations (settings, licensing, …) here.
const ADMIN_LINKS: AdminLink[] = [
  {
    id: 'farms',
    label: 'Farms',
    description: 'Render farms and their defaults',
    to: '/farms',
    permission: 'infra.manage',
  },
  {
    id: 'queues',
    label: 'Queues',
    description: 'Job queues and scheduling priority',
    to: '/queues',
    permission: 'infra.manage',
  },
  {
    id: 'usage',
    label: 'Usage Pools',
    description: 'License / concurrency usage pools',
    to: '/usage-pools',
    permission: 'infra.manage',
  },
  {
    id: 'storage',
    label: 'Storage',
    description: 'Named storage locations and roots',
    to: '/storage-locations',
    permission: 'infra.manage',
  },
  {
    id: 'compute',
    label: 'Locations',
    description: 'Compute locations and worker affinity',
    to: '/compute-locations',
    permission: 'infra.manage',
  },
  {
    id: 'products',
    label: 'Products',
    description: 'Product definitions and presets',
    to: '/products',
    permission: 'products.manage',
  },
  {
    id: 'presets',
    label: 'Preset Library',
    description: 'Browse and install community presets',
    to: '/presets',
    permission: 'products.manage',
  },
  {
    id: 'log',
    label: 'Server Log',
    description: 'Live server diagnostic log',
    to: '/server-log',
    permission: 'diagnostics.read',
  },
  {
    id: 'users',
    label: 'Users',
    description: 'User accounts and roles',
    to: '/users',
    permission: 'users.read',
  },
  {
    id: 'api-keys',
    label: 'API Keys',
    description: 'Personal API keys for headless SDK/submitter access',
    to: '/api-keys',
    permission: 'apikeys.self',
  },
]

/** Admin hub cards the given principal is permitted to see. */
export function visibleAdminLinks(principal: Principal | null): AdminLink[] {
  return ADMIN_LINKS.filter((link) => can(principal, link.permission))
}

/** True when at least one Admin-hub card is visible — drives the Sidebar's Admin entry. */
export function hasAnyAdminAccess(principal: Principal | null): boolean {
  return visibleAdminLinks(principal).length > 0
}

export default function Admin() {
  const { principal } = useAuth()
  const links = visibleAdminLinks(principal)
  return (
    <div className={styles.page}>
      <h1 className={styles.heading}>Admin</h1>
      <ServerVersion />
      <nav className={styles.grid} aria-label="Admin sections">
        {links.map((link) => (
          <Link key={link.id} to={link.to} className={styles.card}>
            <span className={styles.cardLabel}>{link.label}</span>
            <span className={styles.cardDescription}>{link.description}</span>
          </Link>
        ))}
      </nav>
    </div>
  )
}
