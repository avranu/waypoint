import { lazy, Suspense, useEffect, useId, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import clsx from 'clsx'
import {
  AtSign,
  Bell,
  CalendarClock,
  Check,
  CheckCheck,
  ChevronDown,
  ChevronRight,
  Gauge,
  Target,
  GitCommitHorizontal,
  GitPullRequest,
  MessageSquare,
  MessageSquareText,
  MoveRight,
  Server,
  Share2,
  UserPlus,
  Zap,
} from 'lucide-react'
import { api } from '@/lib/api'
import { q } from '@/lib/queries'
import { requestsQuery, waitingQuery } from '@/lib/requests'
import { plural, relativeTime } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Avatar, Badge, Empty, PageHeader, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { IssueSheet } from './Project'
import { AssistantsWaiting, WaitingOnYou } from '@/components/WaitingOnYou'
import type { InboxGroup, InboxView } from '@/lib/types'
import { PauseSessionsButton, useSessionPauses } from './agents/SessionPause'

// The request sheet loads when one is opened.
const RequestSheet = lazy(() => import('./Request').then((m) => ({ default: m.RequestSheet })))

const kindIcon: Record<string, React.ReactNode> = {
  assigned: <UserPlus />,
  mentioned: <AtSign />,
  commented: <MessageSquare />,
  status: <MoveRight />,
  shared: <Share2 />,
  due: <CalendarClock />,
  pr: <GitPullRequest />,
  unpushed: <GitCommitHorizontal />,
  automation: <Zap />,
  request: <MessageSquareText />,
  quota: <Gauge />,
  objective: <Target />,
  server: <Server />,
}

// What a kind is called, singular and plural, for the counts.
const kindName: Record<string, [string, string]> = {
  assigned: ['assignment', 'assignments'],
  mentioned: ['mention', 'mentions'],
  commented: ['comment', 'comments'],
  status: ['status change', 'status changes'],
  shared: ['share', 'shares'],
  due: ['due date', 'due dates'],
  pr: ['pull request update', 'pull request updates'],
  unpushed: ['unpushed work', 'unpushed work'],
  automation: ['automation', 'automations'],
  request: ['request', 'requests'],
  quota: ['quota alert', 'quota alerts'],
  risky: ['risky command', 'risky commands'],
  token: ['sign-in', 'sign-ins'],
  deploy: ['deploy notice', 'deploy notices'],
  audit: ['audit', 'audits'],
  objective: ['objective update', 'objective updates'],
  server: ['server health notice', 'server health notices'],
}
const kindLabel = (k: string, n: number) => (kindName[k] ?? [k, k])[n === 1 ? 0 : 1]

const tiers: { id: InboxGroup['tier']; label: string }[] = [
  { id: 'you', label: 'Asked of you' },
  { id: 'alert', label: 'Alerts' },
  { id: 'update', label: 'Updates' },
]

/** A quota alert's actions (WP-146): pause every agent and running session, or say they are. */
function QuotaActions() {
  const paused = useSessionPauses()
  if (!paused) return null
  const all = paused.find((p) => p.spec === 'any')
  return (
    <div className="flex flex-wrap items-center gap-2 px-4 pb-3 pl-14">
      {all ? (
        <span className="text-xs text-muted">
          Agents and running sessions are paused. Resume them on{' '}
          <Link to="/agents" search={{ tab: 'sessions' }} className="text-accent hover:underline">
            Agents → Sessions
          </Link>
          .
        </span>
      ) : (
        <>
          <PauseSessionsButton />
          <Link
            to="/agents"
            search={{ tab: 'sessions' }}
            className="text-xs text-accent hover:underline"
          >
            See sessions
          </Link>
        </>
      )}
    </div>
  )
}

function dayLabel(iso: string) {
  const d = new Date(iso)
  const today = new Date()
  const y = new Date()
  y.setDate(today.getDate() - 1)
  if (d.toDateString() === today.toDateString()) return 'Today'
  if (d.toDateString() === y.toDateString()) return 'Yesterday'
  return d.toLocaleDateString(undefined, { weekday: 'long', month: 'short', day: 'numeric' })
}

type Section = 'live' | 'stale'

/**
 * "Robin commented on WP-1: text" says who and where twice over in a row
 * under that issue's title; keep "Robin: text".
 */
function tidy(body: string, key?: string, who?: string) {
  if (!key) return body
  const i = body.indexOf(` on ${key}: `)
  if (i < 0) return body
  const rest = body.slice(i + key.length + 5)
  const first = (who ?? body.slice(0, i)).split(' ')[0]
  return first ? `${first}: ${rest}` : rest
}

/** "2 comments, assignment" for a group of several notifications. */
function kindSummary(g: InboxGroup) {
  return Object.entries(g.kinds)
    .sort((a, b) => b[1] - a[1])
    .map(([k, n]) => (n > 1 ? `${n} ${kindLabel(k, n)}` : kindLabel(k, 1)))
    .join(', ')
}

function GroupRow({
  g,
  section,
  onOpen,
  onRead,
}: {
  g: InboxGroup
  section: Section | 'earlier'
  onOpen: (g: InboxGroup) => void
  onRead?: (g: InboxGroup) => void
}) {
  const [open, setOpen] = useState(false)
  const panel = useId()
  const latest = g.items[0]
  const multi = g.count > 1
  const unread = g.unread > 0
  return (
    <li
      data-inbox-group={g.key}
      className={clsx(
        'group border-b border-line last:border-b-0',
        unread && section === 'live' && 'bg-accent-soft/40',
      )}
    >
      <div className="flex items-start">
        <button
          onClick={() => onOpen(g)}
          className="flex min-w-0 flex-1 items-start gap-3 px-4 py-3 text-left hover:bg-hover"
        >
          <span className="relative mt-0.5 shrink-0">
            {(g.actors?.[0] ?? latest?.actor) ? (
              <Avatar user={g.actors?.[0] ?? latest?.actor} size={28} />
            ) : (
              <span className="flex size-7 items-center justify-center rounded-full bg-sunken text-muted [&>svg]:size-3.5">
                {kindIcon[g.kind] ?? <Bell />}
              </span>
            )}
            {(g.actors?.[0] ?? latest?.actor) && (
              <span className="absolute -bottom-1 -right-1 flex size-4 items-center justify-center rounded-full bg-raised text-muted ring-2 ring-panel [&>svg]:size-2.5">
                {kindIcon[g.kind] ?? <Bell />}
              </span>
            )}
          </span>
          <span className="min-w-0 flex-1">
            <span className="line-clamp-2 break-words text-[13px] font-medium">{g.title}</span>
            {multi ? (
              <span className="line-clamp-2 break-words text-[13px] text-muted">
                {kindSummary(g)}
                {latest?.body
                  ? ` · ${tidy(latest.body, g.issue_key, latest.actor?.display_name)}`
                  : ''}
              </span>
            ) : (
              <span className="line-clamp-2 text-[13px] text-muted">
                {latest ? tidy(latest.body, g.issue_key, latest.actor?.display_name) : ''}
              </span>
            )}
            {section === 'stale' && g.reason && <Badge className="mt-1">{g.reason}</Badge>}
          </span>
          <span className="flex shrink-0 flex-col items-end gap-1">
            <span className="whitespace-nowrap text-xs text-subtle">
              {relativeTime(g.latest_at)}
            </span>
            {unread && section !== 'earlier' && (
              <span
                className={clsx(
                  'inline-flex h-4 min-w-4 items-center justify-center rounded-full px-1 text-[10px] font-semibold',
                  section === 'live' ? 'bg-accent text-accent-fg' : 'bg-sunken text-muted',
                )}
                title={`${g.unread} unread`}
              >
                {g.unread}
              </span>
            )}
          </span>
        </button>
        <div className="flex shrink-0 items-center gap-0.5 pr-2 pt-2.5">
          {multi && (
            <button
              onClick={() => setOpen(!open)}
              aria-expanded={open}
              aria-controls={panel}
              aria-label={open ? 'Hide the notifications' : `Show all ${g.count} notifications`}
              title={open ? 'Hide' : `Show all ${g.count}`}
              className="flex size-7 items-center justify-center rounded-md text-subtle hover:bg-hover hover:text-fg"
            >
              {open ? <ChevronDown className="size-4" /> : <ChevronRight className="size-4" />}
            </button>
          )}
          {unread && onRead && (
            <button
              onClick={() => onRead(g)}
              aria-label={`Mark ${g.title} read`}
              title="Mark read"
              className="flex size-7 items-center justify-center rounded-md text-subtle hover:bg-hover hover:text-fg"
            >
              <Check className="size-4" />
            </button>
          )}
        </div>
      </div>
      {open && (
        <ul id={panel} className="border-t border-line bg-sunken/50 py-1 pl-14 pr-4">
          {g.items.map((n) => (
            <li key={n.id} className="flex items-start gap-2 py-1 text-xs">
              <span className="mt-0.5 shrink-0 text-subtle [&>svg]:size-3">
                {kindIcon[n.kind] ?? <Bell />}
              </span>
              <span className="min-w-0 flex-1 text-muted">
                <span className="line-clamp-2">
                  {tidy(n.body, g.issue_key, n.actor?.display_name) || n.title}
                </span>
              </span>
              <span className="shrink-0 whitespace-nowrap text-subtle">
                {relativeTime(n.created_at)}
              </span>
            </li>
          ))}
          {g.count > g.items.length && (
            <li className="py-1 text-xs text-subtle">
              and {plural(g.count - g.items.length, 'earlier one')}
            </li>
          )}
        </ul>
      )}
      {g.kind === 'quota' && section === 'live' && <QuotaActions />}
    </li>
  )
}

function Foldout({
  open,
  onToggle,
  title,
  count,
  hint,
  actions,
  id,
  children,
}: {
  open: boolean
  onToggle: () => void
  title: string
  count: number
  hint?: string
  actions?: React.ReactNode
  id: string
  children: React.ReactNode
}) {
  return (
    <section aria-label={title} data-inbox-section={id}>
      <div className="flex items-center gap-2">
        <h2 className="min-w-0 flex-1">
          <button
            onClick={onToggle}
            aria-expanded={open}
            aria-controls={`inbox-${id}-panel`}
            className="flex w-full min-w-0 items-center gap-1.5 py-1 text-left"
          >
            {open ? (
              <ChevronDown className="size-4 shrink-0 text-subtle" />
            ) : (
              <ChevronRight className="size-4 shrink-0 text-subtle" />
            )}
            <span className="text-xs font-semibold uppercase tracking-wide text-subtle">
              {title}
            </span>
            <span className="text-xs text-subtle">{count.toLocaleString()}</span>
            {hint && (
              <span className="hidden truncate text-xs font-normal normal-case text-subtle sm:inline">
                · {hint}
              </span>
            )}
          </button>
        </h2>
        {actions}
      </div>
      {open && (
        <div id={`inbox-${id}-panel`} className="mt-1.5">
          {children}
        </div>
      )}
    </section>
  )
}

/** The counts by kind, the oldest thing waiting, and what went stale. */
function Summary({
  s,
  kind,
  onKind,
  onStale,
}: {
  s: InboxView['summary']
  kind: string | null
  onKind: (k: string | null) => void
  onStale: () => void
}) {
  const kinds = Object.entries(s.by_kind).sort((a, b) => b[1] - a[1])
  return (
    <aside
      aria-label="Inbox summary"
      className="rounded-xl border border-line bg-panel p-4 lg:sticky lg:top-4"
    >
      <div className="flex items-baseline gap-2">
        <span className="text-2xl font-semibold tabular-nums">
          {s.live_unread.toLocaleString()}
        </span>
        <span className="text-[13px] text-muted">
          unread, across {plural(s.live_groups, 'thing')} that still matter
        </span>
      </div>
      {s.oldest_live && (
        <p className="mt-1 text-xs text-subtle">Oldest waiting: {relativeTime(s.oldest_live)}</p>
      )}
      {kinds.length > 0 && (
        <ul className="mt-3 flex flex-wrap gap-1.5 lg:flex-col lg:gap-0.5" aria-label="By kind">
          {kinds.map(([k, n]) => (
            <li key={k}>
              <button
                onClick={() => onKind(kind === k ? null : k)}
                aria-pressed={kind === k}
                className={clsx(
                  'flex w-full items-center gap-2 rounded-md px-2 py-1 text-left text-xs hover:bg-hover',
                  kind === k ? 'bg-accent-soft text-fg' : 'text-muted',
                )}
              >
                <span className="text-subtle [&>svg]:size-3.5">{kindIcon[k] ?? <Bell />}</span>
                <span className="flex-1">{kindLabel(k, n)}</span>
                <span className="tabular-nums text-fg">{n.toLocaleString()}</span>
              </button>
            </li>
          ))}
        </ul>
      )}
      {s.truncated && (
        <p className="mt-3 text-xs text-subtle">
          Counting the newest notifications only; older unread ones are not included here.
        </p>
      )}
      {s.stale_unread > 0 && (
        <button
          onClick={onStale}
          className="mt-3 w-full rounded-md bg-sunken px-2 py-1.5 text-left text-xs text-muted hover:bg-hover"
        >
          <span className="tabular-nums font-medium text-fg">
            {s.stale_unread.toLocaleString()}
          </span>{' '}
          more went stale by themselves: closed issues, decided requests, old alerts
        </button>
      )}
    </aside>
  )
}

function Loading() {
  return (
    <div className="space-y-3 px-4 sm:px-6" aria-busy="true" aria-label="Loading the inbox">
      <Skeleton className="h-24" />
      <Skeleton className="h-16" />
      <Skeleton className="h-16" />
      <Skeleton className="h-16" />
    </div>
  )
}

export function InboxPage() {
  const qc = useQueryClient()
  const inbox = useQuery(q.inbox())
  const data = inbox.data
  const [sheet, setSheet] = useState<string | null>(null)
  const [request, setRequest] = useState<string | null>(null)
  const [kind, setKind] = useState<string | null>(null)
  const [staleOpen, setStaleOpen] = useState(false)
  const [earlierOpen, setEarlierOpen] = useState(false)
  const waiting = useQuery(waitingQuery()).data
  const [showExpired, setShowExpired] = useState(false)
  const expired = useQuery(requestsQuery({ state: 'expired', limit: 200 })).data
  const expiredRefs = useMemo(() => new Set((expired?.requests ?? []).map((r) => r.ref)), [expired])
  useEffect(() => {
    document.title = 'Inbox · Waypoint'
  }, [])

  const readAll = useMutation({
    mutationFn: () => api.post('/notifications/read', { ids: [], upto: data?.summary.as_of ?? 0 }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['notifications'] }),
    onError: toastError,
  })
  const read = useMutation({
    // Only what was on screen is marked: the server stops at the newest id this
    // view saw, so a notification that arrives meanwhile stays unread.
    mutationFn: (v: { section: Section; group?: string }) =>
      api.post('/inbox/read', { ...v, upto: data?.summary.as_of ?? 0 }),
    onMutate: async (v) => {
      // An in-flight refetch would put the row back over the update below.
      await qc.cancelQueries({ queryKey: ['notifications'] })
      qc.setQueryData<InboxView>(q.inbox().queryKey, (d) => {
        if (!d) return d
        const gone = (g: InboxGroup) => !v.group || g.key === v.group
        const live = v.section === 'live'
        const goneGroups = (live ? d.live : d.stale).filter(gone)
        const n = goneGroups.reduce((c, g) => c + g.unread, 0)
        const by_kind = { ...d.summary.by_kind }
        if (live)
          for (const g of goneGroups)
            for (const [k, c] of Object.entries(g.kinds)) {
              by_kind[k] = (by_kind[k] ?? 0) - c
              if (by_kind[k] <= 0) delete by_kind[k]
            }
        return {
          ...d,
          summary: {
            ...d.summary,
            unread: Math.max(0, d.summary.unread - n),
            live_unread: live ? Math.max(0, d.summary.live_unread - n) : d.summary.live_unread,
            live_groups: live
              ? Math.max(0, d.summary.live_groups - goneGroups.length)
              : d.summary.live_groups,
            stale_unread: live ? d.summary.stale_unread : Math.max(0, d.summary.stale_unread - n),
            by_kind,
          },
          live: live ? d.live.filter((g) => !gone(g)) : d.live,
          stale: live ? d.stale : d.stale.filter((g) => !gone(g)),
        }
      })
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ['notifications'] }),
    onError: toastError,
  })

  const open = (g: InboxGroup, section: Section | 'earlier') => {
    const opens = !!g.issue_key || !!g.url?.startsWith('/requests/')
    if (opens && g.unread > 0 && section !== 'earlier') read.mutate({ section, group: g.key })
    if (g.issue_key) setSheet(g.issue_key)
    else if (g.url?.startsWith('/requests/'))
      setRequest(decodeURIComponent(g.url.slice('/requests/'.length)))
  }

  const isExpired = (g: InboxGroup) =>
    g.kind === 'request' &&
    !!g.url?.startsWith('/requests/') &&
    expiredRefs.has(decodeURIComponent(g.url.slice('/requests/'.length)))
  const live = (data?.live ?? []).filter((g) => !kind || (g.kinds[kind] ?? 0) > 0)
  const earlier = (data?.earlier ?? []).filter((g) => showExpired || !isExpired(g))
  const days = useMemo(() => {
    const m = new Map<string, InboxGroup[]>()
    for (const g of earlier) {
      const k = dayLabel(g.latest_at)
      if (!m.has(k)) m.set(k, [])
      m.get(k)!.push(g)
    }
    return [...m.entries()]
  }, [earlier])
  const s = data?.summary
  const caughtUp = !!s && s.live_unread === 0
  const showSummary = !!s && (s.live_unread > 0 || s.stale_unread > 0)
  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-6xl pb-16">
        <PageHeader
          title="Inbox"
          subtitle={
            s
              ? s.live_unread
                ? `${s.live_unread.toLocaleString()} unread that still matter`
                : 'All caught up'
              : undefined
          }
          actions={
            <>
              {expiredRefs.size > 0 && (
                <Button
                  size="sm"
                  variant={showExpired ? 'primary' : 'secondary'}
                  onClick={() => {
                    setShowExpired(!showExpired)
                    if (!showExpired) setEarlierOpen(true)
                  }}
                  title="Requests the agent moved on from"
                >
                  Expired
                </Button>
              )}
              <Button
                size="sm"
                icon={<CheckCheck className="size-3.5" />}
                onClick={() => readAll.mutate()}
                disabled={!s?.unread}
                title={
                  s?.stale_unread
                    ? `Marks all ${s.unread.toLocaleString()} unread read, stale ones too`
                    : 'Mark all read'
                }
              >
                <span className="max-md:sr-only">Mark all read</span>
              </Button>
            </>
          }
        />
        <AssistantsWaiting />
        {waiting && (waiting.total > 0 || (waiting.closes ?? 0) > 0) && (
          <WaitingOnYou w={waiting} onOpen={setRequest} />
        )}
        {inbox.isPending ? (
          <Loading />
        ) : inbox.isError ? (
          <div
            role="alert"
            className="mx-4 flex flex-wrap items-center gap-2 rounded-xl border border-line bg-panel p-4 text-[13px] sm:mx-6"
          >
            <span className="text-danger">Couldn’t load the inbox: {inbox.error.message}</span>
            <Button size="xs" variant="subtle" onClick={() => void inbox.refetch()}>
              Try again
            </Button>
          </div>
        ) : (
          <div
            className={`grid gap-4 px-4 sm:px-6 lg:items-start ${showSummary ? 'lg:grid-cols-[18rem_minmax(0,1fr)]' : ''}`}
          >
            {(s!.live_unread > 0 || s!.stale_unread > 0) && (
              <Summary s={s!} kind={kind} onKind={setKind} onStale={() => setStaleOpen(true)} />
            )}
            <div className="min-w-0 space-y-5">
              {caughtUp && live.length === 0 && (
                <div className="rounded-xl border border-line bg-panel">
                  <Empty icon={<Bell />} title="Nothing needs you">
                    {data!.earlier.length || s!.stale_unread
                      ? 'Everything unread has been dealt with or went stale.'
                      : 'You’ll hear about issues assigned to you, mentions, comments and status changes on things you watch. Set up Gotify in Settings to get them on your phone.'}
                  </Empty>
                </div>
              )}
              {kind && live.length === 0 && !caughtUp && (
                <p className="text-[13px] text-muted">
                  Nothing unread of that kind.{' '}
                  <button className="text-accent hover:underline" onClick={() => setKind(null)}>
                    Show all
                  </button>
                </p>
              )}
              {tiers.map((t) => {
                const items = live.filter((g) => g.tier === t.id)
                if (!items.length) return null
                return (
                  <section key={t.id} aria-label={t.label} data-inbox-tier={t.id}>
                    <div className="mb-1.5 flex items-center gap-2">
                      <h2 className="text-xs font-semibold uppercase tracking-wide text-subtle">
                        {t.label}
                      </h2>
                      <span className="text-xs text-subtle">{items.length}</span>
                    </div>
                    <ul className="overflow-hidden rounded-xl border border-line bg-panel">
                      {items.map((g) => (
                        <GroupRow
                          key={g.key}
                          g={g}
                          section="live"
                          onOpen={(x) => open(x, 'live')}
                          onRead={(x) => read.mutate({ section: 'live', group: x.key })}
                        />
                      ))}
                    </ul>
                  </section>
                )
              })}
              {s!.live_more > 0 && (
                <p className="text-xs text-subtle">
                  and {plural(s!.live_more, 'more thing')} that still matter, not shown here. Mark
                  some read to see them.
                </p>
              )}
              {s!.stale_unread > 0 && (
                <Foldout
                  id="stale"
                  title="Stale"
                  open={staleOpen}
                  onToggle={() => setStaleOpen(!staleOpen)}
                  count={s!.stale_unread}
                  hint="unread notifications, but the issue closed, the request was decided or the alert is old"
                  actions={
                    <Button
                      size="xs"
                      variant="subtle"
                      icon={<CheckCheck className="size-3" />}
                      onClick={() => read.mutate({ section: 'stale' })}
                      loading={
                        read.isPending &&
                        read.variables?.section === 'stale' &&
                        !read.variables.group
                      }
                    >
                      Mark {s!.stale_unread.toLocaleString()} stale read
                    </Button>
                  }
                >
                  <ul className="overflow-hidden rounded-xl border border-line bg-panel">
                    {data!.stale.map((g) => (
                      <GroupRow
                        key={g.key}
                        g={g}
                        section="stale"
                        onOpen={(x) => open(x, 'stale')}
                        onRead={(x) => read.mutate({ section: 'stale', group: x.key })}
                      />
                    ))}
                  </ul>
                  {s!.stale_more > 0 && (
                    <p className="mt-1.5 text-xs text-subtle">
                      and {plural(s!.stale_more, 'more thing')}
                    </p>
                  )}
                </Foldout>
              )}
              {earlier.length > 0 && (
                <Foldout
                  id="earlier"
                  title="Earlier"
                  open={earlierOpen}
                  onToggle={() => setEarlierOpen(!earlierOpen)}
                  count={earlier.length}
                  hint="things you already read"
                >
                  <div className="space-y-4">
                    {days.map(([day, items]) => (
                      <div key={day}>
                        <h3 className="mb-1 text-xs font-medium text-subtle">{day}</h3>
                        <ul className="overflow-hidden rounded-xl border border-line bg-panel">
                          {items.map((g) => (
                            <GroupRow
                              key={g.key}
                              g={g}
                              section="earlier"
                              onOpen={(x) => open(x, 'earlier')}
                            />
                          ))}
                        </ul>
                      </div>
                    ))}
                  </div>
                </Foldout>
              )}
            </div>
          </div>
        )}
      </div>
      {sheet && <IssueSheet issueKey={sheet} onClose={() => setSheet(null)} />}
      {request && (
        <Suspense fallback={null}>
          <RequestSheet code={request} onClose={() => setRequest(null)} />
        </Suspense>
      )}
    </div>
  )
}
