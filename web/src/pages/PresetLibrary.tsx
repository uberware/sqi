// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import { usePresets, queryKeys, fetchPresets } from '@/api/queries'
import { ApiError } from '@/api/client'
import styles from './PresetLibrary.module.css'

const STATUS_LABEL: Record<string, string> = {
  not_installed: 'Not installed',
  installed: 'Installed',
  update_available: 'Update available',
}

export default function PresetLibrary() {
  const { data, isLoading, isError, error } = usePresets()
  const qc = useQueryClient()
  const { showToast } = useToast()

  const notConfigured = isError && error instanceof ApiError && error.status === 503

  const handleRefresh = async () => {
    try {
      await qc.fetchQuery({ queryKey: queryKeys.presets.all, queryFn: () => fetchPresets(true) })
    } catch (e) {
      if (!(e instanceof ApiError && e.status === 503)) {
        showToast(e instanceof Error ? e.message : 'Failed to refresh presets', 'error')
      }
    }
  }

  return (
    <div className={styles.page}>
      <PageHeader
        title="Preset Library"
        backTo="/admin"
        action={
          <button type="button" onClick={() => void handleRefresh()} className={styles.refreshBtn}>
            Refresh
          </button>
        }
      />

      {notConfigured ? (
        <div className={styles.emptyState}>
          <p>
            No preset library configured. Set <code>preset_library.url</code> in your server
            configuration to enable browsing.
          </p>
        </div>
      ) : isError ? (
        <div className={styles.errorBanner} role="alert">
          Failed to load presets: {error instanceof Error ? error.message : 'Unknown error'}
        </div>
      ) : (
        <div className={styles.tableWrap}>
          <table className={styles.table} aria-label="Presets">
            <thead>
              <tr>
                <th>Title</th>
                <th>Category</th>
                <th>Version</th>
                <th>Status</th>
              </tr>
            </thead>
            <tbody>
              {isLoading && (
                <tr className={styles.emptyRow}>
                  <td colSpan={4}>Loading…</td>
                </tr>
              )}
              {!isLoading && (!data || data.length === 0) && (
                <tr className={styles.emptyRow}>
                  <td colSpan={4}>No presets available yet.</td>
                </tr>
              )}
              {data?.map((p) => (
                <tr key={p.name}>
                  <td>
                    <Link to={`/presets/${encodeURIComponent(p.name)}`} className={styles.linkBtn}>
                      {p.title}
                    </Link>
                  </td>
                  <td>{p.category || '—'}</td>
                  <td>{p.version || '—'}</td>
                  <td>
                    <span className={styles.badge} data-status={p.status}>
                      {STATUS_LABEL[p.status] ?? p.status}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}
