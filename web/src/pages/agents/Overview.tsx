import { lazy, Suspense, useMemo, useState, type ReactNode } from 'react'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import clsx from 'clsx'
import {
  BookOpen,
  Bot,
  ChevronDown,
  CircleDot,
  FileText,
  GitBranch,
  GitPullRequest,
  HardDrive,
  LayoutDashboard,
  MessageSquareText,
  PencilRuler,
  Server,
} from 'lucide-react'
import {
  CHANGE_KINDS,
  changesQuery,
  overviewQuery,
  type Change,
  type ChangeKind,
  type Overview,
  type OverviewBranch,
  type OverviewItem,
  type SummaryList,
} from '@/lib/overview'
import { agentLabel } from '@/lib/agents'
import { q } from '@/lib/queries'
import { duration, plainText, plural, relativeTime } from '@/lib/format'
import { useLocal } from '@/lib/hooks'
import { pendingText, type AgentSession } from '@/lib/sessions'
import { ReviewChip } from './ReviewChip'
import { AppLink } from '@/components/AppLink'
import {
  ContextSize,
  StateDot,
  TaskSessionChip,
  modelLabel,
  sessionName,
  sessionTokens,
} from '@/components/sessions'
import { UsageSummary } from '@/components/UsageSummary'
import { WaitingOnYou } from '@/components/WaitingOnYou'
import { Button } from '@/components/ui/button'
import { Empty, Skeleton } from '@/components/ui/misc'
import { MergeFacts } from './Pulls'
import type { HubTabProps } from './search'

// The request sheet loads when one is opened.
const RequestSheet = lazy(() => import('../Request').then((m) => ({ default: m.RequestSheet })))

export function OverviewTab({ filters }: HubTabProps) {
  const f = { agent: filters.agent, project: filters.project, days: filters.days }
  const { data: ov, error, isPlaceholderData } = useQuery(overviewQuery(f))
  const [request, setRequest] = useState<string | null>(null)
  if (error && !ov)
    return (
      <div className="rounded-xl border border-dashed border-line">
        <Empty icon={<LayoutDashboard />} title="Couldn’t load the overview">
          {error instanceof Error ? error.message : String(error)}
        </Empty>
      </div>
    )
  if (!ov) return <OverviewSkeleton />
  return (
    <div className={clsx('space-y-4 transition-opacity', isPlaceholderData && 'opacity-60')}>
      {ov.waiting.total > 0 || (ov.waiting.closes ?? 0) > 0 ? (
        <WaitingOnYou w={ov.waiting} onOpen={setRequest} className="" />
      ) : (
        <p className="flex items-center gap-2 rounded-xl border border-line bg-panel px-4 py-3 text-[13px] text-muted">
          <MessageSquareText className="size-4 shrink-0 text-subtle" />
          <span>
            <b className="font-medium text-fg">Nothing waits on you.</b> Questions, blocked changes
            and approvals from your agents show up here.
          </span>
        </p>
      )}
      <SummaryCard ov={ov} onRequest={setRequest} />
      <SessionsNow ov={ov} />
      <div className="grid gap-4 lg:grid-cols-2">
        <UpNext ov={ov} filters={filters} />
        <PullRequests ov={ov} />
      </div>
      <Branches ov={ov} />
      <RemoteBranchesFold project={filters.project} />
      <div className="grid gap-4 lg:grid-cols-2">
        <RecentChanges ov={ov} filters={filters} />
        <div className="flex min-w-0 flex-col gap-4">
          <DoneRecently ov={ov} />
          <ResolvedRequests ov={ov} />
        </div>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <EnvironmentsStrip ov={ov} />
        <UsageSummary />
      </div>
      {request && (
        <Suspense fallback={null}>
          <RequestSheet code={request} onClose={() => setRequest(null)} />
        </Suspense>
      )}
    </div>
  )
}

