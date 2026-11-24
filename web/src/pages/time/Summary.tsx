import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { q } from '@/lib/queries'
import { duration, hashColor, hours, localISO } from '@/lib/format'
import { agentLabel } from '@/lib/agents'
import { Bot } from 'lucide-react'
import type { TimeBucket, TimeReport } from '@/lib/types'
import { Section, Skeleton } from '@/components/ui/misc'
import { addDays } from './util'

/** Seven bars, one per day, with the week total. */
export function WeekStrip({
  from,
  report,
  onPickDay,
}: {
  from: Date
  report: TimeReport | undefined
  onPickDay: (iso: string) => void
}) {
  const byKey = new Map((report?.by_day ?? []).map((b) => [b.key, b.seconds]))
  const days = Array.from({ length: 7 }, (_, i) => {
    const d = addDays(from, i)
    const k = localISO(d)
    return { d, k, secs: byKey.get(k) ?? 0 }
  })
  const max = Math.max(8 * 3600, ...days.map((x) => x.secs))
  const today = localISO(new Date())
  if (!report || report.total_seconds === 0) return null
  return (
    <div className="rounded-xl border border-line bg-panel p-3 sm:p-4">
      <div className="mb-3 flex items-baseline justify-between">
        <span className="text-xs font-semibold uppercase tracking-wide text-subtle">
          Hours per day
        </span>
        <span className="text-[13px]">
          <span className="text-lg font-semibold tabular-nums">{hours(report.total_seconds)}</span>
          <span className="text-muted"> h total</span>
        </span>
      </div>
      <div className="grid grid-cols-7 gap-1.5 sm:gap-3">
        {days.map(({ d, k, secs }) => (
          <button
            key={k}
            type="button"
            onClick={() => secs > 0 && onPickDay(k)}
            title={`${d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' })} — ${secs > 0 ? duration(secs) : 'no time'}`}
            className={clsx(
              'group flex flex-col items-center gap-1',
              secs > 0 ? 'cursor-pointer' : 'cursor-default',
            )}
          >
            <span className="text-[11px] tabular-nums text-muted">
              {secs > 0 ? hours(secs) : ''}&nbsp;
            </span>
            <span className="flex h-20 w-full items-end overflow-hidden border-b-2 border-line">
              <span
                className={clsx(
                  'w-full rounded-t-md transition-[height] group-hover:brightness-110',
                  k === today ? 'bg-accent' : 'bg-accent/60',
                )}
                style={{ height: `${(secs / max) * 100}%` }}
              />
            </span>
            <span
              className={clsx(
                'text-[11px]',
                k === today ? 'font-semibold text-accent' : 'text-subtle',
              )}
            >
              {d.toLocaleDateString(undefined, { weekday: 'short' })}
              <span className="block sm:inline"> {d.getDate()}</span>
            </span>
          </button>
        ))}
      </div>
    </div>
  )
}

function Bars({
  items,
  color,
  render,
}: {
  items: TimeBucket[]
  color: (b: TimeBucket) => string
  render: (b: TimeBucket) => React.ReactNode
}) {
  const max = Math.max(1, ...items.map((b) => b.seconds))
  return (
    <ul className="space-y-2.5 p-4">
      {items.map((b) => (
        <li key={b.key} className="space-y-1">
          <div className="flex items-baseline justify-between gap-2 text-[13px]">
            <span className="min-w-0 truncate">{render(b)}</span>
            <span className="shrink-0 text-xs tabular-nums text-muted">{duration(b.seconds)}</span>
          </div>
          <div className="h-1.5 overflow-hidden rounded-full bg-sunken">
            <div
              className="h-full rounded-full"
              style={{ width: `${(b.seconds / max) * 100}%`, background: color(b) }}
            />
          </div>
        </li>
      ))}
    </ul>
  )
}

export function SummaryPanel({ report }: { report: TimeReport | undefined }) {
  const projects = useQuery(q.projects()).data ?? []
  const colorOf = (key: string) =>
    projects.find((p) => p.key === key)?.color ??
    (key === '—' ? 'var(--wp-line-strong)' : hashColor(key))
  if (!report) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-40" />
        <Skeleton className="h-56" />
      </div>
    )
  }
  if (report.total_seconds === 0) return null
  const topIssues = report.by_issue.slice(0, 8)
  return (
    <div className="space-y-4">
      <Section title="By project">
        <Bars
          items={report.by_project}
          color={(b) => colorOf(b.key)}
          render={(b) =>
            b.key === '—' ? (
              <span className="text-muted">No project</span>
            ) : (
              <span className="font-medium">{b.label}</span>
            )
          }
        />
      </Section>
      <Section title="Top issues">
        <Bars
          items={topIssues}
          color={(b) => (b.key === '—' ? 'var(--wp-line-strong)' : colorOf(b.key.split('-')[0]))}
          render={(b) =>
            b.key === '—' ? (
              <span className="text-muted">No issue</span>
            ) : (
              <Link to="/issue/$key" params={{ key: b.key }} className="hover:underline">
                <span className="text-muted">{b.key}</span> {b.label}
              </Link>
            )
          }
        />
        {report.by_issue.length > topIssues.length && (
          <div className="border-t border-line px-4 py-2 text-xs text-subtle">
            +{report.by_issue.length - topIssues.length} more
          </div>
        )}
      </Section>
    </div>
  )
}

/** Agents' time from their runs (claim to done), kept apart from people's. */
export function AgentTimePanel({ report }: { report: TimeReport | undefined }) {
  if (!report || !report.agent_seconds) return null
  return (
    <Section
      title={
        <span className="flex items-center gap-1.5">
          <Bot className="size-3.5 text-accent" /> Agent time
          <span className="font-normal text-muted">· {duration(report.agent_seconds)}</span>
        </span>
      }
      help="How long agents held tasks, from claiming one to finishing or letting it go. Not counted in your hours."
      helpId="section:agent-time"
    >
      <Bars
        items={report.by_agent ?? []}
        color={(b) => hashColor(b.key)}
        render={(b) => (
          <span className="font-medium" title={b.key}>
            {agentLabel(b.label || b.key)}
          </span>
        )}
      />
    </Section>
  )
}
