// A task's life so far, at a glance (WP-1196): one track from when it was
// written to now, coloured by the status it was in, with marks where
// something happened (relations found, follow-ups filed, pull requests,
// edits, comments). It replaces a three-step bar that only repeated the
// status picker, and holds the "Created … · Written on v…" line.

import { useMemo, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  CornerDownRight,
  GitMerge,
  GitPullRequest,
  Hourglass,
  Link2,
  MessageSquare,
  Pencil,
  UserPlus,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { dateTime, dateTimeYear, relativeTime } from '@/lib/format'
import { agentLabel } from '@/lib/agents'
import type { Category, Issue, IssueDetail, Project, WEvent } from '@/lib/types'

/** The colour a status category is drawn in, everywhere a status fills a bar. */
export const CATEGORY_FILL: Record<Category, string> = {
  backlog: 'bg-line-strong',
  todo: 'bg-subtle/60',
  in_progress: 'bg-accent',
  done: 'bg-ok',
  canceled: 'bg-muted/50',
}

/** "40s", "12m", "5h", "3d", "6w": a span of time in one short unit. */
export function shortSpan(ms: number): string {
  const s = Math.max(0, ms / 1000)
  if (s < 60) return `${Math.round(s)}s`
  if (s < 3600) return `${Math.round(s / 60)}m`
  if (s < 86400 * 2) return `${Math.round(s / 3600)}h`
  if (s < 86400 * 14) return `${Math.round(s / 86400)}d`
  return `${Math.round(s / 86400 / 7)}w`
}

/** "v0.52.0", else the short commit. */
function releaseLabel(version?: string, commit?: string): string {
  return version || (commit ?? '').slice(0, 7)
}

type MarkKind = 'followup' | 'relation' | 'waits' | 'pr' | 'merged' | 'edit' | 'comment' | 'assign'

const MARKS: Record<MarkKind, { icon: ReactNode; one: string; many: string; tone: string }> = {
  followup: {
    icon: <CornerDownRight />,
    one: 'follow-up filed',
    many: 'follow-ups filed',
    tone: 'text-accent',
  },
  relation: { icon: <Link2 />, one: 'relation', many: 'relations', tone: 'text-muted' },
  waits: { icon: <Hourglass />, one: 'blocker', many: 'blockers', tone: 'text-warn' },
  pr: {
    icon: <GitPullRequest />,
    one: 'pull request',
    many: 'pull requests',
    tone: 'text-accent',
  },
  merged: { icon: <GitMerge />, one: 'merge', many: 'merges', tone: 'text-ok' },
  edit: { icon: <Pencil />, one: 'edit', many: 'edits', tone: 'text-muted' },
  comment: { icon: <MessageSquare />, one: 'comment', many: 'comments', tone: 'text-muted' },
  assign: { icon: <UserPlus />, one: 'assignment', many: 'assignments', tone: 'text-muted' },
}
const ORDER: MarkKind[] = [
  'followup',
  'relation',
  'waits',
  'pr',
  'merged',
  'edit',
  'comment',
  'assign',
]

interface Mark {
  kind: MarkKind
  at: number
  text: string
}

/** What an event adds to the track, if anything. */
function markOf(e: WEvent): Mark | null {
  const at = Date.parse(e.at)
  switch (e.kind) {
    case 'external.pr_opened':
      return { kind: 'pr', at, text: 'Pull request opened' }
    case 'external.pr_merged':
      return { kind: 'merged', at, text: 'Pull request merged' }
    case 'issue.commented':
      return { kind: 'comment', at, text: `${e.actor?.display_name ?? 'Someone'} commented` }
    case 'issue.assigned':
      return { kind: 'assign', at, text: `Assigned @${e.new}` }
    case 'issue.updated':
      if (e.field === 'relation' && e.new) {
        const [kind, ...rest] = e.new.split(' ')
        const what = rest.join(' ')
        if (kind === 'followed_by') return { kind: 'followup', at, text: `Follow-up ${what}` }
        if (kind === 'blocked_by') return { kind: 'waits', at, text: `Blocked by ${what}` }
        return { kind: 'relation', at, text: `${kind.replace(/_/g, ' ')} ${what}` }
      }
      if (e.field === 'description' || e.field === 'intent' || e.field === 'title')
        return {
          kind: 'edit',
          at,
          text: `${e.actor?.display_name ?? 'Someone'} edited the ${e.field === 'intent' ? 'intent' : e.field}`,
        }
  }
  return null
}

interface Segment {
  name: string
  category: Category
  from: number
  to: number
}

/** History returns the newest this many events (internal/api routes.go). */
const HISTORY_LIMIT = 200

/**
 * The statuses it passed through, oldest first, from status changes and the
 * status it has now, from `start` on: when the history is cut short, start is
 * its oldest event, since what it was before then isn't known.
 */
function segments(
  events: WEvent[],
  issue: IssueDetail,
  project: Project,
  start: number,
  end: number,
): Segment[] {
  const cat = (name: string): Category =>
    project.statuses?.find((s) => s.name === name)?.category ??
    (name === issue.status.name ? issue.status.category : 'todo')
  const changes = events
    .filter((e) => e.kind === 'issue.status_changed')
    .map((e) => ({ at: Date.parse(e.at), old: e.old ?? '', now: e.new ?? '' }))
    .sort((a, b) => a.at - b.at)
  const out: Segment[] = []
  let name = changes[0]?.old || issue.status.name
  let from = start
  for (const c of changes) {
    const at = Math.min(Math.max(c.at, start), end)
    if (at > from) out.push({ name, category: cat(name), from, to: at })
    name = c.now
    from = at
  }
  out.push({ name: issue.status.name, category: issue.status.category, from, to: end })
  return out.filter((s) => s.to > s.from)
}

/** The track from written to now (or closed), with what happened along it. */
export function Lifecycle({ issue, project }: { issue: IssueDetail; project: Project }) {
  const history = useQuery(q.history(issue.key))
  const events = useMemo(() => history.data ?? [], [history.data])
  const closed = issue.status.category === 'done' || issue.status.category === 'canceled'
  const start = Date.parse(issue.created_at)
  const end = Math.max(
    start + 1,
    closed && issue.resolved_at ? Date.parse(issue.resolved_at) : Date.now(),
  )
  const width = end - start
  const pct = (t: number) => Math.min(100, Math.max(0, ((t - start) / width) * 100))
  // A long history comes back cut to its newest events: the track before the
  // oldest one stays blank rather than guess the status it was in.
  const known =
    events.length >= HISTORY_LIMIT
      ? Math.min(end, Math.max(start, Math.min(...events.map((e) => Date.parse(e.at)))))
      : start
  const segs = useMemo(
    () => segments(events, issue, project, known, end),
    [events, issue, project, known, end],
  )
  const marks = useMemo(
    () =>
      events
        .map(markOf)
        .filter((m): m is Mark => !!m)
        .sort((a, b) => a.at - b.at),
    [events],
  )
  // Marks close together share one spot, so the track never turns into a smear.
  const SLOTS = 32
  const slots = new Map<number, Mark[]>()
  for (const m of marks) {
    const s = Math.round((pct(m.at) / 100) * SLOTS)
    slots.set(s, [...(slots.get(s) ?? []), m])
  }
  const counts = ORDER.map((k) => [k, marks.filter((m) => m.kind === k).length] as const).filter(
    ([, n]) => n > 0,
  )
  // Time spent in each status, in the order first reached.
  const spent: { name: string; category: Category; ms: number }[] = []
  for (const s of segs) {
    const row = spent.find((x) => x.name === s.name)
    if (row) row.ms += s.to - s.from
    else spent.push({ name: s.name, category: s.category, ms: s.to - s.from })
  }
  const w = issue.written
  const then = w ? releaseLabel(w.version, w.commit) : ''
  const live = w ? releaseLabel(w.live_version, w.live_commit) : ''
  const since =
    w?.environment && live && live !== then
      ? w.releases_since > 0
        ? `${live}, ${w.releases_since} release${w.releases_since === 1 ? '' : 's'} later`
        : live
      : ''
  const writtenTip = w
    ? `${w.environment ?? 'Production'} ran ${[w.version, w.commit?.slice(0, 7)].filter(Boolean).join(' at ')} when this was written` +
      (since ? `; it runs ${live} now` : w.environment && live ? '; still live' : '')
    : ''
  // Until the history answers, the statuses it passed through aren't known:
  // the track waits rather than paint its whole life in today's status.
  const loading = history.isPending
  const failed = history.isError
  const asker = issue.reporter_agent
    ? agentLabel(issue.reporter_agent)
    : issue.reporter?.display_name
  return (
    <div aria-label="Lifecycle" role="group" className="space-y-1.5">
      <div className="relative h-4" aria-hidden>
        {[...slots.entries()].map(([slot, ms]) => {
          const first = ms[0]
          const m = MARKS[first.kind]
          return (
            <span
              key={slot}
              title={ms
                .map((x) => `${x.text} · ${dateTime(new Date(x.at).toISOString())}`)
                .join('\n')}
              className={clsx(
                'absolute top-0 flex items-center gap-px text-[10px] leading-none [&>svg]:size-3',
                // The ends stay inside the track instead of hanging over it.
                slot === 0 ? '' : slot === SLOTS ? '-translate-x-full' : '-translate-x-1/2',
                m.tone,
              )}
              style={{ left: `${(slot / SLOTS) * 100}%` }}
            >
              {m.icon}
              {ms.length > 1 && <span className="tabular-nums">{ms.length}</span>}
            </span>
          )
        })}
      </div>
      <div
        className={clsx(
          'flex h-2 overflow-hidden rounded-full bg-sunken',
          loading && 'animate-pulse',
        )}
      >
        {!loading && !failed && known > start && (
          <div
            className="h-full"
            style={{ width: `${((known - start) / width) * 100}%` }}
            title={`Earlier history isn't shown here (before ${dateTime(new Date(known).toISOString())})`}
          />
        )}
        {!loading &&
          !failed &&
          segs.map((s, i) => (
            <div
              key={i}
              className={clsx(
                'h-full',
                CATEGORY_FILL[s.category],
                i > 0 && 'border-l border-panel',
              )}
              style={{ width: `${((s.to - s.from) / width) * 100}%` }}
              title={`${s.name} · ${shortSpan(s.to - s.from)} (${dateTime(new Date(s.from).toISOString())} to ${dateTime(new Date(s.to).toISOString())})`}
            />
          ))}
      </div>
      <div className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-0.5 text-xs text-subtle">
        <span>
          <span title={dateTimeYear(issue.created_at)}>
            Created {relativeTime(issue.created_at)}
            {asker && <> by {asker}</>}
          </span>
          {then && <span title={writtenTip}> · {then}</span>}
        </span>
        <span>
          {closed && issue.resolved_at ? (
            <span title={dateTimeYear(issue.resolved_at)}>
              {issue.status.name} {relativeTime(issue.resolved_at)}
            </span>
          ) : (
            'now'
          )}
          {since && <span title={writtenTip}> · {since}</span>}
        </span>
      </div>
      {failed && (
        <div className="flex items-center gap-2 text-xs text-muted">
          Couldn't load its history, so the track is empty.
          <button
            type="button"
            className="text-accent hover:underline"
            onClick={() => void history.refetch()}
          >
            Try again
          </button>
        </div>
      )}
      {(spent.length > 1 || counts.length > 0) && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1 pt-0.5 text-xs text-muted">
          {spent.length > 1 &&
            spent.map((s) => (
              <span
                key={s.name}
                className="inline-flex items-center gap-1"
                title={`${shortSpan(s.ms)} in ${s.name}`}
              >
                <span className={clsx('size-2 rounded-full', CATEGORY_FILL[s.category])} />
                {s.name}
                <span className="tabular-nums text-subtle">{shortSpan(s.ms)}</span>
              </span>
            ))}
          {counts.map(([k, n]) => (
            <span
              key={k}
              className={clsx('inline-flex items-center gap-1 [&>svg]:size-3', MARKS[k].tone)}
              title={`${n} ${n === 1 ? MARKS[k].one : MARKS[k].many}`}
            >
              {MARKS[k].icon}
              <span className="tabular-nums" aria-hidden>
                {n}
              </span>
              <span className="sr-only">{`${n} ${n === 1 ? MARKS[k].one : MARKS[k].many}`}</span>
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

/**
 * Sub-issues as steps (WP-1196): one segment per part, filled in its
 * status's colour, finished ones first, so progress reads as steps done.
 */
export function PartsBar({ parts, className }: { parts: Issue[]; className?: string }) {
  const rank: Record<Category, number> = {
    done: 0,
    canceled: 1,
    in_progress: 2,
    todo: 3,
    backlog: 4,
  }
  const sorted = [...parts].sort((a, b) => rank[a.status.category] - rank[b.status.category])
  const done = parts.filter((p) => p.status.category === 'done').length
  return (
    <div
      role="img"
      aria-label={`${done} of ${parts.length} parts done`}
      className={clsx('flex h-2 gap-0.5', className)}
    >
      {sorted.map((p) => (
        <div
          key={p.id}
          title={`${p.key} ${p.title} · ${p.status.name}`}
          className={clsx(
            'h-full min-w-1 flex-1 rounded-sm',
            p.status.category === 'todo' || p.status.category === 'backlog'
              ? 'bg-sunken ring-1 ring-inset ring-line-strong'
              : CATEGORY_FILL[p.status.category],
          )}
        />
      ))}
    </div>
  )
}