function OverviewSkeleton() {
  return (
    <div className="space-y-4" aria-busy>
      <Skeleton className="h-24 w-full" />
      <Skeleton className="h-40 w-full" />
      <Skeleton className="h-48 w-full" />
    </div>
  )
}

/** A panel that folds; the fold is remembered per person. */
function Fold({
  id,
  title,
  count,
  action,
  empty,
  children,
}: {
  id: string
  title: string
  count?: number
  action?: ReactNode
  /** What fills this section, when it has nothing. */
  empty?: ReactNode
  children?: ReactNode
}) {
  const [folded, setFolded] = useLocal(`agents-ov-${id}`, false)
  const isEmpty = count === 0
  return (
    <section
      className="min-w-0 overflow-hidden rounded-xl border border-line bg-panel"
      aria-label={title}
    >
      <div className="flex h-10 items-center gap-2 pr-3">
        <button
          onClick={() => setFolded(!folded)}
          aria-expanded={!folded}
          className="flex h-full min-w-0 flex-1 items-center gap-2 px-4 text-left text-[13px] font-semibold"
        >
          <ChevronDown
            className={clsx(
              'size-4 shrink-0 text-subtle transition-transform',
              folded && '-rotate-90',
            )}
          />
          <span className="truncate">{title}</span>
          {count !== undefined && (
            <span className="text-xs font-normal tabular-nums text-subtle">{count}</span>
          )}
        </button>
        {action}
      </div>
      {!folded && (
        <div className="border-t border-line">
          {isEmpty && empty ? (
            <p className="px-4 py-3 text-[13px] text-muted">{empty}</p>
          ) : (
            children
          )}
        </div>
      )}
    </section>
  )
}

const rowClass = 'flex min-w-0 items-start gap-3 px-4 py-2.5 hover:bg-hover'
const subtleLink = 'text-xs text-muted hover:text-fg hover:underline'

const KIND_ICON: Record<string, ReactNode> = {
  issue: <CircleDot />,
  pr: <GitPullRequest />,
  request: <MessageSquareText />,
  session: <Bot />,
  review: <BookOpen />,
  design: <PencilRuler />,
}

function SummaryCard({ ov, onRequest }: { ov: Overview; onRequest: (ref: string) => void }) {
  const s = ov.summary
  const cols: { id: string; title: string; list: SummaryList; empty: string; sub?: string }[] = [
    { id: 'doing', title: 'Doing now', list: s.doing, empty: 'No agent is working right now.' },
    {
      id: 'done',
      title: 'Done',
      list: s.done,
      empty: 'Issues agents close and pull requests they merge show up here.',
      sub: `last ${ov.days} days`,
    },
    {
      id: 'deferred',
      title: 'Deferred',
      list: s.deferred,
      empty: 'Work agents filed for later, snoozed requests and pull requests waiting to merge.',
      sub: `last ${ov.days} days`,
    },
    {
      id: 'waiting',
      title: 'Waiting on you',
      list: s.waiting,
      empty: 'Nothing needs a person.',
    },
  ]
  return (
    <Fold id="summary" title="Summary">
      <div className="grid divide-y divide-line sm:grid-cols-2 sm:divide-y-0 lg:grid-cols-4 lg:divide-x [&>*:nth-child(n+3)]:sm:border-t [&>*:nth-child(n+3)]:sm:border-line lg:[&>*:nth-child(n+3)]:border-t-0 sm:[&>*:nth-child(2n)]:border-l sm:[&>*:nth-child(2n)]:border-line lg:[&>*:nth-child(2n)]:border-l-0">
        {cols.map((c) => (
          <div key={c.id} className="min-w-0 p-4" data-summary={c.id}>
            <div className="flex items-baseline gap-2">
              <h3 className="text-xs font-semibold uppercase tracking-wide text-subtle">
                {c.title}
              </h3>
              {c.sub && <span className="text-[11px] text-subtle">{c.sub}</span>}
            </div>
            <div className="mt-1 text-2xl font-semibold tabular-nums tracking-tight">
              {c.list.count}
            </div>
            {c.list.parts.length > 0 && (
              <p className="mt-0.5 text-xs text-muted">
                {c.list.parts.map((p) => p.label).join(' · ')}
              </p>
            )}
            {c.id === 'done' && s.resolved.count > 0 && (
              <p className="mt-0.5 text-xs text-subtle">
                Not counted: {s.resolved.parts.map((p) => p.label).join(' · ')} by people
              </p>
            )}
            {c.list.items.length === 0 ? (
              <p className="mt-2 text-[13px] text-muted">{c.empty}</p>
            ) : (
              <ul className="mt-2 space-y-1.5">
                {c.list.items.map((it, n) => (
                  <li key={n}>
                    <SummaryItem it={it} onRequest={c.id === 'waiting' ? onRequest : undefined} />
                  </li>
                ))}
              </ul>
            )}
          </div>
        ))}
      </div>
    </Fold>
  )
}

