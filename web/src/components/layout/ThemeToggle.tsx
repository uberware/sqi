// SPDX-License-Identifier: AGPL-3.0-or-later

import { useTheme } from '@/theme/context'
import styles from './ThemeToggle.module.css'

function SunIcon() {
  return (
    <svg className={styles.icon} viewBox="0 0 24 24" aria-hidden="true" focusable="false">
      <circle cx="12" cy="12" r="4.5" fill="currentColor" />
      <g stroke="currentColor" strokeWidth="2" strokeLinecap="round">
        <line x1="12" y1="1.5" x2="12" y2="4" />
        <line x1="12" y1="20" x2="12" y2="22.5" />
        <line x1="1.5" y1="12" x2="4" y2="12" />
        <line x1="20" y1="12" x2="22.5" y2="12" />
        <line x1="4.2" y1="4.2" x2="6" y2="6" />
        <line x1="18" y1="18" x2="19.8" y2="19.8" />
        <line x1="19.8" y1="4.2" x2="18" y2="6" />
        <line x1="6" y1="18" x2="4.2" y2="19.8" />
      </g>
    </svg>
  )
}

function MoonIcon() {
  return (
    <svg className={styles.icon} viewBox="0 0 24 24" aria-hidden="true" focusable="false">
      <path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z" fill="currentColor" />
    </svg>
  )
}

export default function ThemeToggle() {
  const { theme, toggleTheme } = useTheme()
  const isDark = theme === 'dark'

  return (
    <button
      type="button"
      role="switch"
      aria-checked={isDark}
      aria-label="Dark mode"
      className={styles.toggle}
      onClick={toggleTheme}
    >
      <span className={styles.track} data-checked={isDark}>
        <SunIcon />
        <MoonIcon />
        <span className={styles.thumb} />
      </span>
    </button>
  )
}
