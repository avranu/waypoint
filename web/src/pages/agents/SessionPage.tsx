import { lazy, Suspense, useEffect, useState, type ReactNode } from 'react'
import { Link, useParams } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import {
  ArrowLeft,
  Bot,
  CircleCheck,
  CircleDot,
  Clock,
  FileText,
  GitBranch,
  GitCommitHorizontal,
  GitPullRequest,
  HardDrive,
  Hourglass,
  MessageSquare,
  MessageSquareText,
  Pause,
  Play,
  PowerOff,
  ShieldX,
  Terminal,
  Thermometer,
  TimerOff,
  Upload,
} from 'lucide-react'
import { ApiError } from '@/lib/api'
import { agentLabel } from '@/lib/agents'
import { dateTime, duration, plainText, plural, relativeTime, safeUrl } from '@/lib/format'
import { q } from '@/lib/queries'
import {
  cacheText,
  pendingText,
  sessionQuery,
  type SessionItem,
  type SessionSummary,
  type TimelineEntry,
} from '@/lib/sessions'
import { ReviewChip } from './ReviewChip'
import { sessionUsageQuery, type SessionUsage } from '@/lib/overview'
import { fresh, pct, tok, type UsageBreakdown, type UsageOverview } from '@/lib/usage-analytics'
import { usd } from '@/lib/activity'
import { kindLabel, type WRequest } from '@/lib/requests'
import { AppLink } from '@/components/AppLink'
import { Attachments } from '@/components/attachments/Attachments'
import { requestIcon } from '@/components/request-icons'
import { RequestAge } from '@/components/WaitingOnYou'
import { ContextSize, StatePill, modelLabel, sessionName } from '@/components/sessions'
import { Badge, Empty, Section, Skeleton } from '@/components/ui/misc'
import { StateBadge } from '../Request'
import { Tile, SortTable, type Col } from './usage/shared'
import { BashSection, ToolsSection } from './usage/Tools'
import { FollowUpPanel } from './FollowUp'
import { RefusalsSection } from './usage/Refusals'

const RequestSheet = lazy(() => import('../Request').then((m) => ({ default: m.RequestSheet })))

export function SessionPage() {
  const { id } = useParams({ strict: false }) as { id: string }
  const sid = Number(id)
  const { data: sum, error } = useQuery(sessionQuery(sid))
  useEffect(() => {
    document.title = `${sum ? sessionName(sum.session) : 'Session'} · Waypoint`
  }, [sum])
  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto w-full max-w-5xl px-4 pb-24 pt-4 sm:px-6 sm:pt-5">
        <Link
          to="/agents"
          search={{ tab: 'sessions' }}
          className="mb-2 inline-flex items-center gap-1 text-[13px] text-muted hover:text-fg"
        >
          <ArrowLeft className="size-4" /> Sessions
        </Link>
        {error instanceof ApiError && error.status === 404 ? (
          <Empty icon={<Terminal />} title="Session not found">
            It may belong to someone else’s private work, or the link is wrong.
          </Empty>
        ) : !sum ? (
          <div className="space-y-3">
            <Skeleton className="h-7 w-2/3" />
            <Skeleton className="h-4 w-1/2" />
            <Skeleton className="h-40 w-full" />
          </div>
        ) : (
          <SessionBody sum={sum} />
        )}
      </div>
    </div>
  )
}