function SummaryItem({ it, onRequest }: { it: OverviewItem; onRequest?: (ref: string) => void }) {
  const body = (
    <>
      <span className="mt-0.5 shrink-0 text-subtle [&>svg]:size-3.5">{KIND_ICON[it.kind]}</span>
      <span className="min-w-0">
        <span className="block truncate text-[13px] font-medium">
          {it.ref && it.kind === 'issue' && (
            <span className="mr-1 font-mono text-xs">{it.ref}</span>
          )}
          {plainText(it.title)}
        </span>
        {it.detail && (
          <span className="block truncate text-xs text-muted">{plainText(it.detail)}</span>
        )}
      </span>
    </>
  )
  const cls = 'flex min-w-0 items-start gap-2 rounded-md hover:text-accent'
  if (it.kind === 'request' && onRequest && it.url)
    return (
      <button
        className={clsx(cls, 'w-full text-left')}
        onClick={() => onRequest(it.url!.replace('/requests/', ''))}
      >
        {body}
      </button>
    )
  return it.url ? (
    <AppLink to={it.url} className={cls}>
      {body}
    </AppLink>
  ) : (
    <div className={cls}>{body}</div>
  )
}

interface Group {
  machine: string
  repos: { repo: string; branches: { branch: string; sessions: AgentSession[] }[] }[]
  count: number
}

/** Machine, then repository, then branch; the newest activity first at each level. */
function groupSessions(list: AgentSession[]): Group[] {
  const machines = new Map<string, Map<string, Map<string, AgentSession[]>>>()
  for (const s of list) {
    const repos = machines.get(s.machine) ?? new Map()
    const branches = repos.get(s.repo ?? '') ?? new Map()
    branches.set(s.branch ?? '', [...(branches.get(s.branch ?? '') ?? []), s])
    repos.set(s.repo ?? '', branches)
    machines.set(s.machine, repos)
  }
  return [...machines].map(([machine, repos]) => ({
    machine,
    count: [...repos.values()].reduce(
      (n, b) => n + [...b.values()].reduce((m, l) => m + l.length, 0),
      0,
    ),
    repos: [...repos].map(([repo, branches]) => ({
      repo,
      branches: [...branches].map(([branch, sessions]) => ({ branch, sessions })),
    })),
  }))
}

