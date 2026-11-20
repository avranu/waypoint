// Where a task's work stands on its way to production (WP-389): the stage
// icons and words the What's live page and the issue page share.

import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  AlertTriangle,
  Bot,
  CheckCircle2,
  CircleDashed,
  GitMerge,
  GitPullRequest,
  Lock,
} from 'lucide-react'
import { deliveryQuery } from '@/lib/queries'
import {
  itemForTask,
  type DeliveryItem,
  type DeliveryRun,
  type DeliveryStage,
} from '@/lib/delivery'
import { sinceTime } from '@/lib/environments'
import type { IssueDetail } from '@/lib/types'

/** The stage's icon; `decorative` beside text that already names the stage. */
export function StageIcon({
  stage,
  className,
  decorative,
}: {
  stage: DeliveryStage
  className?: string
  decorative?: boolean
}) {
  const c = clsx('size-4 shrink-0', className)
  const label = (l: string) => (decorative ? { 'aria-hidden': true } : { 'aria-label': l })
  switch (stage) {
    case 'live':
      return <CheckCircle2 className={clsx(c, 'text-ok')} {...label('Live')} />
    case 'merged':
      return <GitMerge className={clsx(c, 'text-warn')} {...label('Merged, not live')} />
    case 'stuck':
      return <AlertTriangle className={clsx(c, 'text-danger')} {...label('Needs someone')} />
    case 'review':
      return <GitPullRequest className={clsx(c, 'text-accent')} {...label('On its way')} />
    case 'working':
      return <Bot className={clsx(c, 'text-muted')} {...label('Being worked on')} />
    case 'blocked':
      return <Lock className={clsx(c, 'text-muted')} {...label('Blocked')} />
    default:
      return <CircleDashed className={clsx(c, 'text-subtle')} {...label('Not tracked')} />
  }
}

/** "Live in production since 14:02", or where it's missing. */
export function runsText(it: DeliveryItem): string {
  const on = it.envs.filter((r: DeliveryRun) => r.in)
  if (it.stage === 'live' && on.length) {
    const r = on.find((x) => x.kind === 'production') ?? on[0]
    return `Live in ${r.name}${r.since ? ` since ${sinceTime(r.since)}` : ''}`
  }
  return it.detail
}

const TONE: Record<DeliveryStage, string> = {
  live: 'var(--wp-ok)',
  merged: 'var(--wp-warn)',
  stuck: 'var(--wp-danger)',
  review: 'var(--wp-accent)',
  working: 'var(--wp-line-strong)',
  blocked: 'var(--wp-line-strong)',
  untracked: 'var(--wp-line-strong)',
}

/**
 * On a task with shipped work: "Live in production since 14:02", or what it
 * still waits on, so a person knows whether they can try it yet.
 */
export function DeliveryBanner({ issue, className }: { issue: IssueDetail; className?: string }) {
  const shipped = issue.links.some((l) => l.kind === 'pull')
  const d = useQuery({ ...deliveryQuery(), enabled: shipped }).data
  const it = shipped ? itemForTask(d, issue.key) : undefined
  if (!it) return null
  return (
    <div
      role="note"
      className={clsx(
        'flex flex-wrap items-center gap-x-3 gap-y-1 rounded-lg border px-3 py-2 text-[13px]',
        className,
      )}
      style={{
        borderColor: `color-mix(in oklab, ${TONE[it.stage]} 55%, transparent)`,
        background: `color-mix(in oklab, ${TONE[it.stage]} 7%, transparent)`,
      }}
    >
      <StageIcon stage={it.stage} />
      <span className="min-w-0 flex-1">{runsText(it)}</span>
      <Link to="/live" className="text-xs text-muted hover:underline">
        What’s live
      </Link>
    </div>
  )
}