function SessionBody({ sum }: { sum: SessionSummary }) {
  const s = sum.session
  const me = useQuery(q.me()).data
  const mine = !!me && (me.id === s.user.id || me.is_admin)
  const own = !!me && me.id === s.user.id
  const pending = pendingText(s)
  const [request, setRequest] = useState<string | null>(null)
  return (
    <div className="space-y-4">
      <header className="space-y-2">
        <div className="flex flex-wrap items-center gap-2">
          <StatePill session={s} />
          {pending && s.state !== 'ended' && (
            <span className="text-xs text-muted" data-testid="session-pending">
              {pending}
            </span>
          )}
          {s.finished_at && s.state !== 'ended' && (
            <span className="text-xs text-muted" title={dateTime(s.finished_at)}>
              finished {relativeTime(s.finished_at)}
            </span>
          )}
          {s.review && s.state !== 'ended' && <ReviewChip review={s.review} withTask />}
          {cacheText(s.cache) && s.state !== 'ended' && (
            <span className="text-xs text-muted" title={s.cache?.note} data-testid="session-cache">
              {cacheText(s.cache)}
            </span>
          )}
          {s.project && <Badge>{s.project.key}</Badge>}
          {s.worktree && <Badge>worktree</Badge>}
          {s.kind === 'task' && <TaskBinding tasks={s.tasks ?? []} />}
        </div>
        <h1 className="text-xl font-semibold leading-snug tracking-tight">{sessionName(s)}</h1>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px] text-muted">
          <span className="inline-flex items-center gap-1">
            <Bot className="size-3.5" />
            {[agentLabel(s.agent), modelLabel(s.model)].filter(Boolean).join(' · ')}
          </span>
          <span className="inline-flex items-center gap-1">
            <HardDrive className="size-3.5" />
            {s.machine}
          </span>
          {(s.repo || s.branch) && (
            <span className="inline-flex min-w-0 items-center gap-1 font-mono text-xs">
              <GitBranch className="size-3.5 shrink-0" />
              <span className="truncate">{[s.repo, s.branch].filter(Boolean).join(' · ')}</span>
            </span>
          )}
          <span title={dateTime(s.started_at)}>Started {relativeTime(s.started_at)}</span>
          <span title={dateTime(s.last_seen_at)}>Last seen {relativeTime(s.last_seen_at)}</span>
          {s.user && <span>for {s.user.display_name}</span>}
          {s.input_tokens + s.output_tokens > 0 && (
            <span className="tabular-nums">{tok(s.input_tokens + s.output_tokens)} tokens</span>
          )}
          <ContextSize s={s} />
        </div>
        {s.cwd && <p className="truncate font-mono text-xs text-subtle">{s.cwd}</p>}
      </header>

      {own && <FollowUpPanel session={s} />}

      <SummaryGrid sum={sum} onRequest={setRequest} />

      <Requests sum={sum} onOpen={setRequest} />

      <Attachments target={`session:${s.id}`} editable={mine} />

      <UsageSection s={s} />

      <Runs sum={sum} />
      <Timeline entries={sum.timeline} />
      {request && (
        <Suspense fallback={null}>
          <RequestSheet code={request} onClose={() => setRequest(null)} />
        </Suspense>
      )}
    </div>
  )
}

/** A task session (WP-34) and the tasks a launcher bound it to from birth. */
function TaskBinding({ tasks }: { tasks: string[] }) {
  return (
    <span
      className="inline-flex flex-wrap items-center gap-1.5 text-xs text-muted"
      data-testid="session-kind"
    >
      <span
        className="rounded-full bg-accent-soft px-1.5 text-[11px] font-medium text-accent"
        title="Started for these tasks by wp start or a runner; it ends once they are done"
      >
        task session
      </span>
      {tasks.length > 0 && 'for'}
      {tasks.map((k) => (
        <Link key={k} to="/issue/$key" params={{ key: k }} className="font-mono hover:underline">
          {k}
        </Link>
      ))}
    </span>
  )
}

function Col4({ title, count, children }: { title: string; count?: number; children: ReactNode }) {
  return (
    <div className="min-w-0 p-4">
      <h3 className="flex items-baseline gap-2 text-xs font-semibold uppercase tracking-wide text-subtle">
        {title}
        {count !== undefined && (
          <span className="text-sm font-semibold normal-case text-fg tabular-nums">{count}</span>
        )}
      </h3>
      <div className="mt-2 space-y-1.5 text-[13px]">{children}</div>
    </div>
  )
}

