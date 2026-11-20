import { memo } from 'react'
import clsx from 'clsx'
import { ChevronRight, ListTree } from 'lucide-react'
import { AvatarStack } from '@/components/ui/misc'
import { TypeIcon } from './icons'
import { AssigneePicker, LabelChip, PriorityPicker, StatusPicker } from './pickers'
import { usePrefetchIssue } from './IssueCard'
import { AgentChips, ClaimedBy } from './agentPickers'
import { useIssueEdit } from './edit'
import { dueState, issueHover, points, relativeTime, shortDate } from '@/lib/format'
import { isClosed, type Issue, type Project } from '@/lib/types'
import { activate } from '@/lib/hooks'
import { DraftBadge } from '@/components/Drafts'
import { HoldBadge } from '@/components/Hold'
import { CloseReviewBadge } from '@/components/Provenance'

/** One issue in a list, with inline status, priority and assignee editing. */
export const IssueRow = memo(function IssueRow({
  issue,
  project,
  onOpen,
  depth = 0,
  expanded,
  onToggle,
  showProject,
  selected,
  editable = true,
  showUpdated,
  hideParent,
}: {
  issue: Issue
  project?: Project
  onOpen: (key: string) => void
  depth?: number
  expanded?: boolean
  onToggle?: () => void
  showProject?: boolean
  selected?: boolean
  editable?: boolean
  showUpdated?: boolean
  hideParent?: boolean
}) {
  const prefetch = usePrefetchIssue()
  const edit = useIssueEdit(issue, project)
  const closed = isClosed(issue.status.category)
  const due = dueState(issue.due_date, closed)
  const canEdit = editable && !!project && project.role !== 'viewer'
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
        'group flex h-11 cursor-pointer items-center gap-1.5 border-b border-line px-3 text-[13px] hover:bg-hover sm:h-10 sm:px-4',
        selected && 'bg-accent-soft',
      )}
      style={{ paddingLeft: depth ? 12 + depth * 20 : undefined }}
    >
      {onToggle !== undefined && (
        <button
          onClick={(e) => {
            e.stopPropagation()
            onToggle()
          }}
          className={clsx(
            'flex size-5 shrink-0 items-center justify-center rounded text-subtle hover:bg-hover',
            issue.child_count === 0 && 'invisible',
          )}
          aria-label={expanded ? 'Collapse' : 'Expand'}
        >
          <ChevronRight
            className={clsx('size-3.5 transition-transform', expanded && 'rotate-90')}
          />
        </button>
      )}
      <span className="hidden sm:inline-flex">
        <PriorityPicker
          value={issue.priority}
          onChange={edit.priority}
          disabled={!canEdit}
          iconOnly
          variant="bare"
        />
      </span>
      <span
        className={clsx(
          'w-[68px] shrink-0 truncate text-xs tabular-nums text-subtle',
          showProject && 'w-[76px]',
        )}
      >
        {issue.key}
      </span>
      {project ? (
        <StatusPicker
          statuses={project.statuses ?? []}
          value={issue.status}
          onChange={edit.status}
          disabled={!canEdit}
          iconOnly
          variant="bare"
        />
      ) : null}
      <TypeIcon type={issue.type} className="size-3.5 sm:hidden" />
      <span
        title={issueHover(issue)}
        className={clsx(
          'min-w-0 flex-1 truncate',
          closed && 'text-muted line-through decoration-subtle',
        )}
      >
        {issue.title}
      </span>
      {issue.draft && <DraftBadge outside={issue.outside} />}
      {issue.hold && <HoldBadge hold={issue.hold} />}
      {closed && issue.close_unreviewed && <CloseReviewBadge agent={issue.resolved_agent} />}
      <span className="hidden min-w-0 items-center gap-1 md:flex">
        <AgentChips issue={issue} />
        {issue.labels.slice(0, 3).map((l) => (
          <LabelChip key={l.id} label={l} />
        ))}
      </span>
      {issue.parent && !hideParent && (
        <span className="hidden max-w-[180px] items-center gap-1 truncate text-xs text-subtle lg:flex">
          <TypeIcon type={issue.parent.type} className="size-3" />
          <span className="truncate">{issue.parent.title}</span>
        </span>
      )}
      {issue.child_count > 0 && (
        <span className="hidden items-center gap-1 text-xs text-subtle sm:flex">
          <ListTree className="size-3" />
          {issue.child_done}/{issue.child_count}
        </span>
      )}
      {issue.due_date && (
        <span
          className={clsx(
            'w-14 shrink-0 text-right text-xs',
            due === 'overdue'
              ? 'font-medium text-danger'
              : due === 'today'
                ? 'font-medium text-warn'
                : 'text-subtle',
          )}
        >
          {shortDate(issue.due_date)}
        </span>
      )}
      {issue.points !== null && (
        <span className="hidden shrink-0 rounded bg-sunken px-1.5 text-[11px] font-medium tabular-nums text-muted sm:inline">
          {points(issue.points)}
        </span>
      )}
      {showUpdated && (
        <span className="hidden w-24 shrink-0 truncate whitespace-nowrap text-right text-xs text-subtle sm:inline">
          {relativeTime(closed && issue.resolved_at ? issue.resolved_at : issue.updated_at)}
        </span>
      )}
      <ClaimedBy issue={issue} compact />
      <span className="shrink-0">
        {canEdit ? (
          <AssigneePicker
            value={issue.assignees}
            onChange={edit.assignees}
            compact
            variant="bare"
            members={project?.members?.map((m) => m.user.id)}
          />
        ) : (
          <AvatarStack users={issue.assignees} size={20} />
        )}
      </span>
    </div>
  )
})
