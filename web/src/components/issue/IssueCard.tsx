import { memo } from 'react'
import { useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import { CalendarDays, GitPullRequest, ListTree } from 'lucide-react'
import { AvatarStack } from '@/components/ui/misc'
import { PriorityIcon, StatusIcon, TypeIcon } from './icons'
import { LabelChip } from './pickers'
import { isDelegated } from '@/lib/agents'
import { AgentChips, ClaimedBy, activeClaim } from './agentPickers'
import { q } from '@/lib/queries'
import { dueState, issueHover, points, shortDate } from '@/lib/format'
import { isClosed, type Issue } from '@/lib/types'
import { activate } from '@/lib/hooks'
import { DraftBadge } from '@/components/Drafts'
import { HoldBadge } from '@/components/Hold'
import { CloseReviewBadge } from '@/components/Provenance'

/** Prefetch an issue's detail when the pointer lands on it. */
export function usePrefetchIssue() {
  const qc = useQueryClient()
  return (key: string) => {
    qc.prefetchQuery({ ...q.issue(key), staleTime: 15_000 })
    qc.prefetchQuery({ ...q.comments(key), staleTime: 15_000 })
  }
}

export const IssueCard = memo(function IssueCard({
  issue,
  onOpen,
  dragging,
  showStatus,
  selected,
}: {
  issue: Issue
  onOpen: (key: string) => void
  dragging?: boolean
  showStatus?: boolean
  selected?: boolean
}) {
  const prefetch = usePrefetchIssue()
  const closed = isClosed(issue.status.category)
  const due = dueState(issue.due_date, closed)
  return (
    <div
      role="button"
      tabIndex={0}
      data-nav-key={issue.key}
      onClick={() => onOpen(issue.key)}
      onKeyDown={(e) => activate(e) && onOpen(issue.key)}
      onPointerEnter={() => prefetch(issue.key)}
      onTouchStart={() => prefetch(issue.key)}
      className={clsx(
        'group cursor-pointer select-none rounded-lg border bg-raised p-2.5 shadow-card transition-[border-color,box-shadow,transform]',
        dragging
          ? 'rotate-[1.5deg] border-accent shadow-pop'
          : 'border-line hover:border-line-strong',
        selected && 'ring-2 ring-accent',
        closed && !dragging && 'opacity-70',
      )}
    >
      <div className="mb-1 flex items-center gap-1.5 text-[11px] text-subtle">
        <TypeIcon type={issue.type} className="size-3" />
        <span className="font-medium tabular-nums">{issue.key}</span>
        {showStatus && (
          <span className="ml-1 flex items-center gap-1">
            <StatusIcon
              category={issue.status.category}
              color={issue.status.color}
              className="size-3"
            />
            {issue.status.name}
          </span>
        )}
        <span className="flex-1" />
        <PriorityIcon
          priority={issue.priority}
          className={clsx(
            'size-3.5',
            issue.priority !== 4 && 'text-muted',
            issue.priority === 0 && 'opacity-0 group-hover:opacity-100',
          )}
        />
      </div>
      <div
        title={issueHover(issue)}
        className={clsx(
          'line-clamp-3 text-[13px] font-medium leading-snug',
          closed && 'line-through decoration-subtle',
        )}
      >
        {issue.title}
      </div>
      {issue.draft && <DraftBadge outside={issue.outside} className="mt-1.5 inline-flex" />}
      {issue.hold && <HoldBadge hold={issue.hold} className="mt-1.5 inline-flex" />}
      {closed && issue.close_unreviewed && (
        <CloseReviewBadge agent={issue.resolved_agent} className="mt-1.5 inline-flex" />
      )}
      {(issue.labels.length > 0 ||
        (isDelegated(issue.agent) && !activeClaim(issue)) ||
        issue.step) && (
        <div className="mt-1.5 flex flex-wrap gap-1">
          <AgentChips issue={issue} />
          {issue.labels.map((l) => (
            <LabelChip key={l.id} label={l} />
          ))}
        </div>
      )}
      {(issue.parent ||
        issue.due_date ||
        issue.points !== null ||
        issue.child_count > 0 ||
        issue.assignees.length > 0 ||
        activeClaim(issue)) && (
        <div className="mt-2 flex items-center gap-2 text-[11px] text-muted">
          {issue.parent && (
            <span
              className="flex min-w-0 items-center gap-1 truncate"
              title={`${issue.parent.key} ${issue.parent.title}`}
            >
              <TypeIcon type={issue.parent.type} className="size-3" />
              <span className="truncate">{issue.parent.title}</span>
            </span>
          )}
          {issue.due_date && (
            <span
              className={clsx(
                'flex shrink-0 items-center gap-1',
                due === 'overdue' && 'font-medium text-danger',
                due === 'today' && 'font-medium text-warn',
              )}
            >
              <CalendarDays className="size-3" />
              {shortDate(issue.due_date)}
            </span>
          )}
          {issue.child_count > 0 && (
            <span className="flex shrink-0 items-center gap-1" title="Subtasks done">
              <ListTree className="size-3" />
              {issue.child_done}/{issue.child_count}
            </span>
          )}
          {issue.points !== null && (
            <span className="shrink-0 rounded bg-sunken px-1 font-medium tabular-nums">
              {points(issue.points)}
            </span>
          )}
          <span className="flex-1" />
          <ClaimedBy issue={issue} />
          <AvatarStack users={issue.assignees} size={18} />
        </div>
      )}
    </div>
  )
})

export function PRBadge({ state }: { state: string }) {
  const color =
    state === 'merged'
      ? '#a855f7'
      : state === 'open'
        ? '#22c55e'
        : state === 'draft'
          ? '#94a3b8'
          : '#ef4444'
  return (
    <span className="inline-flex items-center gap-1 text-[11px] font-medium" style={{ color }}>
      <GitPullRequest className="size-3" />
      {state || 'linked'}
    </span>
  )
}