function Items({ items, icon }: { items: SessionItem[]; icon: ReactNode }) {
  return (
    <>
      {items.slice(0, 6).map((it, n) => (
        <div key={n} className="flex min-w-0 items-start gap-2">
          <span className="mt-0.5 shrink-0 text-subtle [&>svg]:size-3.5">{icon}</span>
          <span className="min-w-0">
            {it.url ? (
              <AppLink to={it.url} className="block truncate font-medium hover:text-accent">
                {it.label}
              </AppLink>
            ) : (
              <span className="block truncate font-medium">{it.label}</span>
            )}
            {it.detail && <span className="block truncate text-xs text-muted">{it.detail}</span>}
          </span>
        </div>
      ))}
      {items.length > 6 && <p className="text-xs text-subtle">+{items.length - 6} more</p>}
    </>
  )
}

function SummaryGrid({
  sum,
  onRequest,
}: {
  sum: SessionSummary
  onRequest: (ref: string) => void
}) {
  const d = sum.done
  const nothing = <p className="text-muted">Nothing yet.</p>
  const doneCount =
    d.issues_closed.length +
    d.issues_created.length +
    d.pages.length +
    d.pull_requests.length +
    d.commits.length +
    d.comments
  return (
    <section
      aria-label="Summary"
      className="grid divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel sm:grid-cols-2 sm:divide-y-0 lg:grid-cols-4 lg:divide-x [&>*:nth-child(n+3)]:sm:border-t [&>*:nth-child(n+3)]:sm:border-line lg:[&>*:nth-child(n+3)]:border-t-0 sm:[&>*:nth-child(2n)]:border-l sm:[&>*:nth-child(2n)]:border-line lg:[&>*:nth-child(2n)]:border-l-0"
    >
      <Col4 title="Doing">
        {sum.doing.title && <p className="font-medium">{sum.doing.title}</p>}
        {sum.doing.task && (
          <p>
            <Link
              to="/issue/$key"
              params={{ key: sum.doing.task.key }}
              className="hover:text-accent"
            >
              <span className="mr-1 font-mono text-xs">{sum.doing.task.key}</span>
              {sum.doing.task.title}
            </Link>
          </p>
        )}
        {sum.doing.last_prompt && (
          <p className="line-clamp-3 text-muted" title={sum.doing.last_prompt}>
            <span className="text-subtle">Asked: </span>
            {sum.doing.last_prompt}
          </p>
        )}
        {sum.doing.last_update && (
          <p className="line-clamp-4 whitespace-pre-line text-muted" title={sum.doing.last_update}>
            <span className="text-subtle">Last said: </span>
            {plainText(sum.doing.last_update)}
          </p>
        )}
        {!sum.doing.title &&
          !sum.doing.task &&
          !sum.doing.last_prompt &&
          !sum.doing.last_update &&
          nothing}
      </Col4>
      <Col4 title="Done" count={doneCount}>
        {doneCount === 0 && nothing}
        <Items items={d.issues_closed} icon={<CircleCheck />} />
        <Items items={d.issues_created} icon={<CircleDot />} />
        <Items items={d.pages} icon={<FileText />} />
        <Items items={d.pull_requests} icon={<GitPullRequest />} />
        <Items items={d.commits} icon={<GitCommitHorizontal />} />
        {d.comments > 0 && (
          <p className="flex items-center gap-2 text-muted">
            <MessageSquare className="size-3.5 text-subtle" />
            {plural(d.comments, 'comment')}
          </p>
        )}
      </Col4>
      <Col4 title="Deferred" count={sum.deferred.issues.length + sum.deferred.requests.length}>
        {sum.deferred.issues.length + sum.deferred.requests.length === 0 && (
          <p className="text-muted">
            Work it filed for later and requests you snoozed show up here.
          </p>
        )}
        <Items items={sum.deferred.issues} icon={<Hourglass />} />
        {sum.deferred.requests.map((r) => (
          <RequestLine key={r.id} r={r} onOpen={onRequest} />
        ))}
      </Col4>
      <Col4 title="Waiting on you" count={sum.waiting.length}>
        {sum.waiting.length === 0 && (
          <p className="text-muted">Nothing needs you from this session.</p>
        )}
        {sum.waiting.map((r) => (
          <RequestLine key={r.id} r={r} onOpen={onRequest} />
        ))}
      </Col4>
    </section>
  )
}

