import styles from './Sidebar.module.css'

interface NavItem {
  label: string
  href: string
}

const PHASE1_NAV: NavItem[] = [
  { label: 'Dashboard', href: '/' },
  { label: 'Jobs', href: '/jobs' },
  { label: 'Workers', href: '/workers' },
  { label: 'Submit', href: '/submit' },
]

// Labels only — hrefs are inert until routing is wired in task 32
const DEFERRED_LABELS = ['Presets', 'Products', 'Storage', 'License Pools', 'Settings']

export default function Sidebar() {
  return (
    <nav className={styles.sidebar} aria-label="Primary navigation">
      <div className={styles.logoArea}>
        <span className={styles.logoText}>sqi</span>
      </div>

      <ul className={styles.navSection} role="list">
        {PHASE1_NAV.map((item) => (
          <li key={item.href}>
            <a href={item.href} className={styles.navLink}>
              {item.label}
            </a>
          </li>
        ))}
      </ul>

      <div className={styles.divider} />

      <ul className={styles.navSection} role="list">
        {DEFERRED_LABELS.map((label) => (
          <li key={label}>
            <span className={styles.deferredItem} data-disabled="true">
              {label}
              <span className={styles.comingSoonBadge}>coming soon</span>
            </span>
          </li>
        ))}
      </ul>
    </nav>
  )
}
