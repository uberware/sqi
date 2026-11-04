// SPDX-License-Identifier: AGPL-3.0-or-later

import PageHeader from '@/components/PageHeader'
import ErrorBanner from '@/components/ErrorBanner'
import { useServers } from '@/api/servers'
import type { ServerRole } from '@/api/types'
import styles from './Servers.module.css'

const ROLE_LABELS: Record<ServerRole, string> = {
  leader: 'Leader',
  standby: 'Standby',
  missing: 'Missing',
}

/** "2 s ago" / "3 min ago" / "5 h ago" for a count of seconds. */
function formatSecondsAgo(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  if (s < 60) return `${s} s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m} min ago`
  return `${Math.floor(m / 60)} h ago`
}

function RoleBadge({ role }: { role: ServerRole }) {
  return <span className={`${styles.badge} ${styles[role]}`}>{ROLE_LABELS[role]}</span>
}

export default function Servers() {
  const { data, isLoading, isError, error } = useServers()
  const servers = data?.servers ?? []

  return (
    <div className={styles.page}>
      <PageHeader
        title="Servers"
        backTo="/admin"
        subtitle={
          isLoading
            ? 'Loading…'
            : `${servers.length} ${servers.length === 1 ? 'server' : 'servers'}`
        }
      />

      {isError && (
        <ErrorBanner>
          Failed to load servers: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      )}

      {data && !data.ha && (
        <p className={styles.note}>High availability is off. This server runs alone.</p>
      )}
      {data?.ha && <p className={styles.note}>Leadership epoch {data.epoch}</p>}

      <div className={styles.tableWrap}>
        <table className={styles.table} aria-label="Servers">
          <thead>
            <tr>
              <th>Name</th>
              <th>Role</th>
              <th>Host</th>
              <th>Version</th>
              <th>Last seen</th>
              <th>Broker</th>
            </tr>
          </thead>
          <tbody>
            {isLoading && (
              <tr className={styles.emptyRow}>
                <td colSpan={6}>Loading…</td>
              </tr>
            )}
            {servers.map((server) => (
              <tr key={server.instance_id}>
                <td>{server.name}</td>
                <td>
                  <RoleBadge role={server.role} />
                </td>
                <td>{server.host}</td>
                <td>{server.version}</td>
                <td>{formatSecondsAgo(server.last_seen_seconds_ago)}</td>
                <td className={server.broker_ok ? undefined : styles.brokerDown}>
                  {server.broker_ok ? 'ok' : 'unreachable'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
