import styles from './App.module.css'
import ErrorBoundary from '@/components/ErrorBoundary'
import ConnectionStatusBadge from '@/components/ConnectionStatusBadge'
import Sidebar from '@/components/layout/Sidebar'

export default function App() {
  return (
    <div className={styles.layout}>
      <Sidebar />
      <div className={styles.content}>
        <header className={styles.appHeader}>
          <ConnectionStatusBadge />
        </header>
        <main className={styles.main}>
          <ErrorBoundary>
            {/* Route outlet — populated in task 32 when React Router is added */}
          </ErrorBoundary>
        </main>
      </div>
    </div>
  )
}