function RequestLine({ r, onOpen }: { r: WRequest; onOpen: (ref: string) => void }) {
  return (
    <button
      onClick={() => onOpen(r.ref)}
      className="flex w-full min-w-0 items-start gap-2 text-left hover:text-accent"
    >
      <span className="mt-0.5 shrink-0 text-subtle [&>svg]:size-3.5">{requestIcon[r.kind]}</span>
      <span className="min-w-0 flex-1">
        <span className="block truncate font-medium">{r.title}</span>
        <span className="block truncate text-xs text-muted">{kindLabel[r.kind]}</span>
      </span>
      <RequestAge r={r} />
    </button>
  )
}

function Requests({ sum, onOpen }: { sum: SessionSummary; onOpen: (ref: string) => void }) {
  return (
    <Section title={`Requests · ${sum.requests.length}`}>
      {sum.requests.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-muted">
          Questions, blocked changes and commands this session asks of you appear here.
        </p>
      ) : (
        <ul className="divide-y divide-line">
          {sum.requests.map((r) => (
            <li key={r.id}>
              <button
                onClick={() => onOpen(r.ref)}
                className="flex w-full items-center gap-3 px-4 py-2.5 text-left hover:bg-hover"
              >
                <span className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-sunken text-muted [&>svg]:size-4">
                  {requestIcon[r.kind]}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="block truncate text-[13px] font-medium">{r.title}</span>
                  <span className="block truncate text-xs text-subtle">
                    {r.code} · {kindLabel[r.kind]} · {relativeTime(r.created_at)}
                  </span>
                </span>
                <StateBadge r={r} />
              </button>
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

function UsageSection({ s }: { s: SessionSummary['session'] }) {
  const { data: u, error } = useQuery(sessionUsageQuery(s.id))
  if (error)
    return (
      <Section title="Usage">
        <p className="px-4 py-3 text-[13px] text-muted">Couldn’t load usage.</p>
      </Section>
    )
  if (!u)
    return (
      <Section title="Usage">
        <Skeleton className="m-4 h-24" />
      </Section>
    )
  if (u.private)
    return (
      <Section title="Usage">
        <p className="px-4 py-3 text-[13px] text-muted">
          Usage is private to {s.user.display_name}; admins can see it.
        </p>
      </Section>
    )
  if (!u.has_data)
    return (
      <Section title="Usage">
        <p className="px-4 py-3 text-[13px] text-muted">
          Nothing reported for this session yet.{' '}
          <code className="rounded bg-sunken px-1">wp runner</code> uploads usage every 10 minutes,
          and <code className="rounded bg-sunken px-1">wp record usage --from-transcripts</code>{' '}
          does it by hand.
        </p>
      </Section>
    )
  return <UsageBody u={u} s={s} />
}

const breakdownCols = (noun: string): Col<UsageBreakdown>[] => [
  {
    id: 'name',
    label: noun,
    asc: true,
    value: (r) => r.label.toLowerCase(),
    cell: (r) => <span className="truncate font-medium">{r.label}</span>,
  },
  { id: 'fresh', label: 'Fresh tokens', value: fresh, bar: true, cell: (r) => tok(fresh(r)) },
  {
    id: 'messages',
    label: 'Messages',
    value: (r) => r.messages,
    wide: true,
    cell: (r) => tok(r.messages),
  },
  {
    id: 'hit',
    label: 'Cache hits',
    value: (r) => r.hit_rate ?? -1,
    wide: true,
    cell: (r) => pct(r.hit_rate),
  },
  {
    id: 'cost',
    label: 'API cost',
    value: (r) => r.cost_usd,
    cell: (r) => (r.cost_usd > 0 || !r.unpriced_tokens ? usd(r.cost_usd) : '–'),
  },
]

function UsageBody({ u, s }: { u: SessionUsage; s: SessionSummary['session'] }) {
  const t = u.totals
  const me = useQuery(q.me()).data
  const total = t.input + t.cache_write + t.cache_read
  const o = {
    tools: u.tools,
    bash: u.bash,
    unused: [],
    no_inventory: false,
    refusals: u.refusals,
  } as unknown as UsageOverview
  const filters = {
    agent: '',
    project: '',
    days: 365,
    machine: s.machine,
    session: s.cli_id,
    who: me && me.id !== s.user.id ? s.user.username : undefined,
  }
  return (
    <div className="space-y-4">
      <h2 className="text-xs font-semibold uppercase tracking-wide text-subtle">Usage</h2>
      <dl className="grid grid-cols-2 gap-2 lg:grid-cols-4">
        <Tile
          label="Fresh tokens"
          value={tok(fresh(t))}
          sub={`${tok(t.input)} in · ${tok(t.output)} out · ${tok(t.cache_write)} cached`}
          title="Uncached input, output and tokens written to the cache. Cache re-reads are counted apart."
        />
        <Tile
          label="Cache hit rate"
          value={pct(t.hit_rate)}
          sub={`${tok(t.cache_read)} of ${tok(total)} input read from cache`}
        />
        <Tile label="Messages" value={tok(t.messages)} sub={plural(u.kinds.length, 'agent')} />
        <Tile
          label="API-equivalent cost"
          value={usd(t.cost_usd)}
          sub={
            t.unpriced_tokens ? `${tok(t.unpriced_tokens)} tokens have no price` : 'at list prices'
          }
        />
      </dl>
      <div className="grid gap-4 lg:grid-cols-2">
        <Section title="By model">
          <SortTable
            caption="Usage by model"
            rows={u.models}
            rowKey={(r) => r.key}
            sort="fresh"
            cols={breakdownCols('Model')}
          />
        </Section>
        <Section title="By agent">
          <SortTable
            caption="Usage by agent"
            rows={u.kinds}
            rowKey={(r) => r.key}
            sort="fresh"
            cols={breakdownCols('Agent')}
          />
        </Section>
      </div>
      {u.tools.length > 0 && <ToolsSection o={o} />}
      {u.bash.length > 0 && <BashSection o={o} />}
      {u.refusals.total > 0 && <RefusalsSection o={o} filters={filters} />}
    </div>
  )
}

function Runs({ sum }: { sum: SessionSummary }) {
  if (sum.runs.length === 0) return null
  return (
    <Section title={`Tasks held · ${sum.runs.length}`}>
      <ul className="divide-y divide-line">
        {sum.runs.map((r, n) => (
          <li key={n} className="flex items-center gap-3 px-4 py-2 text-[13px]">
            <AppLink to={`/issue/${r.issue}`} className="min-w-0 flex-1 truncate hover:text-accent">
              <span className="mr-1.5 font-mono text-xs">{r.issue}</span>
              {r.title}
            </AppLink>
            <span className="shrink-0 text-xs text-subtle">
              {r.ended_at
                ? `${r.outcome || 'ended'} · ${duration((new Date(r.ended_at).getTime() - new Date(r.started_at).getTime()) / 1000)}`
                : 'holding'}
            </span>
            <span className="hidden shrink-0 text-xs tabular-nums text-subtle sm:inline">
              {tok(r.input_tokens + r.output_tokens)} tokens
            </span>
          </li>
        ))}
      </ul>
    </Section>
  )
}

const TIMELINE_ICON: Record<string, ReactNode> = {
  started: <Play />,
  prompt: <MessageSquare />,
  update: <MessageSquareText />,
  waiting: <Hourglass />,
  idle: <Pause />,
  ended: <PowerOff />,
  subagent: <Bot />,
  issue_created: <CircleDot />,
  issue_closed: <CircleCheck />,
  comment: <MessageSquare />,
  page: <FileText />,
  design: <FileText />,
  glossary: <FileText />,
  pr_linked: <GitPullRequest />,
  request: <MessageSquareText />,
  claimed: <Clock />,
  released: <Clock />,
  attachment: <Upload />,
  cache: <Thermometer />,
  stalled: <TimerOff />,
  unpushed: <GitPullRequest />,
  refused: <ShieldX />,
}

const TIMELINE_WORD: Record<string, string> = {
  started: 'Session started',
  prompt: 'Prompt',
  update: 'Agent’s last message',
  waiting: 'Waiting for you',
  idle: 'Idle',
  ended: 'Session ended',
  subagent: 'Subagent',
  issue_created: 'Created',
  issue_closed: 'Closed',
  comment: 'Commented on',
  page: 'Wrote page',
  design: 'Wrote design note',
  glossary: 'Defined',
  pr_linked: 'Linked pull request',
  request: 'Asked',
  claimed: 'Took',
  released: 'Released',
  cache: 'Prompt cache',
  stalled: 'No progress from background job',
  unpushed: 'Left commits unpushed',
  refused: 'Refused by auto mode:',
}

// PromptText is a prompt's first line, which opens and closes the whole
// prompt below it (WP-860: what the session was asked is kept whole).
function PromptText({ line, text }: { line: string; text: string }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button
        type="button"
        className="block w-full truncate text-left text-xs text-subtle hover:text-fg"
        title={open ? 'Hide the whole prompt' : 'Show the whole prompt'}
        aria-expanded={open}
        onClick={() => setOpen(!open)}
      >
        {line}
      </button>
      {open && (
        <pre className="mt-1 max-h-72 overflow-auto whitespace-pre-wrap break-words rounded-md bg-sunken px-2 py-1.5 font-mono text-xs text-muted">
          {text}
        </pre>
      )}
    </>
  )
}

function Timeline({ entries }: { entries: TimelineEntry[] }) {
  const [all, setAll] = useState(false)
  const shown = all ? entries : entries.slice(0, 25)
  return (
    <Section title={`Timeline · ${entries.length}`}>
      {entries.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-muted">
          What the session does is listed here, newest first.
        </p>
      ) : (
        <>
          <ol className="divide-y divide-line">
            {shown.map((e) => (
              <li key={e.id} className="flex items-start gap-3 px-4 py-2 text-[13px]">
                <span className="mt-0.5 shrink-0 text-subtle [&>svg]:size-3.5">
                  {TIMELINE_ICON[e.kind] ?? <CircleDot />}
                </span>
                <div className="min-w-0 flex-1">
                  <span className="text-muted">{TIMELINE_WORD[e.kind] ?? e.kind} </span>
                  {e.url && /^https?:/i.test(e.url) ? (
                    <a
                      href={safeUrl(e.url)}
                      target="_blank"
                      rel="noreferrer"
                      className="font-medium hover:text-accent"
                    >
                      {e.label}
                    </a>
                  ) : e.url ? (
                    <AppLink to={e.url} className="font-medium hover:text-accent">
                      {e.label}
                    </AppLink>
                  ) : (
                    <span className="font-medium">{e.label}</span>
                  )}
                  {e.text ? (
                    <PromptText line={e.detail ?? ''} text={e.text} />
                  ) : (
                    e.detail && (
                      <span className="block truncate text-xs text-subtle">{e.detail}</span>
                    )
                  )}
                </div>
                <span className="shrink-0 text-xs text-subtle" title={dateTime(e.at)}>
                  {relativeTime(e.at)}
                </span>
              </li>
            ))}
          </ol>
          {entries.length > shown.length && (
            <div className="border-t border-line px-4 py-2">
              <button
                className="text-xs text-muted hover:text-fg hover:underline"
                onClick={() => setAll(true)}
              >
                Show all {entries.length}
              </button>
            </div>
          )}
        </>
      )}
    </Section>
  )
}
