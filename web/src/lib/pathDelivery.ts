// SPDX-License-Identifier: AGPL-3.0-or-later
import { parse as parseYaml, stringify as stringifyYaml } from 'yaml'
import type { PathDelivery, PathDeliveryKind, PathTranslation } from '@/api/types'

const EXT = 'SQI_PATH_TRANSLATION'

function decode(doc: unknown): PathTranslation | null {
  if (typeof doc !== 'object' || doc === null) return null
  const obj = doc as Record<string, unknown>
  const exts = obj.extensions
  const declared = Array.isArray(exts) && exts.includes(EXT)
  if (!declared) return null
  const block = obj[EXT] as Record<string, unknown> | undefined
  const rawList = (block?.deliveries as unknown[]) ?? []
  const deliveries: PathDelivery[] = []
  for (const item of rawList) {
    if (typeof item === 'string') {
      deliveries.push({ kind: item as PathDeliveryKind })
    } else if (item && typeof item === 'object') {
      const entries = Object.entries(item as Record<string, unknown>)
      const first = entries[0]
      if (entries.length === 1 && first !== undefined) {
        const [kind, settings] = first
        const s = (settings ?? {}) as Record<string, unknown>
        deliveries.push({
          kind: kind as PathDeliveryKind,
          ...(typeof s.pattern === 'string' ? { pattern: s.pattern } : {}),
          ...(typeof s.variable === 'string' ? { variable: s.variable } : {}),
        })
      }
    }
  }
  return { deliveries }
}

export function parsePathDeliveries(
  template: string,
  format: 'yaml' | 'json',
): PathTranslation | null {
  const doc = format === 'json' ? JSON.parse(template) : parseYaml(template)
  return decode(doc)
}

function encodeDelivery(d: PathDelivery): unknown {
  if (d.kind === 'command_flags') return { command_flags: { pattern: d.pattern ?? '' } }
  if (d.kind === 'environment') return { environment: { variable: d.variable ?? '' } }
  return d.kind
}

export function serializePathDeliveries(
  template: string,
  format: 'yaml' | 'json',
  pt: PathTranslation,
): string {
  const doc = (format === 'json' ? JSON.parse(template) : parseYaml(template)) as Record<
    string,
    unknown
  >
  const exts = new Set<string>(Array.isArray(doc.extensions) ? (doc.extensions as string[]) : [])
  exts.add(EXT)
  doc.extensions = [...exts]
  doc[EXT] = { deliveries: pt.deliveries.map(encodeDelivery) }
  return format === 'json' ? JSON.stringify(doc, null, 2) : stringifyYaml(doc)
}
