// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import DiagnosticsPanel from '@/components/DiagnosticsPanel'
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

interface AdminSection {
  id: string
  label: string
  /** When true, the section grows to fill the remaining page height. */
  fill?: boolean
  render: () => ReactNode
}

// Extensible registry — add future admin tools (settings, licensing, etc.) here.
const SECTIONS: AdminSection[] = [
  {
    id: 'server-log',
    label: 'Server log',
    fill: true,
    render: () => <DiagnosticsPanel component="server" title="Server log" fill />,
  },
]

export default function Admin() {
  return (
    <div className={styles.page}>
      <h1 className={styles.heading}>Admin</h1>
      <ServerVersion />
      {SECTIONS.map((s) => (
        <section
          key={s.id}
          className={s.fill ? `${styles.section} ${styles.sectionFill}` : styles.section}
          aria-label={s.label}
        >
          {s.render()}
        </section>
      ))}
    </div>
  )
}
