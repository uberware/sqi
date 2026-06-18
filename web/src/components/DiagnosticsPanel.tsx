// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useState } from 'react'
import { useDiagnosticsLogs, type DiagRecord } from '@/api/diagnostics'
import { useWebSocket } from '@/ws/context'
import { isDiagEvent } from '@/ws/diagnostics'
import styles from './DiagnosticsPanel.module.css'

interface Props {
  /** Component filter, e.g. "server" or "worker:w1". */
  component: string
  /** Heading shown above the log lines. */
  title: string
  /** Optional task_id filter (used by the task-detail fallback). */
  taskId?: string
}

export default function DiagnosticsPanel({ component, title, taskId }: Props) {
  const params = taskId ? { component, task_id: taskId } : { component }
  const { data, isLoading, isError } = useDiagnosticsLogs(params)
  const [live, setLive] = useState<DiagRecord[]>([])

  const onMessage = useCallback(
    (payload: unknown) => {
      if (!isDiagEvent(payload)) return
      if (payload.component !== component) return
      if (taskId && payload.attrs?.['task_id'] !== taskId) return
      setLive((prev) => [
        ...prev.slice(-499),
        {
          ts: payload.at,
          component: payload.component,
          level: payload.level,
          msg: payload.msg,
          ...(payload.attrs ? { attrs: payload.attrs } : {}),
        },
      ])
    },
    [component, taskId],
  )
  useWebSocket('diagnostics', onMessage)

  const records = [...(data?.records ?? []), ...live]

  return (
    <section className={styles.panel} aria-label={title}>
      <h3 className={styles.title}>{title}</h3>
      {isLoading && <p className={styles.muted}>Loading…</p>}
      {isError && <p className={styles.muted}>Diagnostics unavailable.</p>}
      {!isLoading && !isError && records.length === 0 && (
        <p className={styles.muted}>No diagnostic logs.</p>
      )}
      {records.length > 0 && (
        <ol className={styles.lines} role="log">
          {records.map((r, i) => (
            <li key={`${r.ts}-${i}`} className={styles.line}>
              <time className={styles.ts} dateTime={r.ts}>
                {new Date(r.ts).toLocaleTimeString()}
              </time>
              <span className={styles.level} data-level={r.level}>
                {r.level}
              </span>
              <span className={styles.msg}>{r.msg}</span>
            </li>
          ))}
        </ol>
      )}
    </section>
  )
}
