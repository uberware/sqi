// SPDX-License-Identifier: AGPL-3.0-or-later

import { useState, useCallback, useRef, useEffect } from 'react'
import styles from './CopyButton.module.css'

interface CopyButtonProps {
  /** The string that will be written to the clipboard on click. */
  value: string
  /** Text shown in place of `value`. Useful for truncated IDs. Defaults to `value`. */
  display?: string
  className?: string
}

const COPIED_RESET_MS = 1_500

export default function CopyButton({ value, display, className }: CopyButtonProps) {
  const [copied, setCopied] = useState(false)
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(
    () => () => {
      if (timerRef.current !== null) clearTimeout(timerRef.current)
    },
    [],
  )

  const handleCopy = useCallback(
    (e: React.SyntheticEvent) => {
      e.preventDefault()
      e.stopPropagation()
      void navigator.clipboard
        .writeText(value)
        .then(() => {
          setCopied(true)
          if (timerRef.current !== null) clearTimeout(timerRef.current)
          timerRef.current = setTimeout(() => setCopied(false), COPIED_RESET_MS)
        })
        .catch(() => {
          // Clipboard write can fail in insecure contexts or when the document
          // is not focused. Silently ignore — user will see no feedback.
        })
    },
    [value],
  )

  const label = display ?? value

  return (
    <button
      type="button"
      className={[styles.root, copied ? styles['root--copied'] : '', className]
        .filter(Boolean)
        .join(' ')}
      onClick={handleCopy}
      title={copied ? 'Copied!' : `Click to copy: ${value}`}
      aria-label={copied ? 'Copied!' : `Copy ${label}`}
    >
      <span className={styles.text}>{label}</span>
      <span className={styles.icon} aria-hidden="true">
        {copied ? '✓' : '⎘'}
      </span>
      {copied && (
        <span className={styles.tooltip} role="tooltip">
          Copied!
        </span>
      )}
    </button>
  )
}
