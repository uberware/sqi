import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClientProvider } from '@tanstack/react-query'
import { queryClient } from '@/api/queryClient'
import { WebSocketProvider } from '@/ws/context'
import './index.css'
import App from './App.tsx'

const rootElement = document.getElementById('root')
if (!rootElement) throw new Error('Root element #root not found in document')

createRoot(rootElement).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <WebSocketProvider>
        <App />
      </WebSocketProvider>
    </QueryClientProvider>
  </StrictMode>,
)
