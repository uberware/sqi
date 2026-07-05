// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useEffect, useRef, useState } from 'react'
import type { SyntheticEvent } from 'react'
import { Check, Copy } from '@/components/icons'
import { truncateId } from '@/lib/id'
import styles from './CopyableId.module.css'

/**
 * Truncated monospace id for table cells that copies the full id to the
 * clipboard on click (or Enter/Space), flashing a check icon on success.
 */
export default function CopyableId({ id }: { id: string }) {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (timerRef.current !== null) clearTimeout(timerRef.current)
    },
    [],
  )

  const triggerCopy = useCallback(
    (e: SyntheticEvent) => {
      e.preventDefault()
      e.stopPropagation()
      void navigator.clipboard
        .writeText(id)
        .then(() => {
          setCopied(true)
          if (timerRef.current !== null) clearTimeout(timerRef.current)
          timerRef.current = setTimeout(() => setCopied(false), 1500)
        })
        .catch(() => {
          // Clipboard write can fail in insecure contexts or when the document
          // is not focused. Silently ignore.
        })
    },
    [id],
  )

  return (
    <span
      className={styles.idCell}
      onClick={triggerCopy}
      title={`Click to copy: ${id}`}
      role="button"
      tabIndex={0}
      data-copied={copied || undefined}
      onKeyDown={(e) => {
        if (e.key === 'Enter' || e.key === ' ') triggerCopy(e)
      }}
    >
      {truncateId(id)}
      <span className={styles.copyHint}>{copied ? <Check size={12} /> : <Copy size={12} />}</span>
    </span>
  )
}
