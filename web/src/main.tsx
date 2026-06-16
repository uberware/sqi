import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { QueryClientProvider } from '@tanstack/react-query'
import { queryClient } from '@/api/queryClient'
import { WebSocketProvider } from '@/ws/context'
import { ThemeProvider } from '@/theme/context'
import './index.css'
import App from './App.tsx'

const rootElement = document.getElementById('root')
if (!rootElement) throw new Error('Root element #root not found in document')

createRoot(rootElement).render(
  <StrictMode>
    <BrowserRouter>
      <QueryClientProvider client={queryClient}>
        <WebSocketProvider>
          <ThemeProvider>
            <App />
          </ThemeProvider>
        </WebSocketProvider>
      </QueryClientProvider>
    </BrowserRouter>
  </StrictMode>,
)
