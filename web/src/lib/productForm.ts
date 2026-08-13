// SPDX-License-Identifier: AGPL-3.0-or-later
import type { ProductParameter } from '@/api/types'

export type Widget = 'text' | 'textarea' | 'select' | 'checkbox' | 'number' | 'hidden' | 'list'

const CONTROL_WIDGET: Record<string, Widget> = {
  LINE_EDIT: 'text',
  MULTILINE_EDIT: 'textarea',
  DROPDOWN_LIST: 'select',
  CHECK_BOX: 'checkbox',
  SPIN_BOX: 'number',
  HIDDEN: 'hidden',
  // No file-picker widget exists yet; these render as text inputs, which is
  // exactly what these fields rendered as before the controls were recognised.
  CHOOSE_INPUT_FILE: 'text',
  CHOOSE_OUTPUT_FILE: 'text',
  CHOOSE_DIRECTORY: 'text',
  // RFC 0007's list controls. The three CHOOSE_*_LIST variants map to the same
  // row editor as the others rather than to a file dialog, following the
  // precedent set by their scalar counterparts three lines above.
  LINE_EDIT_LIST: 'list',
  SPIN_BOX_LIST: 'list',
  CHECK_BOX_LIST: 'list',
  CHOOSE_INPUT_FILE_LIST: 'list',
  CHOOSE_OUTPUT_FILE_LIST: 'list',
  CHOOSE_DIRECTORY_LIST: 'list',
}

/** Element type of a LIST[*] type, or null for a scalar type. */
export function listElementType(t: ProductParameter['type']): ProductParameter['type'] | null {
  switch (t) {
    case 'LIST[STRING]':
      return 'STRING'
    case 'LIST[PATH]':
      return 'PATH'
    case 'LIST[INT]':
      return 'INT'
    case 'LIST[FLOAT]':
      return 'FLOAT'
    case 'LIST[BOOL]':
      return 'BOOL'
    case 'LIST[LIST[INT]]':
      return 'LIST[INT]'
    default:
      return null
  }
}

/** Choose a form widget for a parameter: explicit userInterface control first,
 * else a fallback by type + constraints. */
export function selectWidget(p: ProductParameter): Widget {
  const control = p.user_interface?.control
  if (control && control in CONTROL_WIDGET) {
    return CONTROL_WIDGET[control] as Widget
  }
  if (p.allowed_values && p.allowed_values.length > 0) return 'select'
  // LIST[LIST[INT]] is deliberately absent: RFC 0007 gives it no control but
  // HIDDEN and describes its use case as programmatic, so it falls through to
  // a raw JSON text field rather than a doubly-nested row editor.
  if (
    p.type === 'LIST[STRING]' ||
    p.type === 'LIST[PATH]' ||
    p.type === 'LIST[INT]' ||
    p.type === 'LIST[FLOAT]' ||
    p.type === 'LIST[BOOL]'
  ) {
    return 'list'
  }
  if (p.type === 'BOOL') return 'checkbox'
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
