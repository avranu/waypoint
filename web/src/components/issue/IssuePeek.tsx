import { AvatarStack } from '@/components/ui/misc'
import { relativeTime } from '@/lib/format'
import { PRIORITIES, type Issue } from '@/lib/types'
import { PriorityIcon, StatusIcon, TypeIcon, typeLabel } from './icons'

/** What an issue is, at a glance: the hover card on search results and issue pickers (WP-610). */
export function IssuePeek({ issue: i }: { issue: Issue }) {
  return (
    <div className="space-y-2">
      <div className="flex items-center gap-2 text-xs text-subtle">
        <TypeIcon type={i.type} />
        <span className="font-medium tabular-nums text-muted">{i.key}</span>
        <span>{typeLabel(i.type)}</span>
        <span className="ml-auto truncate">{i.project_key}</span>
      </div>
      <div className="font-medium leading-snug text-fg">{i.title}</div>
      {i.excerpt && (
        <p className="line-clamp-6 whitespace-pre-line break-words text-[12.5px] leading-relaxed text-muted">
          {i.excerpt}
        </p>
      )}
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted">
        <span className="inline-flex items-center gap-1.5">
          <StatusIcon category={i.status.category} color={i.status.color} />
          {i.status.name}
        </span>
        {i.priority > 0 && (
          <span className="inline-flex items-center gap-1.5">
            <PriorityIcon priority={i.priority} />
            <span className="capitalize">{PRIORITIES[i.priority] ?? i.priority}</span>
          </span>
        )}
        {i.assignees.length > 0 && <AvatarStack users={i.assignees} size={18} />}
        <span className="ml-auto text-subtle">updated {relativeTime(i.updated_at)}</span>
      </div>
      {i.labels.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {i.labels.map((l) => (
            <span key={l.id} className="inline-flex items-center gap-1 text-[11px] text-muted">
              <span className="size-1.5 rounded-full" style={{ background: l.color }} />
              {l.name}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

/** An issue as a picker row, with its hover card. */
export function issuePickItem(i: Issue) {
  return {
    value: i.key,
    label: `${i.key} ${i.title}`,
    icon: <TypeIcon type={i.type} />,
    hint: <StatusIcon category={i.status.category} color={i.status.color} />,
    peek: () => <IssuePeek issue={i} />,
  }
}
