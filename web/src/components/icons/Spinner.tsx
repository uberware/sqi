// SPDX-License-Identifier: AGPL-3.0-or-later

import { Icon } from './Icon'
import type { IconProps } from './types'
import styles from './Spinner.module.css'

/** Animated loading spinner — shown in place of an action icon while pending. */
export function Spinner({ size = 16, className }: IconProps) {
  const cls = [styles.spinner, className].filter(Boolean).join(' ')
  return (
    <Icon size={size} className={cls}>
      <circle
        cx="12"
        cy="12"
        r="9"
        fill="none"
        stroke="currentColor"
        strokeWidth={2}
        strokeOpacity={0.25}
      />
      <path
        d="M21 12a9 9 0 0 0-9-9"
        fill="none"
        stroke="currentColor"
        strokeWidth={2}
        strokeLinecap="round"
      />
    </Icon>
  )
}
