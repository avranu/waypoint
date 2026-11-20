// Agents page, Activity tab (WP-D15): what agents did over the period:
// tasks finished, time, tokens and cost, the pull requests that came of it,
// each agent (grouped by family), what the review bots' models did, and the
// latest runs.

import { useMemo, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Bot,
  CircleCheck,
  CircleDashed,
  CirclePause,
  Clock3,
  GitPullRequest,
  Undo2,
} from 'lucide-react'
import {
  activityQuery,
  tokens,
  usd,
  type ActivityRun,
  type AgentActivity,
  type BotActivity,
} from '@/lib/activity'
import { agentLabel } from '@/lib/agents'
import { duration, plural, relativeTime, safeUrl, shortDate } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import { BarChart, DataTable, viz } from '@/components/charts'
import { PRBadge } from '@/components/issue/IssueCard'
import { Empty, Section, Skeleton } from '@/components/ui/misc'
import { shortModel } from '@/components/ModelChain'
import type { HubTabProps } from './search'

export function ActivityTab({ filters, setFilters }: HubTabProps) {
  const aq = useQuery(activityQuery(filters))
  const a = aq.data
  if (aq.error) {
    return (
      <div className="rounded-xl border border-dashed border-line">
        <Empty icon={<Bot />} title="Couldn’t load agent activity">
          {aq.error instanceof Error ? aq.error.message : String(aq.error)}
        </Empty>
      </div>
    )
  }
  if (!a) return <ActivitySkeleton />
  const t = a.totals
  const quiet = a.agents.length === 0 && a.bots.length === 0 && a.runs.length === 0
  if (quiet) {
    return (
      <div className="rounded-xl border border-dashed border-line">
        <Empty icon={<Bot />} title={`No agent work in the last ${filters.days} days`}>
          {filters.agent ? (
            <>
              No agent matching <b>{filters.agent}</b> worked on anything you can see.{' '}
            </>
          ) : null}
          Delegate an issue to an agent (its <b>Agent</b> field), then have the agent run{' '}
          <code className="rounded bg-sunken px-1">wp next</code>. Its time is recorded from claim
          to done; in Claude Code, <code className="rounded bg-sunken px-1">wp hook claude</code>{' '}
          records its tokens and model too.
        </Empty>
      </div>
    )
  }
  const agents = new Set(a.agents.map((x) => x.identity)).size
  return (
    <div className={clsx('space-y-4 transition-opacity', aq.isPlaceholderData && 'opacity-60')}>
      <dl className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5">
        <Tile
          label="Tasks finished"
          value={String(t.tasks_done)}
          sub={plural(t.runs, 'run')}
          title="Tasks agents moved to Done or to their next workflow step"
        />
        <Tile
          label="Agent time"
          value={duration(t.seconds)}
          sub={plural(agents, 'agent')}
          title="Time between taking a task and finishing it, minus idle time; one agent on several tasks at once counts once"
        />
        <Tile
          label="Tokens"
          value={
            t.input_tokens + t.output_tokens > 0
              ? `${tokens(t.input_tokens)} / ${tokens(t.output_tokens)}`
              : '–'
          }
          sub={t.cost_usd > 0 ? `${usd(t.cost_usd)} · in / out` : 'in / out'}
          title="Tokens in and out, as tools reported them (wp hook claude does it for Claude Code)"
        />
        <Tile
          label="Pull requests merged"
          value={String(t.prs_merged)}
          sub={`${t.prs_opened} opened`}
          title="Pull requests linked to agents’ work, merged or opened in the period"
        />
        <Tile
          label="Bot reviews"
          value={String(t.bot_reviews)}
          sub={plural(a.bots.length, 'model')}
          title="Pull request reviews Waypoint’s bots posted"
        />
      </dl>

      <DailyCard days={a.daily} />

      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <AgentsCard
          agents={a.agents}
          active={filters.agent}
          onPick={(agent) => setFilters({ agent })}
        />
        <BotsCard bots={a.bots} />
      </div>

      <RunsCard runs={a.runs} />
    </div>
  )
}

