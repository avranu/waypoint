import clsx from 'clsx'
import { TrendingDown, TrendingUp } from 'lucide-react'
import { groupCounts } from '@/lib/knowledge'
import type { Grounding, GoalDrift } from '@/lib/types'
import { vizClass } from '@/components/charts'
import { GROUP_COLOR, GroundingBar, groupTitle } from '@/components/goals'

/**
 * What each color means, once: the groups in the order of every bar. With
 * `counts`, a number after each; hover a group for what it means (and, for
 * the agent group, what it holds).
 */
export function GroundingLegend({
  counts,
  className,
}: {
  counts: Partial<Record<Grounding, number>>
  className?: string
}) {
  const present = groupCounts(counts)
  return (
    <ul
      className={clsx(
        'flex flex-wrap items-center gap-x-3.5 gap-y-1 text-xs text-muted',
        vizClass,
        className,
      )}
      data-testid="goals-legend"
    >
      {present.map(({ group, n }) => (
        <li
          key={group.id}
          className="flex items-center gap-1.5"
          title={`${group.hint}.\n${groupTitle(group, counts)}`}
        >
          <span
            aria-hidden
            className="size-2.5 rounded-[3px]"
            style={{ background: GROUP_COLOR[group.id] }}
          />
          {group.label}
          <span className="font-medium tabular-nums text-fg">{n}</span>
        </li>
      ))}
    </ul>
  )
}

const total = (c: Partial<Record<Grounding, number>>) =>
  Object.values(c).reduce((a, b) => a + (b ?? 0), 0)

/**
 * Finished tasks this week and last, each as a bar split by group, with the
 * share that traces to an approved goal at the end. Hover for the counts.
 */
export function WeekChart({ d }: { d: GoalDrift }) {
  const rows = [
    { name: 'This week', counts: d.week, share: d.grounded },
    { name: 'Last week', counts: d.previous, share: d.previous_grounded },
  ]
  const trend = d.grounded - d.previous_grounded
  return (
    <div className="space-y-2" data-testid="goals-week" title={d.summary}>
      <div className="flex items-center gap-1.5 text-xs text-muted">
        {trend < -0.05 ? (
          <TrendingDown className="size-3.5 text-warn" aria-label="Drifting" />
        ) : (
          <TrendingUp className="size-3.5 text-ok" aria-label="Steady" />
        )}
        Finished tasks that trace to an approved goal
      </div>
      {rows.map((r) => {
        const n = total(r.counts)
        return (
          <div key={r.name} className="flex items-center gap-2 text-xs">
            <span className="w-16 shrink-0 text-muted">{r.name}</span>
            <div
              className="min-w-0 flex-1"
              title={`${r.name}: ${n} finished${n ? `, ${Math.round(r.share * n)} at an approved goal` : ''}`}
            >
              <GroundingBar counts={r.counts} className="h-3" />
            </div>
            <b className="w-9 shrink-0 text-right font-semibold tabular-nums">
              {n ? `${Math.round(r.share * 100)}%` : '–'}
            </b>
          </div>
        )
      })}
    </div>
  )
}
