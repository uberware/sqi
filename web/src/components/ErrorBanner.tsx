// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import styles from './ErrorBanner.module.css'

/** Full-width error strip shown between the page header and its content. */
export default function ErrorBanner({ children }: { children: ReactNode }) {
  return (
    <div className={styles.errorBanner} role="alert">
      {children}
    </div>
  )
}