function SessionsNow({ ov }: { ov: Overview }) {
  const groups = useMemo(() => groupSessions(ov.sessions), [ov.sessions])
  const waiting = ov.sessions.filter((s) => s.state === 'waiting').length
  return (
    <Fold
      id="sessions"
      title="Sessions now"
      count={ov.sessions_total}
      empty={
        <>
          Agent sessions appear here while they run.{' '}
          <code className="rounded bg-sunken px-1">wp hook claude</code> sets that up.
        </>
      }
      action={
        <span className="flex items-center gap-3">
          {waiting > 0 && (
            <span className="hidden items-center gap-1.5 text-xs text-warn sm:inline-flex">
              <StateDot state="waiting" label={false} /> {waiting} waiting for you
            </span>
          )}
          <Link to="/agents" search={{ tab: 'sessions' }} className={subtleLink}>
            All sessions
          </Link>
        </span>
      }
    >
      <div className="divide-y divide-line">
        {groups.map((g) => (
          <div key={g.machine}>
            <h3 className="flex items-center gap-2 bg-sunken/50 px-4 py-1.5 text-xs font-medium text-muted">
              <HardDrive className="size-3.5" />
              <span className="truncate">{g.machine}</span>
              <span className="text-subtle">{plural(g.count, 'session')}</span>
            </h3>
            {g.repos.map((r) =>
              r.branches.map((b) => (
                <div key={r.repo + '\0' + b.branch}>
                  {(r.repo || b.branch) && (
                    <div className="flex min-w-0 items-center gap-1.5 px-4 pt-2 text-xs text-subtle">
                      {r.repo && <span className="truncate">{r.repo}</span>}
                      {b.branch && (
                        <span className="inline-flex min-w-0 items-center gap-1 font-mono text-[11px] text-muted">
                          <GitBranch className="size-3 shrink-0" />
                          <span className="truncate">{b.branch}</span>
                        </span>
                      )}
                    </div>
                  )}
                  <ul>
                    {b.sessions.map((s) => (
                      <li key={s.id}>
                        <SessionRow s={s} />
                      </li>
                    ))}
                  </ul>
                </div>
              )),
            )}
          </div>
        ))}
      </div>
    </Fold>
  )
}

/** One session: its state, what it is called, the task, its model, what it last said, and when. */
export function SessionRow({ s, showWhere = false }: { s: AgentSession; showWhere?: boolean }) {
  const said = plainText((s.last_update || s.last_prompt || '').split('\n')[0])
  return (
    <Link to="/agents/sessions/$id" params={{ id: String(s.id) }} className={rowClass}>
      <StateDot state={s.state} className="mt-[7px]" />
      <span className="min-w-0 flex-1">
        <span className="flex min-w-0 flex-wrap items-baseline gap-x-2">
          <span className="truncate text-[14px] font-medium">{sessionName(s)}</span>
          {s.issue && s.title && (
            <span className="truncate font-mono text-xs text-muted" title={s.issue.title}>
              {s.issue.key}
            </span>
          )}
          <TaskSessionChip s={s} />
          {s.open_requests > 0 && (
            <span className="rounded-full bg-warn/15 px-1.5 text-[11px] font-medium text-warn">
              {s.open_requests === 1 ? 'asks you' : `${s.open_requests} asks`}
            </span>
          )}
          {(s.follow_up?.state === 'armed' || s.follow_up?.state === 'running') && (
            <span
              className="rounded-full bg-sunken px-1.5 text-[11px] font-medium text-muted"
              title="Steps queued for when it finishes"
            >
              {s.follow_up.state === 'running'
                ? 'running its follow-up'
                : `then ${s.follow_up.steps === 1 ? '1 step' : `${s.follow_up.steps} steps`}`}
            </span>
          )}
          {s.review && s.state !== 'ended' && (
            <ReviewChip
              review={s.review}
              title={[s.review.task, s.review.detail].filter(Boolean).join(': ')}
            />
          )}
        </span>
        {said && <span className="block truncate text-[13px] text-muted">{said}</span>}
        <span className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-subtle">
          <span>{[agentLabel(s.agent), modelLabel(s.model)].filter(Boolean).join(' · ')}</span>
          {showWhere && (
            <span className="inline-flex min-w-0 items-center gap-1 font-mono text-[11px]">
              <HardDrive className="size-3 shrink-0" />
              <span className="truncate">
                {[s.machine, s.repo, s.branch].filter(Boolean).join(' · ')}
              </span>
            </span>
          )}
          <ContextSize s={s} />
          {sessionTokens(s) && <span className="tabular-nums">{sessionTokens(s)}</span>}
          {s.state !== 'ended' && pendingText(s) && <span>{pendingText(s)}</span>}
        </span>
      </span>
      <span className="shrink-0 pt-0.5 text-xs text-subtle" title={s.last_seen_at}>
        {relativeTime(s.last_seen_at)}
      </span>
    </Link>
  )
}

