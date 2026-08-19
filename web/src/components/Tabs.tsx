// SPDX-License-Identifier: AGPL-3.0-or-later

import { useCallback, useId, type KeyboardEvent, type ReactNode } from 'react'
import styles from './Tabs.module.css'

export interface TabDef {
  /** Stable identifier, also used as the URL `?tab=` value. */
  id: string
  /** Visible, accessible name for the tab. */
  label: string
  /** When true the tab cannot be selected and is skipped by the arrow keys. */
  disabled?: boolean
}

interface TabsProps {
  tabs: TabDef[]
  /** Currently selected tab id. */
  active: string
  onChange: (id: string) => void
  /** Accessible name for the tablist itself. */
  label: string
  /** The active tab's panel content — the caller renders one panel at a time. */
  children: ReactNode
}

/**
 * Controlled tabs following the WAI-ARIA tabs pattern: a roving tabindex so
 * only the selected tab is in the tab order, arrow keys to move between tabs
 * (wrapping, and skipping disabled ones), and Home/End to jump to the ends.
 *
 * Only the active panel is rendered. Callers own the panel content, so a tab
 * whose data is absent should be passed `disabled` rather than given an empty
 * panel.
 */
export default function Tabs({ tabs, active, onChange, label, children }: TabsProps) {
  const baseId = useId()
  const tabId = (id: string) => `${baseId}-tab-${id}`
  const panelId = (id: string) => `${baseId}-panel-${id}`

  const activeTab = tabs.find((t) => t.id === active)

  const onKeyDown = useCallback(
    (e: KeyboardEvent<HTMLButtonElement>) => {
      const selectable = tabs.filter((t) => t.disabled !== true)
      if (selectable.length === 0) return
      const current = selectable.findIndex((t) => t.id === active)
      let next: TabDef | undefined
      switch (e.key) {
        case 'ArrowRight':
          next = selectable[(current + 1) % selectable.length]
          break
        case 'ArrowLeft':
          next = selectable[(current - 1 + selectable.length) % selectable.length]
          break
        case 'Home':
          next = selectable[0]
          break
        case 'End':
          next = selectable[selectable.length - 1]
          break
        default:
          return
      }
      e.preventDefault()
      if (next !== undefined) onChange(next.id)
    },
    [tabs, active, onChange],
  )

  return (
    <div className={styles.tabs}>
      <div role="tablist" aria-label={label} className={styles.tablist}>
        {tabs.map((t) => {
          const selected = t.id === active
          return (
            <button
              key={t.id}
              type="button"
              role="tab"
              id={tabId(t.id)}
              aria-selected={selected}
              aria-controls={panelId(t.id)}
              tabIndex={selected ? 0 : -1}
              disabled={t.disabled === true}
              className={styles.tab}
              onClick={() => onChange(t.id)}
              onKeyDown={onKeyDown}
            >
              {t.label}
            </button>
          )
        })}
      </div>
      {activeTab !== undefined && (
        <div
          role="tabpanel"
          id={panelId(activeTab.id)}
          aria-labelledby={tabId(activeTab.id)}
          tabIndex={0}
          className={styles.panel}
        >
          {children}
        </div>
      )}
    </div>
  )
}
