// Pieces the Activity views share: day headings, an expandable event row,
// a JSON details box (as text), filter chips and source icons.

import { useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import clsx from 'clsx'
import {
  CalendarClock,
  ChevronRight,
  Compass,
  MessageCircle,
  MessagesSquare,
  Monitor,
  Terminal,
  Workflow,
} from 'lucide-react'
import { dateTime } from '@/lib/format'
import { CopyButton } from '@/pages/agents/usage/shared'
import { TONE_CLASS, type Tone } from './kinds'

/** "Today", "Yesterday", "Mon, Oct 19". */
export function dayLabel(ms: number): string {
  const d = new Date(ms)
  const today = new Date()
  const start = (x: Date) => new Date(x.getFullYear(), x.getMonth(), x.getDate()).getTime()
  const diff = Math.round((start(today) - start(d)) / 86_400_000)
  if (diff === 0) return 'Today'
  if (diff === 1) return 'Yesterday'
  return d.toLocaleDateString(undefined, {
    weekday: 'short',
    month: 'short',
    day: 'numeric',
    ...(d.getFullYear() === today.getFullYear() ? {} : { year: 'numeric' }),
  })
}

export const clockTime = (ms: number) =>
  new Date(ms).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' })

export const isoOf = (ms: number) => new Date(ms).toISOString()

/** Items grouped by day, keeping their order. */
export function byDay<T>(items: T[], at: (t: T) => number): [string, T[]][] {
  const m = new Map<string, T[]>()
  for (const it of items) {
    const k = dayLabel(at(it))
    if (!m.has(k)) m.set(k, [])
    m.get(k)!.push(it)
  }
  return [...m.entries()]
}

export function DayHeading({ children }: { children: ReactNode }) {
  return (
    <h3 className="mb-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">{children}</h3>
  )
}

export function ToneIcon({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <span
      className={clsx(
        'flex size-7 shrink-0 items-center justify-center rounded-full [&>svg]:size-3.5',
        TONE_CLASS[tone],
      )}
    >
      {children}
    </span>
  )
}

/** Pretty JSON of an entry, as text, with a copy button. */
export function Details({ value }: { value: unknown }) {
  const text = typeof value === 'string' ? value : JSON.stringify(value, null, 2)
  return (
    <div className="relative mt-2 rounded-lg border border-line bg-sunken">
      <div className="absolute right-1 top-1">
        <CopyButton text={text} />
      </div>
      <pre className="max-h-80 overflow-auto whitespace-pre-wrap break-all p-2.5 pr-20 font-mono text-xs leading-relaxed text-muted">
        {text}
      </pre>
    </div>
  )
}

/**
 * One event: an icon, a sentence, a quieter line and the time. With
 * `details` it opens to show them; with `chat` it links to that session.
 */
export function EventRow({
  icon,
  tone,
  text,
  sub,
  at,
  timeLabel,
  details,
  chat,
  assistantId,
  aside,
}: {
  icon: ReactNode
  tone: Tone
  text: ReactNode
  sub?: ReactNode
  at: number
  /** Shown instead of the clock time (e.g. a full date). */
  timeLabel?: string
  details?: unknown
  /** A session id: the row opens it in the Chat tab. */
  chat?: string
  assistantId: number
  aside?: ReactNode
}) {
  const [open, setOpen] = useState(false)
  const time = (
    <time
      dateTime={at ? isoOf(at) : undefined}
      title={at ? dateTime(isoOf(at)) : undefined}
      className="shrink-0 pt-1 text-xs tabular-nums text-subtle"
    >
      {at ? (timeLabel ?? clockTime(at)) : ''}
    </time>
  )
  const body = (
    <>
      <ToneIcon tone={tone}>{icon}</ToneIcon>
      <div className="min-w-0 flex-1 pt-0.5">
        <div className="text-[13px] leading-5 [overflow-wrap:anywhere]">{text}</div>
        {sub && (
          <div className="mt-0.5 line-clamp-2 text-xs text-subtle [overflow-wrap:anywhere]">
            {sub}
          </div>
        )}
      </div>
      {aside}
      {time}
    </>
  )
  const rowClass =
    'flex w-full items-start gap-3 px-3 py-2.5 text-left sm:px-4 min-h-11 hover:bg-hover'
  if (chat)
    return (
      <li className="border-b border-line last:border-b-0">
        <Link
          to="/assistant/$id"
          params={{ id: String(assistantId) }}
          search={{ s: chat }}
          className={rowClass}
        >
          {body}
          <ChevronRight className="mt-1.5 size-3.5 shrink-0 text-subtle" />
        </Link>
      </li>
    )
  if (details === undefined)
    return (
      <li className="border-b border-line last:border-b-0">
        <div className={clsx(rowClass, 'hover:bg-transparent')}>{body}</div>
      </li>
    )
  return (
    <li className="border-b border-line last:border-b-0">
      <button aria-expanded={open} onClick={() => setOpen(!open)} className={rowClass}>
        {body}
        <ChevronRight
          className={clsx(
            'mt-1.5 size-3.5 shrink-0 text-subtle transition-transform',
            open && 'rotate-90',
          )}
        />
      </button>
      {open && (
        <div className="px-3 pb-3 sm:px-4 sm:pl-14">
          <Details value={details} />
        </div>
      )}
    </li>
  )
}

/** A row of toggle chips with counts. */
export function Chips<T extends string>({
  label,
  options,
  value,
  onChange,
}: {
  label: string
  options: { id: T; label: string; count?: number }[]
  value: T
  onChange: (v: T) => void
}) {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className="no-scrollbar -mx-1 flex max-w-full gap-1 overflow-x-auto px-1"
    >
      {options.map((c) => (
        <button
          key={c.id}
          role="radio"
          aria-checked={value === c.id}
          onClick={() => onChange(c.id)}
          className={clsx(
            'inline-flex h-7 shrink-0 items-center gap-1 rounded-full border px-3 text-xs',
            value === c.id
              ? 'border-accent/50 bg-accent-soft font-medium text-fg'
              : 'border-line text-muted hover:bg-hover hover:text-fg',
          )}
        >
          {c.label}
          {c.count !== undefined && <span className="tabular-nums text-subtle">{c.count}</span>}
        </button>
      ))}
    </div>
  )
}

/** An icon for where a session came from (Hermes's `source`). */
export function SourceIcon({ source, className }: { source: string; className?: string }) {
  const s = source.toLowerCase()
  const c = clsx('shrink-0', className ?? 'size-3.5')
  if (s === 'signal') return <MessageCircle className={c} />
  if (s === 'cron') return <CalendarClock className={c} />
  if (s === 'waypoint') return <Compass className={c} />
  if (s === 'cli' || s === 'tui') return <Terminal className={c} />
  if (s === 'desktop') return <Monitor className={c} />
  if (s === 'subagent' || s === 'tool' || s === 'oneshot') return <Workflow className={c} />
  return <MessagesSquare className={c} />
}

/** Error in place, in the codebase's style. */
export function ErrorBox({ what, error }: { what: string; error: unknown }) {
  return (
    <div className="rounded-xl border border-danger/40 bg-danger/5 px-4 py-3 text-[13px] text-danger">
      Couldn’t load {what}: {error instanceof Error ? error.message : String(error)}
    </div>
  )
}
