import styles from './App.module.css'
import ErrorBoundary from '@/components/ErrorBoundary'
import ConnectionStatusBadge from '@/components/ConnectionStatusBadge'
import Sidebar from '@/components/layout/Sidebar'
import AppRoutes from '@/routes'

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
            <AppRoutes />
          </ErrorBoundary>
        </main>
      </div>
    </div>
  )
}
