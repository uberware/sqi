// SPDX-License-Identifier: AGPL-3.0-or-later

import { useQuery } from '@tanstack/react-query'
import { apiFetch } from './client'

/** A single diagnostic log record (mirrors internal/diag.Record). */
export interface DiagRecord {
  ts: string
  component: string
  level: 'DEBUG' | 'INFO' | 'WARN' | 'ERROR'
  msg: string
  attrs?: Record<string, string>
}

export interface DiagnosticLogsResponse {
  records: DiagRecord[]
}

export interface DiagnosticsParams {
  component?: string
  level?: string
  task_id?: string
  since?: string
  limit?: number
}

function buildQuery(params: DiagnosticsParams): string {
  const qs = new URLSearchParams()
  if (params.component) qs.set('component', params.component)
  if (params.level) qs.set('level', params.level)
  if (params.task_id) qs.set('task_id', params.task_id)
  if (params.since) qs.set('since', params.since)
  if (params.limit != null) qs.set('limit', String(params.limit))
  const s = qs.toString()
  return s ? `?${s}` : ''
}

export function fetchDiagnosticsLogs(
  params: DiagnosticsParams = {},
): Promise<DiagnosticLogsResponse> {
  return apiFetch<DiagnosticLogsResponse>(`/diagnostics/logs${buildQuery(params)}`)
}

export const diagnosticsQueryKey = (params: DiagnosticsParams) =>
  ['diagnostics', params] as const

export function useDiagnosticsLogs(params: DiagnosticsParams = {}) {
  return useQuery({
    queryKey: diagnosticsQueryKey(params),
    queryFn: () => fetchDiagnosticsLogs(params),
  })
}
