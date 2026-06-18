// SPDX-License-Identifier: AGPL-3.0-or-later

import type { DiagRecord } from '@/api/diagnostics'

/** Payload received on the "diagnostics" subject for each diagnostic record. */
export interface WsDiagEvent {
  component: string
  level: DiagRecord['level']
  msg: string
  attrs?: Record<string, string>
  at: string
}

/** Returns true when payload is a WsDiagEvent (has component, level, msg). */
export function isDiagEvent(payload: unknown): payload is WsDiagEvent {
  return (
    typeof payload === 'object' &&
    payload !== null &&
    'component' in payload &&
    'level' in payload &&
    'msg' in payload
  )
}
