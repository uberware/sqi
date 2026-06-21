// SPDX-License-Identifier: AGPL-3.0-or-later

import type { ReactNode } from 'react'
import type { IconProps } from './types'

interface BaseIconProps extends IconProps {
  children: ReactNode
}

/**
 * Shared wrapper for every icon. Establishes the standard contract: a square
 * `currentColor`-aware SVG on a 24×24 grid, hidden from assistive tech (icons
 * are decorative — their accessible name comes from the parent control's
 * `aria-label`).
 */
export function Icon({ size = 16, className, children }: BaseIconProps) {
  return (
    <svg
      className={className}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      aria-hidden="true"
      focusable="false"
    >
      {children}
    </svg>
  )
}
