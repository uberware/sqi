// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import { Spinner } from '@/components/icons'

interface IconButtonProps {
  /** Icon shown in the resting state. */
  icon: ReactNode
  /**
   * Accessible name. An icon-only button carries no visible text; with
   * `children` it must include that text.
   */
  label: string
  /** Optional visible text, shown after the icon. */
  children?: ReactNode
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
 * A button led by an icon, swapped for a spinner while an async action is in
 * flight, with optional visible text after it. Centralizes the busy/disabled/
 * tooltip/accessible-name contract shared by the row actions across the list
 * pages and the actions on detail pages.
 */
export default function IconButton({
  icon,
  label,
  children,
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
      {children}
    </button>
  )
}