function UpNext({ ov, filters }: { ov: Overview; filters: HubTabProps['filters'] }) {
  const queue = {
    tab: 'queue' as const,
    agent: filters.agent || undefined,
    project: filters.project || undefined,
  }
  return (
    <Fold
      id="next"
      title="Up next"
      count={ov.next.reduce((n, g) => n + g.items.length + g.more, 0)}
      empty="Issues delegated to an agent and ready to take, in the order they are taken."
      action={
        <Link to="/agents" search={queue} className={subtleLink}>
          Open the queue
        </Link>
      }
    >
      {ov.claims.length > 0 && (
        <div className="border-b border-line">
          <h3 className="px-4 pt-2.5 text-xs font-medium text-muted">Holding now</h3>
          <ul>
            {ov.claims.map((c) => (
              <li key={c.key}>
                <Link to="/issue/$key" params={{ key: c.key }} className={rowClass}>
                  <StateDot state="working" className="mt-[7px]" />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate text-[13px]">
                      <span className="mr-1.5 font-mono text-xs">{c.key}</span>
                      {c.title}
                    </span>
                    <span className="block truncate text-xs text-subtle">
                      {agentLabel(c.agent)}
                      {c.seconds > 0 && ` · ${duration(c.seconds)} on it`}
                    </span>
                  </span>
                </Link>
              </li>
            ))}
          </ul>
        </div>
      )}
      <div className="divide-y divide-line">
        {ov.next.map((g) => (
          <div key={g.agent || 'none'}>
            <h3 className="flex items-center gap-1.5 px-4 pt-2.5 text-xs font-medium text-muted">
              <Bot className="size-3.5" />
              {g.agent ? agentLabel(g.agent) : 'Anyone'}
            </h3>
            <ul>
              {g.items.map((it) => (
                <li key={it.key}>
                  <Link to="/issue/$key" params={{ key: it.key }} className={rowClass}>
                    <span className="mt-0.5 w-4 shrink-0 text-right text-xs tabular-nums text-subtle">
                      {it.position}
                    </span>
                    <span className="min-w-0 flex-1">
                      <span className="block truncate text-[13px]">
                        <span className="mr-1.5 font-mono text-xs">{it.key}</span>
                        {it.title}
                      </span>
                      <span className="block truncate text-xs text-subtle">{it.why}</span>
                    </span>
                  </Link>
                </li>
              ))}
            </ul>
            {g.more > 0 && (
              <p className="px-4 pb-2 pl-11 text-xs text-subtle">+{g.more} more ready</p>
            )}
          </div>
        ))}
      </div>
    </Fold>
  )
}

function PullRequests({ ov }: { ov: Overview }) {
  const p = ov.pulls
  return (
    <Fold
      id="pulls"
      title="Pull requests"
      count={p.total}
      empty="Open pull requests in your projects’ repositories, with checks, review gate and merge state."
      action={
        <Link to="/agents" search={{ tab: 'pulls' }} className={subtleLink}>
          All pull requests
        </Link>
      }
    >
      <ul className="divide-y divide-line">
        {p.open.map((pr) => (
          <li key={pr.id}>
            <Link to="/agents" search={{ tab: 'pulls', pr: pr.id }} className={rowClass}>
              <GitPullRequest className="mt-0.5 size-4 shrink-0 text-subtle" />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-[13px] font-medium">{pr.title}</span>
                <span className="block truncate text-xs text-subtle">
                  {pr.repo}#{pr.number}
                  {pr.agents[0] && ` · ${agentLabel(pr.agents[0])}`}
                  {pr.state === 'draft' && ' · draft'}
                </span>
                <span className="mt-0.5 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted">
                  <MergeFacts pr={pr} />
                </span>
              </span>
              <span className="shrink-0 pt-0.5 text-xs text-subtle">
                {relativeTime(pr.last_activity)}
              </span>
            </Link>
          </li>
        ))}
      </ul>
      {p.total > p.open.length && (
        <p className="border-t border-line px-4 py-2 text-xs text-subtle">
          {p.total - p.open.length} more on the Pull requests tab.
        </p>
      )}
    </Fold>
  )
}

