import { Link } from '@tanstack/react-router'
import { Activity, History } from 'lucide-react'

const tab =
  'inline-flex h-7 items-center gap-1.5 rounded-md px-2.5 text-[13px] font-medium transition-colors'
const active = 'bg-panel text-fg shadow-card'
const inactive = 'text-muted hover:text-fg'

export function CITabs() {
  return (
    <nav aria-label="CI views" className="inline-flex rounded-lg bg-sunken p-0.5">
      <Link
        to="/ci"
        activeOptions={{ exact: true }}
        className={tab}
        activeProps={{ className: active, 'aria-current': 'page' }}
        inactiveProps={{ className: inactive }}
      >
        <Activity className="size-3.5" aria-hidden />
        Now
      </Link>
      <Link
        to="/ci/history"
        className={tab}
        activeProps={{ className: active, 'aria-current': 'page' }}
        inactiveProps={{ className: inactive }}
      >
        <History className="size-3.5" aria-hidden />
        History
      </Link>
    </nav>
  )
}
