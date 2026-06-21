// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import { Spinner } from '@/components/icons'

interface IconButtonProps {
  /** Icon shown in the resting state. */
  icon: ReactNode
  /** Accessible name — icon-only buttons carry no visible text. */
  label: string
  onClick?: () => void
  /** When true, shows a spinner in place of the icon and disables the button. */
  busy?: boolean
  /** Disables the button independently of `busy`. */
  disabled?: boolean
  /** Hover tooltip. Defaults to `label`. */
  title?: string
  /** Visual styling — callers own the look (table-row reveal, color, etc.). */
  className?: string | undefined
  type?: 'button' | 'submit' | 'reset'
}

/**
 * A label-less button whose only content is an icon, swapped for a spinner
 * while an async action is in flight. Centralizes the busy/disabled/tooltip/
 * accessible-name contract shared by every row action across the list pages.
 */
export default function IconButton({
  icon,
  label,
  onClick,
  busy = false,
  disabled = false,
  title,
  className,
  type = 'button',
}: IconButtonProps) {
  return (
    <button
      type={type}
      className={className}
      onClick={onClick}
      disabled={busy || disabled}
      title={title ?? label}
      aria-label={label}
    >
      {busy ? <Spinner /> : icon}
    </button>
  )
}