function Branches({ ov }: { ov: Overview }) {
  return (
    <Fold
      id="branches"
      title="Branches in work"
      count={ov.branches.length}
      empty="Branches with a live session, a recent push or an open pull request."
    >
      <ul className="divide-y divide-line">
        {ov.branches.map((b) => (
          <li key={b.repo + '\0' + b.branch}>
            <BranchRow b={b} />
          </li>
        ))}
      </ul>
    </Fold>
  )
}

/** What Waypoint's daily sweep deletes, and the branches it calls stale. */
function RemoteBranchesFold({ project }: { project: string }) {
  const repos = useQuery(q.remoteBranches(project)).data
  const rows = (repos ?? []).flatMap((r) =>
    r.rows
      .filter((b) => b.verdict === 'removable' || b.verdict === 'stale')
      .map((b) => ({ ...b, repo: r.repo, enabled: r.enabled })),
  )
  if (!rows.length) return null
  return (
    <Fold
      id="remote-branches"
      title="Branches on the forge to tidy"
      count={rows.length}
      empty="Merged branches Waypoint deletes, and stale ones it only lists."
    >
      <ul className="divide-y divide-line">
        {rows.map((b) => (
          <li key={b.repo + '\0' + b.branch} className={rowClass + ' hover:bg-transparent'}>
            <GitBranch className="mt-0.5 size-4 shrink-0 text-subtle" />
            <span className="min-w-0 flex-1">
              <span className="block truncate font-mono text-[13px] font-medium">{b.branch}</span>
              <span className="block text-xs text-subtle">
                {b.repo} · {b.reason}
              </span>
            </span>
            <span
              className={clsx(
                'shrink-0 pt-0.5 text-xs',
                b.verdict === 'stale' ? 'text-muted' : 'text-accent',
              )}
            >
              {b.verdict === 'stale'
                ? 'Stale'
                : b.enabled
                  ? 'Deleted in the next sweep'
                  : 'Deleting is off'}
            </span>
          </li>
        ))}
      </ul>
    </Fold>
  )
}

const CI_LABEL: Record<string, { text: string; cls: string }> = {
  passed: { text: 'Checks passed', cls: 'text-ok' },
  failed: { text: 'Checks failed', cls: 'text-danger' },
  pending: { text: 'Checks running', cls: 'text-muted' },
}

function BranchRow({ b }: { b: OverviewBranch }) {
  const ci = b.ci ? CI_LABEL[b.ci] : undefined
  const body = (
    <>
      <GitBranch className="mt-0.5 size-4 shrink-0 text-subtle" />
      <span className="min-w-0 flex-1">
        <span className="block truncate font-mono text-[13px] font-medium">{b.branch}</span>
        <span className="flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-subtle">
          <span className="truncate">{b.repo}</span>
          {b.pr && (
            <span className="text-muted">
              #{b.pr.number}
              {b.pr.state === 'draft' ? ' draft' : ''}
            </span>
          )}
          {ci && <span className={ci.cls}>{ci.text}</span>}
          {b.pr?.armed && <span className="text-accent">Merges when ready</span>}
          {b.sessions.length > 0 && (
            <span className="inline-flex items-center gap-1 text-muted">
              {b.sessions.map((s) => (
                <StateDot key={s.id} state={s.state} />
              ))}
              {plural(b.sessions.length, 'session')}
            </span>
          )}
        </span>
      </span>
      <span className="shrink-0 pt-0.5 text-xs text-subtle">{relativeTime(b.pushed_at)}</span>
    </>
  )
  if (b.sessions.length === 1)
    return (
      <Link
        to="/agents/sessions/$id"
        params={{ id: String(b.sessions[0].id) }}
        className={rowClass}
      >
        {body}
      </Link>
    )
  if (b.pr)
    return (
      <Link to="/agents" search={{ tab: 'pulls', pr: b.pr.id }} className={rowClass}>
        {body}
      </Link>
    )
  return <div className={clsx(rowClass, 'hover:bg-transparent')}>{body}</div>
}

