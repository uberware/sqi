// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useLayoutEffect, useRef, useState } from 'react'
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
  /**
   * When true, the panel grows to fill its flex parent (used on the Admin page
   * where the server log is the only content). When false (default), the log
   * list is capped at a fixed height.
   */
  fill?: boolean
}

// How close to the bottom (in px) still counts as "at the bottom" for the
// auto-follow behavior. A small tolerance absorbs sub-pixel rounding.
const BOTTOM_THRESHOLD_PX = 16

// orderedAttrs returns a record's attributes as key=value entries with `error`
// first (it carries the failure detail operators care about most), then the
// rest alphabetically for stable rendering.
function orderedAttrs(attrs: Record<string, string> | undefined): Array<[string, string]> {
  if (!attrs) return []
  return Object.entries(attrs).sort(([a], [b]) => {
    if (a === b) return 0
    if (a === 'error') return -1
    if (b === 'error') return 1
    return a.localeCompare(b)
  })
}

export default function DiagnosticsPanel({ component, title, taskId, fill = false }: Props) {
  // An empty component means "all components" — omit it from the query so the
  // backend returns diagnostics across every component (used by the task-detail
  // fallback when the failing task has no recorded worker id).
  const params: { component?: string; task_id?: string } = {}
  if (component) params.component = component
  if (taskId) params.task_id = taskId
  const { data, isLoading, isError } = useDiagnosticsLogs(params)
  const [live, setLive] = useState<DiagRecord[]>([])

  const onMessage = useCallback(
    (payload: unknown) => {
      if (!isDiagEvent(payload)) return
      // Skip the component-equality check when filtering across all components.
      if (component && payload.component !== component) return
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

  // Auto-follow the tail: scroll to the newest line when records change, but
  // only while the viewer is already at (or near) the bottom. If the user has
  // scrolled up to read history, leave their position untouched.
  const listRef = useRef<HTMLOListElement>(null)
  const stickToBottomRef = useRef(true)

  const handleScroll = useCallback(() => {
    const el = listRef.current
    if (!el) return
    const distanceFromBottom = el.scrollHeight - el.scrollTop - el.clientHeight
    stickToBottomRef.current = distanceFromBottom <= BOTTOM_THRESHOLD_PX
  }, [])

  useLayoutEffect(() => {
    const el = listRef.current
    if (el && stickToBottomRef.current) {
      el.scrollTop = el.scrollHeight
    }
  }, [records.length])

  return (
    <section className={fill ? `${styles.panel} ${styles.fill}` : styles.panel} aria-label={title}>
      <h3 className={styles.title}>{title}</h3>
      {isLoading && <p className={styles.muted}>Loading…</p>}
      {isError && <p className={styles.muted}>Diagnostics unavailable.</p>}
      {!isLoading && !isError && records.length === 0 && (
        <p className={styles.muted}>No diagnostic logs.</p>
      )}
      {records.length > 0 && (
        <ol
          ref={listRef}
          onScroll={handleScroll}
          className={fill ? `${styles.lines} ${styles.fillLines}` : styles.lines}
          role="log"
        >
          {records.map((r, i) => (
            <li key={`${r.ts}-${i}`} className={styles.line}>
              <time className={styles.ts} dateTime={r.ts}>
                {new Date(r.ts).toLocaleTimeString()}
              </time>
              <span className={styles.level} data-level={r.level}>
                {r.level}
              </span>
              <span className={styles.msg}>
                <span className={styles.msgText}>{r.msg}</span>
                {orderedAttrs(r.attrs).map(([k, v]) => (
                  <span key={k} className={styles.attr} data-attr-key={k}>
                    <span className={styles.attrKey}>{k}</span>
                    <span className={styles.attrSep}>=</span>
                    <span className={styles.attrVal}>{v}</span>
                  </span>
                ))}
              </span>
            </li>
          ))}
        </ol>
      )}
    </section>
  )
}
