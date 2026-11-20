import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { RouterProvider } from '@tanstack/react-router'
import { ApiError, setUnauthorizedHandler } from '@/lib/api'
import { applyTheme, storedTheme, watchSystemTheme } from '@/lib/theme'
import { makeRouter } from './router'
import './index.css'

applyTheme(storedTheme())
watchSystemTheme()

const qc = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 30_000,
      gcTime: 30 * 60_000,
      retry: (n, e) => !(e instanceof ApiError && e.status < 500) && n < 2,
      refetchOnWindowFocus: true,
    },
  },
})

const router = makeRouter(qc)

setUnauthorizedHandler(() => {
  qc.clear()
  if (!location.pathname.startsWith('/login'))
    router.navigate({ to: '/login', search: { next: location.pathname + location.search } })
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
)

if ('serviceWorker' in navigator && import.meta.env.PROD) {
  navigator.serviceWorker.register('/sw.js').catch(() => {})
}