const CHANGE_ICON: Record<ChangeKind, ReactNode> = {
  issue: <CircleDot />,
  page: <FileText />,
  glossary: <BookOpen />,
  design: <PencilRuler />,
}

function RecentChanges({ ov, filters }: { ov: Overview; filters: HubTabProps['filters'] }) {
  const [kind, setKind] = useState<ChangeKind | ''>('')
  const [more, setMore] = useState(false)
  const paged = kind !== '' || more
  const q = useInfiniteQuery({
    ...changesQuery({ kind, agent: filters.agent, project: filters.project }),
    enabled: paged,
  })
  const list: Change[] = paged
    ? (q.data?.pages.flatMap((p) => p.changes) ?? [])
    : ov.changes.changes
  const hasMore = paged ? !!q.hasNextPage : !!ov.changes.next_before
  return (
    <Fold id="changes" title="Recent changes">
      <div role="radiogroup" aria-label="Kind of change" className="flex flex-wrap gap-1 px-4 py-2">
        {[{ id: '' as const, label: 'All' }, ...CHANGE_KINDS].map((k) => (
          <button
            key={k.id}
            role="radio"
            aria-checked={kind === k.id}
            onClick={() => {
              setKind(k.id)
              setMore(false)
            }}
            className={clsx(
              'h-6 rounded-full px-2.5 text-xs',
              kind === k.id
                ? 'bg-accent-soft font-medium text-fg'
                : 'text-muted hover:bg-hover hover:text-fg',
            )}
          >
            {k.label}
          </button>
        ))}
      </div>
      {paged && q.isPending ? (
        <Skeleton className="mx-4 mb-3 h-16" />
      ) : list.length === 0 ? (
        <p className="border-t border-line px-4 py-3 text-[13px] text-muted">
          {kind
            ? `No ${CHANGE_KINDS.find((k) => k.id === kind)?.label.toLowerCase()} changes yet.`
            : 'Issues, pages, glossary terms and design notes show up here as they change, with who and which agent.'}
        </p>
      ) : (
        <ul className="divide-y divide-line border-t border-line">
          {list.map((c, n) => (
            <li key={`${c.kind}-${c.ref}-${c.at}-${n}`}>
              <ChangeRow c={c} />
            </li>
          ))}
        </ul>
      )}
      {hasMore && (
        <div className="border-t border-line px-4 py-2">
          <Button
            size="xs"
            variant="ghost"
            loading={q.isFetchingNextPage || (paged && q.isPending)}
            onClick={() => (paged ? q.fetchNextPage() : setMore(true))}
          >
            Show more
          </Button>
        </div>
      )}
    </Fold>
  )
}

function ChangeRow({ c }: { c: Change }) {
  const who = c.agent
    ? c.by
      ? `${agentLabel(c.agent)} for ${c.by.display_name}`
      : agentLabel(c.agent)
    : (c.by?.display_name ?? 'Someone')
  return (
    <AppLink to={c.url} className={rowClass}>
      <span className="mt-0.5 shrink-0 text-subtle [&>svg]:size-4">{CHANGE_ICON[c.kind]}</span>
      <span className="min-w-0 flex-1">
        <span className="line-clamp-2 text-[13px] sm:line-clamp-1">
          <span className="font-medium">{who}</span> {c.verb}{' '}
          <span className={clsx(c.kind === 'issue' && 'font-mono text-xs')}>{c.ref}</span>
          {c.ref !== c.title && <span className="text-muted"> {c.title}</span>}
        </span>
        {c.project && <span className="block truncate text-xs text-subtle">{c.project}</span>}
      </span>
      <span className="shrink-0 pt-0.5 text-xs text-subtle">{relativeTime(c.at)}</span>
    </AppLink>
  )
}