function ActivitySkeleton() {
  return (
    <div className="space-y-4" aria-busy>
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5">
        {Array.from({ length: 5 }, (_, i) => (
          <Skeleton key={i} className="h-[74px]" />
        ))}
      </div>
      <Skeleton className="h-56" />
      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_20rem]">
        <Skeleton className="h-48" />
        <Skeleton className="h-48" />
      </div>
      <Skeleton className="h-72" />
    </div>
  )
}

function Tile({
  label,
  value,
  sub,
  title,
}: {
  label: string
  value: string
  sub?: string
  title?: string
}) {
  return (
    <div
      className="min-w-0 rounded-xl border border-line bg-panel px-3 py-2.5 last:col-span-2 lg:last:col-span-1"
      title={title}
    >
      <dt className="truncate text-xs text-muted">{label}</dt>
      <dd className="truncate text-xl font-semibold tabular-nums tracking-tight">{value}</dd>
      {sub && <dd className="truncate text-xs text-subtle">{sub}</dd>}
    </div>
  )
}

/** Agent time per day: one series, so the title names it and there's no legend. */
function DailyCard({
  days,
}: {
  days: { date: string; seconds: number; tokens: number; done: number }[]
}) {
  const busy = days.some((d) => d.seconds > 0)
  return (
    <Section title="Agent time per day">
      <div className="space-y-3 p-4">
        {busy ? (
          <>
            <BarChart
              ariaLabel="Agent time per day"
              labels={days.map((d) => shortDate(d.date))}
              titles={days.map(
                (d) =>
                  `${new Date(d.date + 'T00:00:00').toLocaleDateString(undefined, {
                    weekday: 'short',
                    month: 'short',
                    day: 'numeric',
                  })}${d.done ? ` · ${plural(d.done, 'task')} finished` : ''}`,
              )}
              unit="hours"
              height={180}
              format={(h) => duration(h * 3600)}
              series={[
                { name: 'Agent time', color: viz.blue, values: days.map((d) => d.seconds / 3600) },
              ]}
            />
            <DataTable
              columns={['Day', 'Agent time', 'Tokens', 'Finished']}
              rows={days.map((d) => [
                shortDate(d.date),
                duration(d.seconds),
                tokens(d.tokens),
                d.done,
              ])}
            />
          </>
        ) : (
          <p className="py-6 text-center text-[13px] text-muted">
            No agent time recorded in this period.
          </p>
        )}
      </div>
    </Section>
  )
}

interface FamilyGroup {
  family: string
  members: AgentActivity[]
  total: AgentActivity
}

/** Identities grouped by family (claude-opus, claude-code), busiest first. */
function groupByFamily(agents: AgentActivity[]): FamilyGroup[] {
  const groups = new Map<string, AgentActivity[]>()
  for (const a of agents) groups.set(a.family, [...(groups.get(a.family) ?? []), a])
  return [...groups.entries()]
    .map(([family, members]) => ({
      family,
      members,
      total: members.reduce(
        (t, m) => ({
          ...t,
          runs: t.runs + m.runs,
          done: t.done + m.done,
          seconds: t.seconds + m.seconds,
          input_tokens: t.input_tokens + m.input_tokens,
          output_tokens: t.output_tokens + m.output_tokens,
          cost_usd: t.cost_usd + m.cost_usd,
          prs_merged: t.prs_merged + m.prs_merged,
          issues_created: t.issues_created + m.issues_created,
          comments: t.comments + m.comments,
          last_active:
            (m.last_active ?? '') > (t.last_active ?? '') ? m.last_active : t.last_active,
        }),
        {
          ...members[0],
          identity: family,
          runs: 0,
          done: 0,
          seconds: 0,
          input_tokens: 0,
          output_tokens: 0,
          cost_usd: 0,
          prs_merged: 0,
          issues_created: 0,
          comments: 0,
          last_active: null as string | null,
        },
      ),
    }))
    .sort((x, y) => y.total.seconds - x.total.seconds || y.total.done - x.total.done)
}

