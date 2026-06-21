// SPDX-License-Identifier: AGPL-3.0-or-later

import { Icon } from './Icon'
import type { IconProps } from './types'

// Shared stroke attributes for the outline-style icons. Solid icons (Pause,
// Play) set their own `fill` instead.
const stroke = {
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 2,
  strokeLinecap: 'round',
  strokeLinejoin: 'round',
} as const

/** Pause — disable a worker (reversible "stop taking work"). */
export function Pause(props: IconProps) {
  return (
    <Icon {...props}>
      <rect x="6" y="5" width="4" height="14" rx="1" fill="currentColor" />
      <rect x="14" y="5" width="4" height="14" rx="1" fill="currentColor" />
    </Icon>
  )
}

/** Play — enable a worker (natural pair with Pause). */
export function Play(props: IconProps) {
  return (
    <Icon {...props}>
      <path d="M8 5v14l11-7z" fill="currentColor" />
    </Icon>
  )
}

/** Trash — remove a worker / delete a job. */
export function Trash(props: IconProps) {
  return (
    <Icon {...props}>
      <g {...stroke}>
        <path d="M3 6h18" />
        <path d="M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2" />
        <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
        <line x1="10" y1="11" x2="10" y2="17" />
        <line x1="14" y1="11" x2="14" y2="17" />
      </g>
    </Icon>
  )
}

/** X — cancel / abort an operation. */
export function X(props: IconProps) {
  return (
    <Icon {...props}>
      <g {...stroke}>
        <line x1="6" y1="6" x2="18" y2="18" />
        <line x1="18" y1="6" x2="6" y2="18" />
      </g>
    </Icon>
  )
}

/** Rotate (clockwise, single arrow) — retry a task. */
export function Rotate(props: IconProps) {
  return (
    <Icon {...props}>
      <g {...stroke}>
        <polyline points="21 4 21 10 15 10" />
        <path d="M20.49 15a9 9 0 1 1-2.12-9.36L21 8" />
      </g>
    </Icon>
  )
}

/** Refresh (two arrows) — reload a list. Distinct from the single-arrow Rotate. */
export function Refresh(props: IconProps) {
  return (
    <Icon {...props}>
      <g {...stroke}>
        <polyline points="21 4 21 10 15 10" />
        <polyline points="3 20 3 14 9 14" />
        <path d="M19.42 9A7.5 7.5 0 0 0 6.7 6L3 9.5M21 14.5L17.3 18A7.5 7.5 0 0 1 4.58 15" />
      </g>
    </Icon>
  )
}

/** Document — view a task's logs. */
export function Document(props: IconProps) {
  return (
    <Icon {...props}>
      <g {...stroke}>
        <path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z" />
        <polyline points="14 2 14 8 20 8" />
        <line x1="16" y1="13" x2="8" y2="13" />
        <line x1="16" y1="17" x2="8" y2="17" />
        <line x1="10" y1="9" x2="8" y2="9" />
      </g>
    </Icon>
  )
}

/** Copy — copy a value to the clipboard. */
export function Copy(props: IconProps) {
  return (
    <Icon {...props}>
      <g {...stroke}>
        <rect x="9" y="9" width="13" height="13" rx="2" />
        <path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1" />
      </g>
    </Icon>
  )
}

/** Check — confirmation / "copied" state. */
export function Check(props: IconProps) {
  return (
    <Icon {...props}>
      <polyline {...stroke} points="20 6 9 17 4 12" />
    </Icon>
  )
}

/** ChevronUp — ascending sort indicator. */
export function ChevronUp(props: IconProps) {
  return (
    <Icon {...props}>
      <polyline {...stroke} points="18 15 12 9 6 15" />
    </Icon>
  )
}

/** ChevronDown — descending sort indicator. */
export function ChevronDown(props: IconProps) {
  return (
    <Icon {...props}>
      <polyline {...stroke} points="6 9 12 15 18 9" />
    </Icon>
  )
}

/** ChevronUpDown — sortable-but-unsorted column indicator. */
export function ChevronUpDown(props: IconProps) {
  return (
    <Icon {...props}>
      <g {...stroke}>
        <polyline points="8 9 12 5 16 9" />
        <polyline points="16 15 12 19 8 15" />
      </g>
    </Icon>
  )
}

/** Sun — light theme. */
export function Sun(props: IconProps) {
  return (
    <Icon {...props}>
      <circle cx="12" cy="12" r="4.5" fill="currentColor" />
      <g stroke="currentColor" strokeWidth={2} strokeLinecap="round">
        <line x1="12" y1="1.5" x2="12" y2="4" />
        <line x1="12" y1="20" x2="12" y2="22.5" />
        <line x1="1.5" y1="12" x2="4" y2="12" />
        <line x1="20" y1="12" x2="22.5" y2="12" />
        <line x1="4.2" y1="4.2" x2="6" y2="6" />
        <line x1="18" y1="18" x2="19.8" y2="19.8" />
        <line x1="19.8" y1="4.2" x2="18" y2="6" />
        <line x1="6" y1="18" x2="4.2" y2="19.8" />
      </g>
    </Icon>
  )
}

/** Moon — dark theme. */
export function Moon(props: IconProps) {
  return (
    <Icon {...props}>
      <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" fill="currentColor" />
    </Icon>
  )
}
