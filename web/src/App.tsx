import styles from './App.module.css'
import ErrorBoundary from '@/components/ErrorBoundary'
import Sidebar from '@/components/layout/Sidebar'
import { ToastProvider } from '@/components/Toast'
import AppRoutes from '@/routes'

export default function App() {
  return (
    <ToastProvider>
      <div className={styles.layout}>
        <Sidebar />
        <main className={styles.main}>
          <ErrorBoundary>
            <AppRoutes />
          </ErrorBoundary>
        </main>
      </div>
    </ToastProvider>
  )
}
