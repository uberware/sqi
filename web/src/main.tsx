import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { QueryClientProvider } from '@tanstack/react-query'
import { queryClient } from '@/api/queryClient'
import { AuthProvider } from '@/auth/context'
import { ThemeProvider } from '@/theme/context'
import './index.css'
import App from './App.tsx'

const rootElement = document.getElementById('root')
if (!rootElement) throw new Error('Root element #root not found in document')

createRoot(rootElement).render(
  <StrictMode>
    <BrowserRouter>
      <QueryClientProvider client={queryClient}>
        {/*
          AuthProvider sits inside QueryClientProvider (useAuthMe needs a
          query client) and outside ThemeProvider so the login page is
          themed too. WebSocketProvider used to live here, connecting
          unconditionally on mount; App now owns it instead, scoped to the
          authed shell branch — connecting a socket before the app knows
          whether anyone is logged in has no use, since it would just be
          torn down and reopened once the login page hands off. When auth is
          disabled server-side, /auth/me resolves 'authed' with the
          anonymous principal immediately, so this costs nothing in that
          (default) mode.
        */}
        <AuthProvider>
          <ThemeProvider>
            <App />
          </ThemeProvider>
        </AuthProvider>
      </QueryClientProvider>
    </BrowserRouter>
  </StrictMode>,
)
