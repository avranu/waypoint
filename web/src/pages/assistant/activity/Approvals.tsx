import { useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Ban,
  CheckCircle2,
  ChevronRight,
  CircleAlert,
  CircleCheckBig,
  Clock,
  Hand,
  ShieldCheck,
} from 'lucide-react'
import { approvalsQuery, iso, type Assistant, type GateApproval } from '@/lib/assistant'
import { dateTime, relativeTime } from '@/lib/format'
import { Empty, Skeleton } from '@/components/ui/misc'
import { Details, ErrorBox } from './shared'

const STATUS: Record<string, { label: string; color: string; icon: ReactNode }> = {
  pending: { label: 'Waiting', color: 'var(--wp-warn)', icon: <Hand /> },
  approved: { label: 'Approved', color: 'var(--wp-ok)', icon: <CheckCircle2 /> },
  done: { label: 'Done', color: 'var(--wp-ok)', icon: <CircleCheckBig /> },
  denied: { label: 'Denied', color: 'var(--wp-danger)', icon: <Ban /> },
  failed: { label: 'Failed', color: 'var(--wp-danger)', icon: <CircleAlert /> },
  expired: { label: 'Expired', color: 'var(--wp-subtle)', icon: <Clock /> },
}

function StatusBadge({ status }: { status: string }) {
  const s = STATUS[status] ?? { label: status, color: 'var(--wp-subtle)', icon: <ShieldCheck /> }
  return (
    <span
      className="inline-flex h-5 shrink-0 items-center gap-1 rounded-md px-1.5 text-[11px] font-medium [&>svg]:size-3"
      style={{
        background: `color-mix(in oklab, ${s.color} 14%, transparent)`,
        color: `color-mix(in oklab, ${s.color} 85%, var(--wp-fg))`,
      }}
    >
      {s.icon}
      {s.label}
    </span>
  )
}

export function ApprovalsView({ assistant }: { assistant: Assistant }) {
  const q = useQuery({ ...approvalsQuery(assistant.id, 200), refetchInterval: 10_000 })
  const pending = q.data?.pending ?? []
  const pendingCodes = new Set(pending.map((a) => a.code))
  const recent = (q.data?.recent ?? []).filter(
    (a) => a.status !== 'pending' && !pendingCodes.has(a.code),
  )
  return (
    <div className="space-y-5">
      <p className="flex items-center gap-2 text-[13px] text-muted">
        <ShieldCheck className="size-4 shrink-0 text-subtle" />
        <span>
          The gate asks you on Signal; reply “approve <i>code</i>” or “deny <i>code</i>” there.
        </span>
      </p>
      {q.isPending ? (
        <Skeleton className="h-48" />
      ) : q.isError ? (
        <ErrorBox what="approvals" error={q.error} />
      ) : (
        <>
          <section>
            <h3 className="mb-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">
              Waiting · {pending.length}
            </h3>
            {pending.length === 0 ? (
              <div className="rounded-xl border border-dashed border-line px-4 py-3.5 text-[13px] text-muted">
                Nothing is waiting for you.
              </div>
            ) : (
              <ul className="overflow-hidden rounded-xl border border-warn/40 bg-panel">
                {pending.map((a) => (
                  <ApprovalRow key={a.code} a={a} />
                ))}
              </ul>
            )}
          </section>
          <section>
            <h3 className="mb-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">
              Decided
            </h3>
            {recent.length === 0 ? (
              <div className="rounded-xl border border-dashed border-line">
                <Empty icon={<ShieldCheck />} title="No decisions yet">
                  Approvals you give or refuse on Signal show up here with their results.
                </Empty>
              </div>
            ) : (
              <ul className="overflow-hidden rounded-xl border border-line bg-panel">
                {recent.map((a) => (
                  <ApprovalRow key={a.code} a={a} />
                ))}
              </ul>
            )}
          </section>
        </>
      )}
    </div>
  )
}

function ApprovalRow({ a }: { a: GateApproval }) {
  const [open, setOpen] = useState(false)
  const created = iso(a.created)
  const decided = iso(a.decided)
  const head = (
    <>
      <span
        className={clsx(
          'mt-0.5 shrink-0 rounded px-1.5 font-mono text-xs font-semibold leading-5',
          a.status === 'pending' ? 'bg-warn/15 text-warn' : 'bg-sunken text-muted',
        )}
      >
        {a.code}
      </span>
      <div className="min-w-0 flex-1">
        <div className="text-[13px] leading-5 [overflow-wrap:anywhere]">{a.summary}</div>
        <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-subtle">
          <StatusBadge status={a.status} />
          <span className="font-mono">{a.tool}</span>
          {created && (
            <time dateTime={created} title={dateTime(created)}>
              asked {relativeTime(created)}
            </time>
          )}
          {decided && (
            <time dateTime={decided} title={dateTime(decided)}>
              decided {relativeTime(decided)}
            </time>
          )}
        </div>
      </div>
    </>
  )
  const row = 'flex w-full items-start gap-3 px-3 py-2.5 text-left sm:px-4'
  if (!a.result)
    return (
      <li className="border-b border-line last:border-b-0">
        <div className={row}>{head}</div>
      </li>
    )
  return (
    <li className="border-b border-line last:border-b-0">
      <button
        aria-expanded={open}
        onClick={() => setOpen(!open)}
        className={clsx(row, 'min-h-11 hover:bg-hover')}
      >
        {head}
        <span className="mt-0.5 flex shrink-0 items-center gap-0.5 text-xs text-subtle">
          Result
          <ChevronRight className={clsx('size-3.5 transition-transform', open && 'rotate-90')} />
        </span>
      </button>
      {open && (
        <div className="px-3 pb-3 sm:px-4">
          <Details value={a.result} />
        </div>
      )}
    </li>
  )
}
