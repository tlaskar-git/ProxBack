import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import './index.css'
import App from './App.tsx'
import { ToastProvider } from './components/Toast.tsx'
import { ConfirmProvider } from './components/Confirm.tsx'
import { ThemeProvider } from './theme.tsx'

const container = document.getElementById('root')
if (!container) throw new Error('Root element #root is missing from index.html.')

createRoot(container).render(
  <StrictMode>
    <ThemeProvider>
      <BrowserRouter>
        <ToastProvider>
          <ConfirmProvider>
            <App />
          </ConfirmProvider>
        </ToastProvider>
      </BrowserRouter>
    </ThemeProvider>
  </StrictMode>,
)
