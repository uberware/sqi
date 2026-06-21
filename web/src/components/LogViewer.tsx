// SPDX-License-Identifier: AGPL-3.0-or-later

import { useEffect, useRef, useState, memo, useLayoutEffect } from 'react'
import AnsiToHtml from 'ansi-to-html'
import { Check } from '@/components/icons'
import { fetchTaskLogs } from '@/api/queries'
import { useWebSocket } from '@/ws/context'
import type { TaskLog, TaskStatus } from '@/api/types'
import styles from './LogViewer.module.css'

// ── ANSI converter ────────────────────────────────────────────────────────

const ansiConverter = new AnsiToHtml({ escapeXML: true, stream: false })

function toHtmlSafe(raw: string): string {
  try {
    return ansiConverter.toHtml(raw)
  } catch {
    return raw.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
  }
}

function stripAnsi(str: string): string {
  // eslint-disable-next-line no-control-regex -- intentional ANSI strip
  return str.replace(/\x1b\[[0-9;]*[a-zA-Z]/g, '')
}

// ── Internal line representation ──────────────────────────────────────────

interface LogLine {
  key: string
  lineNumber: number
  stream: 'stdout' | 'stderr'
  html: string
  plain: string
}

function chunkToLines(chunk: TaskLog, baseLineNumber: number): LogLine[] {
  const rawLines = chunk.data.split('\n')
  const trimmed =
    rawLines.length > 1 && rawLines[rawLines.length - 1] === '' ? rawLines.slice(0, -1) : rawLines

  return trimmed.map((raw, i) => ({
    key: `${chunk.id}:${i}`,
    lineNumber: baseLineNumber + i,
    stream: chunk.stream,
    html: toHtmlSafe(raw),
    plain: stripAnsi(raw),
  }))
}

function chunksToLines(chunks: TaskLog[]): LogLine[] {
  let lineNumber = 1
  const result: LogLine[] = []
  for (const chunk of chunks) {
    const chunkLines = chunkToLines(chunk, lineNumber)
    for (const l of chunkLines) result.push(l)
    lineNumber += chunkLines.length
  }
  return result
}

// ── LogLineRow ─────────────────────────────────────────────────────────────

interface LogLineRowProps {
  line: LogLine
}

const LogLineRow = memo(function LogLineRow({ line }: LogLineRowProps) {
  return (
    <div
      className={`${styles.logLine} ${line.stream === 'stderr' ? styles.streamStderr : styles.streamStdout}`}
    >
      <span className={styles.gutter}>{line.lineNumber}</span>
      <span
        className={styles.logText}
        // Content comes from the server's log store (trusted), converted by ansi-to-html
        // which emits only <span style="color:..."> wrappers — XSS surface is minimal.
        dangerouslySetInnerHTML={{ __html: line.html }}
      />
    </div>
  )
})

// ── WebSocket log push type (matches ws.TaskLogPush on the server) ─────────

interface TaskLogPush {
  task_id: string
  attempt_id?: string
  seq_num: number
  stream: string
  data: string
  at: string
}

// ── LogViewer ──────────────────────────────────────────────────────────────

const INITIAL_LIMIT = 500

export interface LogViewerProps {
  taskId: string
  taskStatus: TaskStatus
}

export default function LogViewer({ taskId, taskStatus }: LogViewerProps) {
  const isLive = taskStatus === 'running' || taskStatus === 'assigned'

  // ── Auto-scroll state — declared first so WS handler can reference ────────

  const [autoScroll, setAutoScroll] = useState(true)
  const [unreadCount, setUnreadCount] = useState(0)

  // ── Chunks ────────────────────────────────────────────────────────────────

  const [chunks, setChunks] = useState<TaskLog[]>([])
  const [afterNatsSeq, setAfterNatsSeq] = useState(0)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)

  // Initial fetch: runs whenever taskId changes (component is re-keyed by parent).
  useEffect(() => {
    let cancelled = false
    fetchTaskLogs({ taskId, afterNatsSeq: 0, limit: INITIAL_LIMIT })
      .then((resp) => {
        if (cancelled) return
        setChunks(resp.items)
        const last = resp.items[resp.items.length - 1]
        setAfterNatsSeq(last !== undefined ? last.nats_seq : 0)
        setHasMore(resp.items.length >= INITIAL_LIMIT)
        setLoading(false)
      })
      .catch((err: unknown) => {
        if (cancelled) return
        setLoadError(err instanceof Error ? err.message : 'Failed to load logs')
        setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [taskId])

  // ── Live log chunks via WebSocket ─────────────────────────────────────────

  // Sync latest autoScroll value into a ref so the WS handler always reads current state.
  const autoScrollRef = useRef(autoScroll)
  useLayoutEffect(() => {
    autoScrollRef.current = autoScroll
  })

  useWebSocket(`tasks/${taskId}/logs`, (payload) => {
    if (!isLive) return
    const push = payload as TaskLogPush
    if (!push || typeof push.data !== 'string') return

    const liveChunk: TaskLog = {
      id: `live-${push.seq_num}`,
      task_id: push.task_id,
      attempt_id: push.attempt_id ?? '',
      seq_num: push.seq_num,
      nats_seq: push.seq_num,
      stream: push.stream === 'stderr' ? 'stderr' : 'stdout',
      data: push.data,
      at: push.at,
      received_at: push.at,
    }
    const attemptId = push.attempt_id ?? ''
    setChunks((prev) => {
      // Dedup by (attempt_id, seq_num) so retries (which reset seq_num to 1)
      // are not silently dropped.
      if (prev.some((c) => c.attempt_id === attemptId && c.seq_num === push.seq_num)) return prev
      return [...prev, liveChunk]
    })
    if (!autoScrollRef.current) {
      // Count actual rendered lines, not just WS messages.
      const newLineCount = chunkToLines(liveChunk, 0).length
      setUnreadCount((n) => n + newLineCount)
    }
  })

  // ── Load more pages ───────────────────────────────────────────────────────

  function loadMore() {
    if (loadingMore || !hasMore) return
    setLoadingMore(true)
    fetchTaskLogs({ taskId, afterNatsSeq, limit: INITIAL_LIMIT })
      .then((resp) => {
        setChunks((prev) => {
          // Dedup against chunks already delivered live via WS so we don't
          // render duplicate lines after REST and WS both deliver the same seq.
          const existing = new Set(prev.map((c) => `${c.attempt_id}:${c.nats_seq}`))
          const fresh = resp.items.filter(
            (item) => !existing.has(`${item.attempt_id}:${item.nats_seq}`),
          )
          return [...prev, ...fresh]
        })
        const last = resp.items[resp.items.length - 1]
        setAfterNatsSeq(last !== undefined ? last.nats_seq : afterNatsSeq)
        setHasMore(resp.items.length >= INITIAL_LIMIT)
        setLoadingMore(false)
      })
      .catch((err: unknown) => {
        setLoadError(err instanceof Error ? err.message : 'Failed to load more logs')
        setLoadingMore(false)
      })
  }

  // ── Derived log lines ─────────────────────────────────────────────────────

  const lines = chunksToLines(chunks)

  // ── Scroll behaviour ──────────────────────────────────────────────────────

  const bodyRef = useRef<HTMLDivElement>(null)
  const bottomRef = useRef<HTMLDivElement>(null)

  // Scroll to bottom on new lines while auto-scroll is active.
  useEffect(() => {
    if (autoScroll && bottomRef.current) {
      bottomRef.current.scrollIntoView({ behavior: 'instant' })
    }
  }, [lines.length, autoScroll])

  function handleScroll() {
    const el = bodyRef.current
    if (!el) return
    const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 40
    if (atBottom && !autoScroll) {
      setAutoScroll(true)
      setUnreadCount(0)
    } else if (!atBottom && autoScroll) {
      setAutoScroll(false)
    }
  }

  function jumpToBottom() {
    setAutoScroll(true)
    setUnreadCount(0)
    bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
  }

  function toggleAutoScroll() {
    if (autoScroll) {
      setAutoScroll(false)
    } else {
      jumpToBottom()
    }
  }

  // ── Copy to clipboard ─────────────────────────────────────────────────────

  const [copied, setCopied] = useState(false)
  const copyTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (copyTimerRef.current !== null) clearTimeout(copyTimerRef.current)
    },
    [],
  )

  function handleCopy() {
    const text = lines.map((l) => l.plain).join('\n')
    void navigator.clipboard
      .writeText(text)
      .then(() => {
        setCopied(true)
        if (copyTimerRef.current !== null) clearTimeout(copyTimerRef.current)
        copyTimerRef.current = setTimeout(() => setCopied(false), 1500)
      })
      .catch(() => {
        // Clipboard unavailable in some browser contexts; silently ignore.
      })
  }

  // ── Render ────────────────────────────────────────────────────────────────

  return (
    <div className={styles.panel} role="log" aria-label="Task log output">
      {/* Controls bar */}
      <div className={styles.controls}>
        <div className={styles.controlsLeft}>
          {isLive && (
            <span className={styles.liveIndicator} aria-label="Live updates active">
              <span className={styles.liveDot} aria-hidden="true" />
              Live
            </span>
          )}
          {lines.length > 0 && (
            <span className={styles.lineCount}>
              {lines.length} line{lines.length !== 1 ? 's' : ''}
            </span>
          )}
        </div>
        <div className={styles.controlsRight}>
          <button
            className={`${styles.controlBtn} ${autoScroll ? styles.controlBtnActive : styles.controlBtnPrimary}`}
            onClick={toggleAutoScroll}
            type="button"
            aria-pressed={autoScroll}
          >
            {autoScroll ? 'Pause scroll' : 'Resume scroll'}
          </button>
          <button
            className={`${styles.controlBtn} ${styles.controlBtnPrimary}`}
            onClick={handleCopy}
            type="button"
            aria-label="Copy log text to clipboard"
            disabled={lines.length === 0}
          >
            {copied ? (
              <>
                <Check size={13} /> Copied
              </>
            ) : (
              'Copy'
            )}
          </button>
        </div>
      </div>

      {/* Scrollable log body */}
      <div className={styles.body} ref={bodyRef} onScroll={handleScroll} aria-label="Log lines">
        {loading && <div className={styles.empty}>Loading logs…</div>}

        {!loading && loadError !== null && (
          <div className={styles.errorMsg} role="alert">
            {loadError}
          </div>
        )}

        {!loading && loadError === null && lines.length === 0 && (
          <div className={styles.empty}>No log output yet.</div>
        )}

        {lines.map((line) => (
          <LogLineRow key={line.key} line={line} />
        ))}

        {!loading && hasMore && (
          <div className={styles.loadMore}>
            <button
              className={styles.loadMoreBtn}
              onClick={loadMore}
              disabled={loadingMore}
              type="button"
            >
              {loadingMore ? 'Loading…' : 'Load more'}
            </button>
          </div>
        )}

        <div ref={bottomRef} aria-hidden="true" />
      </div>

      {/* Jump-to-bottom bar when paused with unread lines */}
      {!autoScroll && unreadCount > 0 && (
        <div className={styles.jumpBar}>
          <span className={styles.unreadBadge} aria-label={`${unreadCount} unread lines`}>
            {unreadCount} new line{unreadCount !== 1 ? 's' : ''}
          </span>
          <button className={styles.jumpBtn} onClick={jumpToBottom} type="button">
            Jump to bottom
          </button>
        </div>
      )}
    </div>
  )
}
