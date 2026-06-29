import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import styles from './PageHeader.module.css'

interface PageHeaderProps {
  title: string
  subtitle?: string
  action?: ReactNode
  /**
   * Vertical alignment of the action slot within the header. 'start' (default)
   * keeps it level with the title; 'end' bottom-aligns it with the subtitle.
   */
  actionAlign?: 'start' | 'end'
  /**
   * When set, renders a "← {backLabel}" link in the header's right cluster,
   * top-aligned with the title and before any action.
   */
  backTo?: string
  /** Label for the back link (default 'Admin'). */
  backLabel?: string
}

function invertCase(text: string): string {
  return text
    .split(' ')
    .map((word) =>
      word.length === 0 ? word : word[0]?.toLowerCase() + word.slice(1).toUpperCase(),
    )
    .join(' ')
}

export default function PageHeader({
  title,
  subtitle,
  action,
  actionAlign = 'start',
  backTo,
  backLabel = 'Admin',
}: PageHeaderProps) {
  const actionClass = actionAlign === 'end' ? `${styles.action} ${styles.actionEnd}` : styles.action
  return (
    <header className={styles.header}>
      <div className={styles.text}>
        <h1 className={styles.title}>{invertCase(title)}</h1>
        {subtitle !== undefined && <p className={styles.subtitle}>{subtitle}</p>}
      </div>
      {backTo !== undefined ? (
        <div className={styles.headerEnd}>
          <Link to={backTo} className={styles.back}>
            ← {backLabel}
          </Link>
          {action !== undefined && <div className={actionClass}>{action}</div>}
        </div>
      ) : (
        action !== undefined && <div className={actionClass}>{action}</div>
      )}
    </header>
  )
}
