// A compact view of this week's agent usage (WP-D20): tokens, cache hit rate
// and the subscription quota, linking to Agents → Usage. For the overview page.

import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { Gauge } from 'lucide-react'
import { fresh, paceText, pct, tok, usageSummaryQuery } from '@/lib/usage-analytics'
import { relativeTime } from '@/lib/format'
import { Skeleton } from '@/components/ui/misc'

export function UsageSummary({ className }: { className?: string }) {
  const q = useQuery(usageSummaryQuery())
  const d = q.data
  const week = d?.quota?.windows.find((w) => w.span === 'seven_day')
  const pace = week && paceText(week)
  return (
    <Link
      to="/agents"
      search={{ tab: 'usage' }}
      className={clsx(
        'block rounded-xl border border-line bg-panel p-3 hover:bg-hover focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent',
        className,
      )}
    >
      <div className="mb-1.5 flex items-center gap-1.5 text-xs font-medium text-muted">
        <Gauge className="size-3.5" /> Agent usage this week
      </div>
      {q.isPending ? (
        <Skeleton className="h-10" />
      ) : q.isError && !d ? (
        <p role="alert" className="text-[13px] text-danger">
          Usage didn’t load.
        </p>
      ) : !d?.has_data ? (
        <p className="text-[13px] text-muted">
          Nothing reported yet. <code className="rounded bg-sunken px-1">wp hook claude</code>{' '}
          starts it.
        </p>
      ) : (
        <dl className="grid grid-cols-3 gap-3">
          <div className="min-w-0">
            <dt className="truncate text-xs text-subtle">Tokens</dt>
            <dd className="truncate text-base font-semibold tabular-nums">
              {tok(fresh(d.totals))}
            </dd>
          </div>
          <div className="min-w-0">
            <dt className="truncate text-xs text-subtle">Cache hits</dt>
            <dd className="truncate text-base font-semibold tabular-nums">
              {pct(d.totals.hit_rate)}
            </dd>
          </div>
          <div className="min-w-0">
            <dt className="truncate text-xs text-subtle">7-day quota</dt>
            <dd
              className={clsx(
                'truncate text-base font-semibold tabular-nums',
                pace?.tone === 'bad' && 'text-danger',
              )}
              title={pace?.text}
            >
              {week && week.status !== 'reset' ? `${Math.round(week.percent)}%` : '–'}
            </dd>
          </div>
        </dl>
      )}
      {pace && week?.status === 'will_run_out' && (
        <p className="mt-1.5 truncate text-xs text-danger">{pace.text}</p>
      )}
      {d?.last_sync && (
        <p className="mt-1 text-[11px] text-subtle">Synced {relativeTime(d.last_sync)}</p>
      )}
    </Link>
  )
}
