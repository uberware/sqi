// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQueryClient } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import PageHeader from '@/components/PageHeader'
import { useToast } from '@/components/Toast'
import DebouncedSearchInput from '@/components/DebouncedSearchInput'
import ErrorBanner from '@/components/ErrorBanner'
import { useSearchParam } from '@/hooks/useSearchParam'
import { filterBySearch } from '@/utils/filterBySearch'
import { usePresets, queryKeys, fetchPresets } from '@/api/queries'
import { ApiError } from '@/api/client'
import type { PresetListItem } from '@/api/types'
import styles from './PresetLibrary.module.css'

const STATUS_LABEL: Record<string, string> = {
  not_installed: 'Not installed',
  installed: 'Installed',
  update_available: 'Update available',
}

const UNCATEGORIZED = 'Uncategorized'

// groupByCategory buckets presets by their category (empty → "Uncategorized")
// and returns the buckets sorted alphabetically by category name.
function groupByCategory(presets: PresetListItem[]): [string, PresetListItem[]][] {
  const groups = new Map<string, PresetListItem[]>()
  for (const p of presets) {
    const category = p.category || UNCATEGORIZED
    const bucket = groups.get(category)
    if (bucket) bucket.push(p)
    else groups.set(category, [p])
  }
  return [...groups.entries()].sort(([a], [b]) =>
    a.localeCompare(b, undefined, { sensitivity: 'base' }),
  )
}

export default function PresetLibrary() {
  const { data, isLoading, isError, error } = usePresets()
  const qc = useQueryClient()
  const { showToast } = useToast()
  const { search, setSearch } = useSearchParam()

  const notConfigured = isError && error instanceof ApiError && error.status === 503
  const filtered = filterBySearch(data ?? [], search, (p) => [
    p.name,
    p.title,
    p.description,
    p.category,
  ])

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
        <ErrorBanner>
          Failed to load presets: {error instanceof Error ? error.message : 'Unknown error'}
        </ErrorBanner>
      ) : isLoading ? (
        <div className={styles.emptyState}>Loading…</div>
      ) : !data || data.length === 0 ? (
        <div className={styles.emptyState}>No presets available yet.</div>
      ) : (
        <>
          <div className={styles.toolbar}>
            <DebouncedSearchInput
              value={search}
              onChange={setSearch}
              placeholder="Search presets…"
              aria-label="Search presets"
            />
          </div>
          {filtered.length === 0 ? (
            <div className={styles.emptyState}>No presets match “{search}”.</div>
          ) : (
            <div className={styles.groups}>
              {groupByCategory(filtered).map(([category, presets]) => (
                <section className={styles.group} key={category}>
                  <div className={styles.groupHeading}>
                    <h2 className={styles.groupLabel}>{category}</h2>
                    <hr className={styles.rule} />
                  </div>
                  <table className={styles.table} aria-label={`${category} presets`}>
                    <thead>
                      <tr>
                        <th>Title</th>
                        <th>Version</th>
                        <th>Status</th>
                      </tr>
                    </thead>
                    <tbody>
                      {presets.map((p) => (
                        <tr key={p.name}>
                          <td>
                            <Link
                              to={`/presets/${encodeURIComponent(p.name)}`}
                              className={styles.linkBtn}
                            >
                              {p.title}
                            </Link>
                          </td>
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
                </section>
              ))}
            </div>
          )}
        </>
      )}
    </div>
  )
}
