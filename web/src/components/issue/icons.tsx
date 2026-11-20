import clsx from 'clsx'
import { Bookmark, Bug, CornerDownRight, SquareCheck, Zap } from 'lucide-react'
import type { Category, IssueType } from '@/lib/types'
import { contrastText } from '@/lib/theme'

const typeMeta: Record<IssueType, { icon: typeof Zap; color: string; label: string }> = {
  epic: { icon: Zap, color: '#a855f7', label: 'Epic' },
  story: { icon: Bookmark, color: '#22c55e', label: 'Story' },
  task: { icon: SquareCheck, color: '#3b82f6', label: 'Task' },
  bug: { icon: Bug, color: '#ef4444', label: 'Bug' },
  subtask: { icon: CornerDownRight, color: '#64748b', label: 'Subtask' },
}

export const typeLabel = (t: IssueType) => typeMeta[t]?.label ?? t

export function TypeIcon({ type, className }: { type: IssueType; className?: string }) {
  const m = typeMeta[type] ?? typeMeta.task
  const I = m.icon
  return (
    <I
      aria-label={m.label}
      className={clsx('shrink-0', className ?? 'size-3.5')}
      style={{ color: m.color }}
      strokeWidth={2.25}
    />
  )
}

/** Linear-style priority bars. */
export function PriorityIcon({ priority, className }: { priority: number; className?: string }) {
  const cls = clsx('shrink-0', className ?? 'size-3.5')
  if (priority === 4) {
    return (
      <svg viewBox="0 0 16 16" className={cls} aria-label="Urgent">
        <rect x="1" y="1" width="14" height="14" rx="3.5" fill="#f97316" />
        <rect x="7.1" y="4" width="1.8" height="5" rx=".9" fill="#fff" />
        <circle cx="8" cy="11.3" r="1" fill="#fff" />
      </svg>
    )
  }
  if (priority === 0) {
    return (
      <svg viewBox="0 0 16 16" className={cls} aria-label="No priority">
        {[2, 6.5, 11].map((x) => (
          <rect
            key={x}
            x={x}
            y="7.25"
            width="3"
            height="1.5"
            rx=".75"
            fill="currentColor"
            opacity=".45"
          />
        ))}
      </svg>
    )
  }
  const heights = [5, 8.5, 12]
  return (
    <svg viewBox="0 0 16 16" className={cls} aria-label={['', 'Low', 'Medium', 'High'][priority]}>
      {heights.map((h, i) => (
        <rect
          key={i}
          x={2 + i * 4.5}
          y={14 - h}
          width="3"
          height={h}
          rx="1"
          fill="currentColor"
          opacity={i < priority ? 0.9 : 0.22}
        />
      ))}
    </svg>
  )
}

/** A circle whose fill shows the status category. */
export function StatusIcon({
  category,
  color,
  className,
}: {
  category: Category
  color?: string
  className?: string
}) {
  const c = color || 'currentColor'
  const cls = clsx('shrink-0', className ?? 'size-3.5')
  switch (category) {
    case 'backlog':
      return (
        <svg viewBox="0 0 16 16" className={cls}>
          <circle
            cx="8"
            cy="8"
            r="6"
            fill="none"
            stroke={c}
            strokeWidth="1.6"
            strokeDasharray="2.2 2.1"
          />
        </svg>
      )
    case 'todo':
      return (
        <svg viewBox="0 0 16 16" className={cls}>
          <circle cx="8" cy="8" r="6" fill="none" stroke={c} strokeWidth="1.6" />
        </svg>
      )
    case 'in_progress':
      return (
        <svg viewBox="0 0 16 16" className={cls}>
          <circle cx="8" cy="8" r="6" fill="none" stroke={c} strokeWidth="1.6" />
          <path d="M8 4.5a3.5 3.5 0 0 1 0 7z" fill={c} />
        </svg>
      )
    case 'done':
      return (
        <svg viewBox="0 0 16 16" className={cls}>
          <circle cx="8" cy="8" r="7" fill={c} />
          <path
            d="m5 8.2 2 2 4-4.2"
            fill="none"
            stroke="#fff"
            strokeWidth="1.7"
            strokeLinecap="round"
            strokeLinejoin="round"
          />
        </svg>
      )
    case 'canceled':
      return (
        <svg viewBox="0 0 16 16" className={cls}>
          <circle cx="8" cy="8" r="7" fill={c} opacity=".85" />
          <path
            d="m5.6 5.6 4.8 4.8m0-4.8-4.8 4.8"
            stroke="#fff"
            strokeWidth="1.6"
            strokeLinecap="round"
          />
        </svg>
      )
  }
}

export function ProjectIcon({
  icon,
  color,
  name,
  size = 20,
}: {
  icon?: string
  color: string
  name: string
  size?: number
}) {
  return (
    <span
      className="inline-flex shrink-0 select-none items-center justify-center rounded-md font-semibold"
      style={{
        width: size,
        height: size,
        background: color,
        color: contrastText(color),
        fontSize: icon ? size * 0.62 : size * 0.48,
      }}
    >
      {icon || name.slice(0, 1).toUpperCase()}
    </span>
  )
}
