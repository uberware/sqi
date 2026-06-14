import type { ReactNode } from 'react'
import styles from './PageHeader.module.css'

interface PageHeaderProps {
  title: string
  subtitle?: string
  action?: ReactNode
}

function invertCase(text: string): string {
  return text
    .split(' ')
    .map((word) =>
      word.length === 0 ? word : word[0]?.toLowerCase() + word.slice(1).toUpperCase(),
    )
    .join(' ')
}

export default function PageHeader({ title, subtitle, action }: PageHeaderProps) {
  return (
    <header className={styles.header}>
      <div className={styles.text}>
        <h1 className={styles.title}>{invertCase(title)}</h1>
        {subtitle !== undefined && <p className={styles.subtitle}>{subtitle}</p>}
      </div>
      {action !== undefined && <div className={styles.action}>{action}</div>}
    </header>
  )
}
