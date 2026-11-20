// Pieces shared by the audit lists and the audit page (WP-348).

import { Link } from '@tanstack/react-router'
import clsx from 'clsx'
import { Loader2, ShieldAlert, ShieldCheck, ShieldOff, ShieldQuestion } from 'lucide-react'
import { relativeTime, dateTime, plural } from '@/lib/format'
import {
  modelShort,
  SEVERITY,
  STATUS,
  type AuditCounts,
  type AuditStatus,
  type CodeAudit,
} from '@/lib/code-audits'
import { Badge } from '@/components/ui/misc'

export function AuditStatusBadge({ status }: { status: AuditStatus }) {
  const s = STATUS[status]
  return (
    <Badge color={s.color}>
      {status === 'running' && <Loader2 className="size-3 animate-spin" />}
      {s.label}
    </Badge>
  )
}

export function AuditIcon({ audit: a, className }: { audit: CodeAudit; className?: string }) {
  const cls = clsx('size-4 shrink-0', className)
  if (a.status === 'queued' || a.status === 'running')
    return <ShieldQuestion className={clsx(cls, 'text-accent')} />
  if (a.status !== 'done') return <ShieldOff className={clsx(cls, 'text-subtle')} />
  if (a.counts.open > 0) return <ShieldAlert className={clsx(cls, 'text-warn')} />
  return <ShieldCheck className={clsx(cls, 'text-ok')} />
}

/** Severity chips: "2 high · 1 low", or nothing. */
export function SeverityChips({ counts: c }: { counts: AuditCounts }) {
  const parts = (['critical', 'high', 'medium', 'low'] as const).filter((k) => c[k] > 0)
  if (!parts.length) return null
  return (
    <span className="inline-flex flex-wrap items-center gap-1">
      {parts.map((k) => (
        <Badge key={k} color={SEVERITY[k].color}>
          {c[k]} {k}
        </Badge>
      ))}
    </span>
  )
}

/** What an audit came to, in a few words. */
export function auditOutcome(a: CodeAudit): string {
  switch (a.status) {
    case 'queued':
      return a.retry_at
        ? `every model was busy; trying again ${relativeTime(a.retry_at)}`
        : 'waiting for your runner'
    case 'running':
      return `${modelShort(a.model || a.models[a.slot] || '')} is reading`
    case 'done':
      if (a.attempts.at(-1)?.outcome === 'nothing') return 'nothing to read'
      if (!a.counts.total) return 'no findings'
      return a.counts.open
        ? `${plural(a.counts.total, 'finding')}, ${a.counts.open} to do`
        : `${plural(a.counts.total, 'finding')}, all decided`
    case 'limited':
      return 'every model was out of quota'
    case 'failed':
      return a.error ? firstLine(a.error) : 'failed'
    case 'cancelled':
      return 'cancelled'
  }
}

const firstLine = (s: string) => {
  const l = s.split('\n')[0]
  return l.length > 120 ? l.slice(0, 117) + '…' : l
}

/** One audit in a list: links to its page. */
export function AuditRow({ audit: a, onPR }: { audit: CodeAudit; onPR?: boolean }) {
  const who = a.requested_by?.display_name || a.agent || a.automation
  return (
    <li>
      <Link
        to="/audits/$id"
        params={{ id: String(a.id) }}
        className="flex items-start gap-3 px-3 py-2.5 hover:bg-hover"
      >
        <AuditIcon audit={a} className="mt-0.5" />
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="min-w-0 truncate text-[13px] font-medium">
              {onPR ? `Audit by ${modelShort(a.model || a.models[0] || 'a model')}` : a.title}
            </span>
            <AuditStatusBadge status={a.status} />
            <SeverityChips counts={a.counts} />
          </div>
          <div className="flex flex-wrap items-center gap-x-1.5 text-xs text-subtle">
            <span className="font-mono">#{a.id}</span>
            <span>·</span>
            <span>{auditOutcome(a)}</span>
            {who && (
              <>
                <span>·</span>
                <span>{who}</span>
              </>
            )}
            <span>·</span>
            <span title={dateTime(a.created_at)}>{relativeTime(a.created_at)}</span>
          </div>
        </div>
      </Link>
    </li>
  )
}
