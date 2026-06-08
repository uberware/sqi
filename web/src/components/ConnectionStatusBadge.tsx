// SPDX-License-Identifier: AGPL-3.0-only

import { useConnectionState } from '@/ws/context'
import type { ConnectionState } from '@/ws/types'
import styles from './ConnectionStatusBadge.module.css'

function dotClass(state: ConnectionState): string {
  if (state === 'connected') return styles['dot--connected'] ?? ''
  if (state === 'reconnecting' || state === 'connecting') return styles['dot--reconnecting'] ?? ''
  return styles['dot--failed'] ?? ''
}

function label(state: ConnectionState): string {
  if (state === 'connected') return 'Live'
  if (state === 'reconnecting' || state === 'connecting') return 'Reconnecting'
  return 'Disconnected'
}

export default function ConnectionStatusBadge() {
  const state = useConnectionState()
  return (
    <span className={styles.badge} aria-label={`WebSocket: ${state}`} role="status">
      <span className={`${styles.dot} ${dotClass(state)}`} aria-hidden="true" />
      {label(state)}
    </span>
  )
}
