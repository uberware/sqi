// SPDX-License-Identifier: AGPL-3.0-or-later
import type { ProductParameter } from '@/api/types'

export type Widget = 'text' | 'textarea' | 'select' | 'checkbox' | 'chips' | 'number' | 'hidden'

const CONTROL_WIDGET: Record<string, Widget> = {
  LINE_EDIT: 'text',
  MULTILINE_EDIT: 'textarea',
  DROPDOWN_LIST: 'select',
  CHECK_BOX: 'checkbox',
  CHIP_INPUT: 'chips',
  SPIN_BOX: 'number',
  HIDDEN: 'hidden',
}

/** Choose a form widget for a parameter: explicit userInterface control first,
 * else a fallback by type + constraints. */
export function selectWidget(p: ProductParameter): Widget {
  const control = p.user_interface?.control
  if (control && control in CONTROL_WIDGET) {
    return CONTROL_WIDGET[control] as Widget
  }
  if (p.allowed_values && p.allowed_values.length > 0) return 'select'
  if (p.type === 'INT' || p.type === 'FLOAT') return 'number'
  return 'text'
}

export function paramLabel(p: ProductParameter): string {
  return p.user_interface?.label || p.name
}

export function isRequired(p: ProductParameter): boolean {
  return p.default === null
}

export function initialValue(p: ProductParameter): string {
  return p.default ?? ''
}

/** Group heading for a parameter, or '' when ungrouped. */
export function paramGroup(p: ProductParameter): string {
  return p.user_interface?.group_label ?? ''
}

/** "<title> <local timestamp>" — the editable default job name. */
export function defaultJobName(title: string, now: Date = new Date()): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  const ts = `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())} ${pad(now.getHours())}:${pad(now.getMinutes())}`
  return `${title} ${ts}`
}