function AgentsCard({
  agents,
  active,
  onPick,
}: {
  agents: AgentActivity[]
  active: string
  onPick: (spec: string) => void
}) {
  const groups = useMemo(() => groupByFamily(agents), [agents])
  const rows: { a: AgentActivity; family?: boolean; nested?: boolean; spec: string }[] = []
  for (const g of groups) {
    if (g.members.length > 1) {
      rows.push({ a: g.total, family: true, spec: g.family })
      for (const m of g.members) rows.push({ a: m, nested: true, spec: m.identity })
    } else rows.push({ a: g.members[0], spec: g.members[0].identity })
  }
  return (
    <Section
      title="Agents"
      className="min-w-0"
      help={
        <>
          Each agent identity, grouped by its model family. Pick one to see only its work on every
          tab. <b>Expired</b> means its claim lapsed or its clock ran out before it finished.
        </>
      }
      helpId="agents:activity-agents"
    >
      {agents.length === 0 ? (
        <p className="px-4 py-6 text-center text-[13px] text-muted">
          No agent worked on a task in this period.
        </p>
      ) : (
        <>
          <table className="hidden w-full text-[13px] sm:table">
            <thead className="text-xs text-muted">
              <tr className="border-b border-line">
                <th className="px-4 py-2 text-left font-medium">Agent</th>
                <th className="px-2 py-2 text-right font-medium">Done</th>
                <th className="px-2 py-2 text-right font-medium">Time</th>
                <th className="px-2 py-2 text-right font-medium">Tokens</th>
                <th className="px-2 py-2 text-right font-medium">Cost</th>
                <th className="px-2 py-2 text-right font-medium" title="Pull requests merged">
                  PRs
                </th>
                <th className="px-4 py-2 text-right font-medium">Last active</th>
              </tr>
            </thead>
            <tbody>
              {rows.map(({ a, family, nested, spec }) => (
                <tr
                  key={(family ? 'f:' : 'a:') + spec}
                  className={clsx(
                    'border-b border-line last:border-0',
                    family && 'bg-sunken/50',
                    active === spec && 'bg-accent-soft',
                  )}
                >
                  <td className={clsx('w-full max-w-0 px-4 py-2', nested && 'pl-8')}>
                    <AgentName a={a} family={family} onPick={() => onPick(spec)} />
                  </td>
                  <td className="whitespace-nowrap px-2 py-2 text-right tabular-nums">
                    {a.done}
                    {a.expired > 0 && (
                      <span
                        className="ml-1 text-xs text-warn"
                        title={`${plural(a.expired, 'run')} expired before finishing`}
                      >
                        +{a.expired}
                      </span>
                    )}
                  </td>
                  <td className="whitespace-nowrap px-2 py-2 text-right tabular-nums">
                    {duration(a.seconds)}
                  </td>
                  <td
                    className="whitespace-nowrap px-2 py-2 text-right tabular-nums"
                    title={`${a.input_tokens.toLocaleString()} in · ${a.output_tokens.toLocaleString()} out`}
                  >
                    {a.input_tokens + a.output_tokens > 0
                      ? tokens(a.input_tokens + a.output_tokens)
                      : '–'}
                  </td>
                  <td className="whitespace-nowrap px-2 py-2 text-right tabular-nums">
                    {a.cost_usd > 0 ? usd(a.cost_usd) : '–'}
                  </td>
                  <td className="whitespace-nowrap px-2 py-2 text-right tabular-nums">
                    {a.prs_merged || '–'}
                  </td>
                  <td className="whitespace-nowrap px-4 py-2 text-right text-xs text-subtle">
                    {a.last_active ? relativeTime(a.last_active) : '–'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <ul className="divide-y divide-line sm:hidden">
            {rows.map(({ a, family, nested, spec }) => (
              <li
                key={(family ? 'f:' : 'a:') + spec}
                className={clsx(
                  'px-4 py-2.5',
                  family && 'bg-sunken/50',
                  nested && 'pl-8',
                  active === spec && 'bg-accent-soft',
                )}
              >
                <AgentName a={a} family={family} onPick={() => onPick(spec)} />
                <div className="mt-0.5 flex flex-wrap gap-x-3 text-xs tabular-nums text-muted">
                  <span>{a.done} done</span>
                  <span>{duration(a.seconds)}</span>
                  {a.input_tokens + a.output_tokens > 0 && (
                    <span>{tokens(a.input_tokens + a.output_tokens)} tokens</span>
                  )}
                  {a.cost_usd > 0 && <span>{usd(a.cost_usd)}</span>}
                  {a.prs_merged > 0 && <span>{plural(a.prs_merged, 'PR')} merged</span>}
                  {a.last_active && (
                    <span className="text-subtle">{relativeTime(a.last_active)}</span>
                  )}
                </div>
              </li>
            ))}
          </ul>
        </>
      )}
    </Section>
  )
}

function AgentName({
  a,
  family,
  onPick,
}: {
  a: AgentActivity
  family?: boolean
  onPick: () => void
}) {
  const writing = [
    a.issues_created > 0 && plural(a.issues_created, 'issue') + ' created',
    a.comments > 0 && plural(a.comments, 'comment'),
  ].filter(Boolean)
  return (
    <div className="min-w-0">
      <button
        onClick={onPick}
        className="flex max-w-full items-center gap-1.5 text-left hover:underline"
        title={family ? `Only agents in the ${a.family} family` : `Only ${a.identity}`}
      >
        <Bot className="size-3.5 shrink-0 text-subtle" />
        {family ? (
          <span className="truncate font-medium">{a.family}</span>
        ) : (
          // The provider is the least telling part, so it stays quiet.
          <span className="truncate">
            {a.identity.includes('/') && (
              <span className="text-subtle">
                {a.identity.slice(0, a.identity.lastIndexOf('/') + 1)}
              </span>
            )}
            {agentLabel(a.identity)}
          </span>
        )}
        {family && <span className="shrink-0 text-xs text-subtle">family</span>}
      </button>
      {!family && (a.models.length > 0 || writing.length > 0) && (
        <div className="truncate pl-5 text-xs text-subtle">
          {[
            a.models.length > 0 && 'on ' + a.models.map((m) => agentLabel(m)).join(', '),
            ...writing,
          ]
            .filter(Boolean)
            .join(' · ')}
        </div>
      )}
    </div>
  )
}

function BotsCard({ bots }: { bots: BotActivity[] }) {
  return (
    <Section
      title="Review bots"
      className="min-w-0"
      help={
        <>
          The models Waypoint’s bots used to review pull requests, from the project’s automations. A
          model that failed (rate limit, timeout, an unusable answer) hands over to the next in its
          chain.
        </>
      }
      helpId="agents:activity-bots"
    >
      {bots.length === 0 ? (
        <p className="px-4 py-6 text-center text-[13px] text-muted">
          No bot reviewed a pull request in this period.
        </p>
      ) : (
        <ul className="divide-y divide-line">
          {bots.map((b) => (
            <li key={b.model} className="px-4 py-2.5">
              <div className="truncate font-mono text-xs" title={b.model}>
                {shortModel(b.model)}
              </div>
              <div className="mt-0.5 flex flex-wrap gap-x-3 text-xs tabular-nums text-muted">
                <span>{plural(b.reviews, 'review')}</span>
                <span>{plural(b.findings, 'finding')}</span>
                {b.failed > 0 && (
                  <span className="text-warn" title="Calls that didn’t answer">
                    {b.failed} failed
                  </span>
                )}
                <span className="text-subtle">{duration(b.seconds)}</span>
              </div>
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

const OUTCOME: Record<
  ActivityRun['outcome'] | 'working',
  { label: string; icon: ReactNode; className: string }
> = {
  working: {
    label: 'working',
    icon: <Bot className="animate-pulse motion-reduce:animate-none" />,
    className: 'text-accent',
  },
  done: { label: 'done', icon: <CircleCheck />, className: 'text-ok' },
  released: { label: 'released', icon: <Undo2 />, className: 'text-muted' },
  expired: { label: 'expired', icon: <Clock3 />, className: 'text-warn' },
  timeout: { label: 'clock stopped', icon: <Clock3 />, className: 'text-warn' },
  paused: { label: 'paused', icon: <CirclePause />, className: 'text-muted' },
  '': { label: 'ended', icon: <CircleDashed />, className: 'text-subtle' },
}

function RunsCard({ runs }: { runs: ActivityRun[] }) {
  useTick(
    1000,
    runs.some((r) => !r.ended_at),
  )
  return (
    <Section title="Latest runs" className="min-w-0">
      {runs.length === 0 ? (
        <p className="px-4 py-6 text-center text-[13px] text-muted">No runs in this period.</p>
      ) : (
        <ul className="divide-y divide-line">
          {runs.map((r) => (
            <RunRow key={r.id} r={r} />
          ))}
        </ul>
      )}
    </Section>
  )
}

function RunRow({ r }: { r: ActivityRun }) {
  const o = OUTCOME[r.ended_at ? r.outcome : 'working'] ?? OUTCOME['']
  const secs = r.ended_at
    ? r.seconds
    : Math.max(r.seconds, (Date.now() - Date.parse(r.started_at)) / 1000)
  const used = r.input_tokens + r.output_tokens
  const model = r.model && agentLabel(r.model) !== agentLabel(r.agent) ? agentLabel(r.model) : ''
  return (
    <li className="flex gap-3 px-4 py-2.5">
      <span
        className={clsx('mt-0.5 shrink-0 [&_svg]:size-4', o.className)}
        title={o.label}
        aria-label={o.label}
      >
        {o.icon}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex min-w-0 items-baseline gap-2">
          <Link
            to="/issue/$key"
            params={{ key: r.issue.key }}
            className="min-w-0 truncate text-[13px] hover:underline"
            title={`${r.issue.key} ${r.issue.title} · ${r.issue.status}`}
          >
            <span className="mr-1.5 font-mono text-xs text-subtle">{r.issue.key}</span>
            {r.issue.title}
          </Link>
          <span
            className="ml-auto shrink-0 text-xs tabular-nums text-muted"
            title={`Started ${new Date(r.started_at).toLocaleString()}`}
          >
            {duration(secs)}
          </span>
        </div>
        <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-subtle">
          <span className="text-muted" title={r.model ? `${r.agent} on ${r.model}` : r.agent}>
            {agentLabel(r.agent)}
            {model && ` (${model})`}
          </span>
          {r.step && <span>· {r.step}</span>}
          <span>· {o.label}</span>
          <span>· {relativeTime(r.ended_at ?? r.started_at)}</span>
          {used > 0 && (
            <span
              className="tabular-nums"
              title={`${r.input_tokens.toLocaleString()} in · ${r.output_tokens.toLocaleString()} out`}
            >
              · {tokens(used)} tokens
            </span>
          )}
          {r.cost_usd > 0 && <span className="tabular-nums">· {usd(r.cost_usd)}</span>}
          {r.prs.map((p) => (
            <a
              key={p.url}
              href={safeUrl(p.url)}
              target="_blank"
              rel="noreferrer"
              title={p.title || p.url}
              className="inline-flex items-center gap-1 rounded-md bg-sunken px-1.5 py-px hover:bg-hover"
            >
              {p.state ? (
                <PRBadge state={p.state} />
              ) : (
                <GitPullRequest className="size-3 text-subtle" />
              )}
              <span className="font-mono text-muted">
                {p.repo.split('/').pop()}#{p.number}
              </span>
            </a>
          ))}
        </div>
      </div>
    </li>
  )
}
