// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import styles from './ErrorBanner.module.css'

interface Props {
  /** Palette: 'error' (default) or 'warning'. */
  variant?: 'error' | 'warning' | undefined
  children: ReactNode
}

/** Full-width alert strip shown between the page header and its content. */
export default function ErrorBanner({ variant = 'error', children }: Props) {
  const palette = variant === 'warning' ? styles.warning : styles.error
  return (
    <div className={[styles.banner, palette].filter(Boolean).join(' ')} role="alert">
      {children}
    </div>
  )
}
