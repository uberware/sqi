// SPDX-License-Identifier: AGPL-3.0-or-later

import { useTheme } from '@/theme/context'
import { Moon, Sun } from '@/components/icons'
import styles from './ThemeToggle.module.css'

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
        <Sun className={styles.icon} />
        <Moon className={styles.icon} />
        <span className={styles.thumb} />
      </span>
    </button>
  )
}