function DoneRecently({ ov }: { ov: Overview }) {
  return (
    <Fold
      id="done"
      title="Done recently"
      count={ov.done.length}
      empty={`Issues agents closed and pull requests they merged in the last ${ov.days} days.`}
    >
      <ul className="divide-y divide-line">
        {ov.done.map((it, n) => (
          <li key={n}>
            {it.url ? (
              <AppLink to={it.url} className={rowClass}>
                <DoneBody it={it} />
              </AppLink>
            ) : (
              <div className={rowClass}>
                <DoneBody it={it} />
              </div>
            )}
          </li>
        ))}
      </ul>
    </Fold>
  )
}

/** Requests people decided: named for the person, not the agent that asked. */
function ResolvedRequests({ ov }: { ov: Overview }) {
  return (
    <Fold
      id="resolved"
      title="Requests resolved"
      count={ov.resolved.length}
      empty={`Questions answered and changes approved in the last ${ov.days} days.`}
    >
      <ul className="divide-y divide-line">
        {ov.resolved.map((it, n) => (
          <li key={n}>
            <AppLink to={it.url ?? '/requests'} className={rowClass}>
              <DoneBody it={it} />
            </AppLink>
          </li>
        ))}
      </ul>
    </Fold>
  )
}

function DoneBody({ it }: { it: OverviewItem }) {
  const what =
    it.kind === 'request'
      ? `${it.detail ?? 'Resolved'}${it.agent ? ` · asked by ${agentLabel(it.agent)}` : ''}`
      : `${it.kind === 'issue' ? 'Closed' : 'Merged'}${it.agent ? ` by ${agentLabel(it.agent)}` : ''}`
  return (
    <>
      <span className="mt-0.5 shrink-0 text-ok [&>svg]:size-4">{KIND_ICON[it.kind]}</span>
      <span className="min-w-0 flex-1">
        <span className="block truncate text-[13px]">
          {it.ref && <span className="mr-1.5 font-mono text-xs">{it.ref}</span>}
          {it.title}
        </span>
        <span className="block truncate text-xs text-subtle">{what}</span>
      </span>
      <span className="shrink-0 pt-0.5 text-xs text-subtle">{relativeTime(it.at)}</span>
    </>
  )
}

function EnvironmentsStrip({ ov }: { ov: Overview }) {
  const e = ov.environments
  const down = e?.projects.flatMap((p) => p.down_names ?? []) ?? []
  return (
    <Link
      to="/environments"
      className="block rounded-xl border border-line bg-panel p-3 hover:bg-hover focus-visible:outline focus-visible:outline-2 focus-visible:outline-accent"
    >
      <div className="mb-1.5 flex items-center gap-1.5 text-xs font-medium text-muted">
        <Server className="size-3.5" /> Environments
      </div>
      {!e || e.total === 0 ? (
        <p className="text-[13px] text-muted">
          None yet. Add one under a project’s settings to see what runs where.
        </p>
      ) : (
        <>
          <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px]">
            <Count n={e.up} label="up" dot="bg-ok" />
            {e.degraded > 0 && <Count n={e.degraded} label="degraded" dot="bg-warn" />}
            {e.down > 0 && <Count n={e.down} label="down" dot="bg-danger" />}
            {e.unknown > 0 && <Count n={e.unknown} label="unknown" dot="bg-subtle" />}
          </p>
          {down.length > 0 && (
            <p className="mt-1 truncate text-xs text-danger">Down: {down.join(', ')}</p>
          )}
        </>
      )}
    </Link>
  )
}

function Count({ n, label, dot }: { n: number; label: string; dot: string }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className={clsx('size-2 rounded-full', dot)} />
      <b className="tabular-nums">{n}</b> {label}
    </span>
  )
}
