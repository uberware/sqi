// SPDX-License-Identifier: AGPL-3.0-or-later
import { useMemo, useState } from 'react'
import type { PathDelivery, PathDeliveryKind, PathTranslation } from '@/api/types'
import styles from './ProductPathDelivery.module.css'

interface Props {
  value: PathTranslation | null
  onChange: (pt: PathTranslation) => void
}

const LABELS: Record<PathDeliveryKind, string> = {
  swap_in_place: 'Swap paths in place',
  translation_file: 'Write translation file',
  command_flags: 'Pass as command flags',
  environment: 'Set environment variables',
  stage_locally: 'Stage files locally',
}

const ORDER: PathDeliveryKind[] = [
  'swap_in_place',
  'translation_file',
  'command_flags',
  'environment',
  'stage_locally',
]

export function ProductPathDelivery({ value, onChange }: Props) {
  // Local state drives the UI so the checkboxes respond immediately to user
  // interaction. When the external prop changes (e.g. user edits the raw
  // template), we sync back using the store-previous-prop pattern recommended
  // by React: https://react.dev/learn/you-might-not-need-an-effect#adjusting-some-state-when-a-prop-changes
  const [local, setLocal] = useState<PathTranslation | null>(value)
  const [prevValue, setPrevValue] = useState<PathTranslation | null>(value)
  if (value !== prevValue) {
    setPrevValue(value)
    setLocal(value)
  }

  const byKind = useMemo(() => {
    const m = new Map<PathDeliveryKind, PathDelivery>()
    for (const d of local?.deliveries ?? []) m.set(d.kind, d)
    return m
  }, [local])

  const emit = (next: Map<PathDeliveryKind, PathDelivery>) => {
    const deliveries: PathDelivery[] = []
    for (const k of ORDER) {
      const d = next.get(k)
      if (d !== undefined) deliveries.push(d)
    }
    const pt: PathTranslation = { deliveries }
    setLocal(pt)
    onChange(pt)
  }

  const toggle = (kind: PathDeliveryKind, checked: boolean) => {
    const next = new Map(byKind)
    if (checked) next.set(kind, { kind })
    else next.delete(kind)
    emit(next)
  }

  const setField = (kind: PathDeliveryKind, field: 'pattern' | 'variable', v: string) => {
    const next = new Map(byKind)
    const cur = next.get(kind) ?? { kind }
    if (field === 'pattern') {
      next.set(kind, { ...cur, pattern: v })
    } else {
      next.set(kind, { ...cur, variable: v })
    }
    emit(next)
  }

  return (
    <fieldset className={styles.panel}>
      <legend>Path delivery</legend>
      {ORDER.map((kind) => {
        const checked = byKind.has(kind)
        return (
          <div key={kind} className={styles.row}>
            <label>
              <input
                type="checkbox"
                checked={checked}
                onChange={(e) => toggle(kind, e.target.checked)}
              />
              {LABELS[kind]}
            </label>
            {checked && kind === 'command_flags' && (
              <label className={styles.field}>
                Flag pattern
                <input
                  type="text"
                  value={byKind.get(kind)?.pattern ?? ''}
                  placeholder="--remap {src}={dest}"
                  onChange={(e) => setField(kind, 'pattern', e.target.value)}
                />
              </label>
            )}
            {checked && kind === 'environment' && (
              <label className={styles.field}>
                Variable name
                <input
                  type="text"
                  value={byKind.get(kind)?.variable ?? ''}
                  placeholder="PROJECT_ROOT"
                  onChange={(e) => setField(kind, 'variable', e.target.value)}
                />
              </label>
            )}
          </div>
        )
      })}
    </fieldset>
  )
}
