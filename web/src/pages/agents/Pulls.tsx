// Agents page, Pull requests tab (WP-D15): where pull requests went, how
// many pushes they took, who reviewed and approved them, what bots found,
// and what agents spent on the issues they're linked to. A pull request
// opens in a side sheet (?pr=) with its whole story.

import { useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ArrowRight,
  Bot,
  Check,
  CircleCheck,
  CircleDot,
  CircleX,
  Clock,
  ExternalLink,
  Eye,
  GitCommitHorizontal,
  GitMerge,
  GitPullRequest,
  GitPullRequestClosed,
  GitPullRequestDraft,
  Globe,
  LoaderCircle,
  MessageSquare,
  RotateCcw,
  Rocket,
  Search,
  ShieldAlert,
  ShieldCheck,
  Sparkles,
  X,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { OutsidePR } from './OutsidePR'
import { agentLabel } from '@/lib/agents'
import { dateTime, duration, plural, relativeTime, safeUrl } from '@/lib/format'
import { useDebounced } from '@/lib/hooks'
import {
  compact,
  hoursLabel,
  pullQuery,
  pullsQuery,
  usd,
  type PRState,
  type PRStateFilter,
  type PRSummary,
  type PRTotals,
  type PRTimelineItem,
} from '@/lib/pulls'
import { NAME_FONT, envAtQuery, sinceTime, ago as envAgo, type EnvAt } from '@/lib/environments'
import { Badge, Empty, Skeleton } from '@/components/ui/misc'
import { IconButton } from '@/components/ui/button'
import { useEscapeLayer } from '@/components/ui/layers'
import { BotRunCard, SEVERITY } from '../project-settings/Bots'
import { PullAudits } from './PullAudits'
import { Babysitter } from './Babysitter'
import { Shipping } from './Shipping'
import type { HubTabProps } from './search'

const STATE: Record<PRState, { label: string; color: string; icon: ReactNode }> = {
  open: { label: 'Open', color: 'var(--wp-ok)', icon: <GitPullRequest /> },
  draft: { label: 'Draft', color: 'var(--wp-subtle)', icon: <GitPullRequestDraft /> },
  merged: { label: 'Merged', color: 'var(--wp-accent)', icon: <GitMerge /> },
  closed: { label: 'Closed', color: 'var(--wp-danger)', icon: <GitPullRequestClosed /> },
}

const STATE_FILTERS: { id: PRStateFilter; label: string }[] = [
  { id: '', label: 'All' },
  { id: 'open', label: 'Open' },
  { id: 'merged', label: 'Merged' },
  { id: 'closed', label: 'Closed' },
]

export function PullsTab({ filters, setFilters }: HubTabProps) {
  const search = useSearch({ from: '/app/agents' })
  const navigate = useNavigate({ from: '/agents' })
  const [state, setState] = useState<PRStateFilter>('')
  const [text, setText] = useState('')
  const debounced = useDebounced(text.trim(), 250)
  const list = useQuery(pullsQuery({ ...filters, state, q: debounced }))
  const open = (id: number | undefined) =>
    navigate({ search: (s) => ({ ...s, pr: id }), replace: id === undefined })
  const pulls = list.data?.pulls ?? []
  return (
    <div className="space-y-4">
      <Totals data={list.data?.totals} days={filters.days} loading={list.isPending} />
      <div className="flex flex-wrap items-center gap-2">
        <div
          role="radiogroup"
          aria-label="State"
          className="flex h-8 items-center rounded-lg border border-line p-0.5"
        >
          {STATE_FILTERS.map((f) => (
            <button
              key={f.id}
              role="radio"
              aria-checked={state === f.id}
              onClick={() => setState(f.id)}
              className={clsx(
                'h-full rounded-md px-2.5 text-xs',
                state === f.id ? 'bg-accent-soft font-medium text-fg' : 'text-muted hover:text-fg',
              )}
            >
              {f.label}
            </button>
          ))}
        </div>
        <label className="relative ml-auto w-full sm:w-64">
          <span className="sr-only">Find a pull request</span>
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
          <input
            value={text}
            onChange={(e) => setText(e.target.value)}
            placeholder="Title, repo, #number or author"
            className="h-8 w-full rounded-lg border border-line bg-panel pl-8 pr-2 text-[13px] outline-none placeholder:text-subtle focus:border-accent"
          />
        </label>
      </div>
      {list.isPending ? (
        <div className="space-y-2">
          <Skeleton className="h-20" />
          <Skeleton className="h-20" />
          <Skeleton className="h-20" />
        </div>
      ) : list.isError ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<ShieldAlert />} title="Couldn’t load pull requests">
            {(list.error as Error).message}
          </Empty>
        </div>
      ) : pulls.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<GitPullRequest />} title="No pull requests here">
            {filters.agent || filters.project || state || debounced ? (
              'Nothing matches these filters in this period. Try a longer period, or clear a filter.'
            ) : (
              <>
                Pull requests of the repos attached to your projects show up here, with their
                reviews, approvals and what agents spent on them. Attach a repo in a project’s{' '}
                <b>Settings → Integrations</b>.
              </>
            )}
          </Empty>
        </div>
      ) : (
        <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel">
          {pulls.map((p) => (
            <PullRow
              key={p.id}
              pr={p}
              onOpen={() => open(p.id)}
              onAgent={(a) => setFilters({ agent: a })}
            />
          ))}
        </ul>
      )}
      {search.pr && <PullSheet id={search.pr} onClose={() => open(undefined)} />}
    </div>
  )
}

function Totals({ data, days, loading }: { data?: PRTotals; days: number; loading: boolean }) {
  if (loading && !data)
    return (
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
        {Array.from({ length: 5 }, (_, i) => (
          <Skeleton key={i} className="h-[68px]" />
        ))}
      </div>
    )
  if (!data) return null
  return (
    <dl className="grid grid-cols-2 gap-2 sm:grid-cols-5">
      <Tile label="Opened" value={data.opened} sub={`in ${days} days`} />
      <Tile
        label="Merged"
        value={data.merged}
        sub={
          data.median_hours_to_merge != null
            ? `${hoursLabel(data.median_hours_to_merge)} median to merge`
            : data.closed
              ? `${data.closed} closed unmerged`
              : undefined
        }
      />
      <Tile label="Open now" value={data.open_now} />
      <Tile
        label="Bot reviews"
        value={data.bot_reviews}
        sub={data.findings ? plural(data.findings, 'finding') : undefined}
      />
      <Tile
        label="Approvals by people"
        value={data.human_approvals}
        className="col-span-2 sm:col-span-1"
      />
    </dl>
  )
}

function Tile({
  label,
  value,
  sub,
  className,
}: {
  label: string
  value: number | string
  sub?: string
  className?: string
}) {
  return (
    <div className={clsx('min-w-0 rounded-xl border border-line bg-panel px-3 py-2.5', className)}>
      <dt className="truncate text-xs text-muted">{label}</dt>
      <dd className="text-xl font-semibold tabular-nums">{value}</dd>
      {sub && <dd className="truncate text-xs text-subtle">{sub}</dd>}
    </div>
  )
}

function StateIcon({ state, className }: { state: PRState; className?: string }) {
  const s = STATE[state] ?? STATE.open
  return (
    <span
      title={s.label}
      role="img"
      aria-label={s.label}
      className={clsx('inline-flex shrink-0 [&_svg]:size-4', className)}
      style={{ color: s.color }}
    >
      {s.icon}
    </span>
  )
}

/** "merged 2h ago by alex · 5h open" */
function lifeLine(p: PRSummary) {
  if (p.state === 'merged' && p.merged_at)
    return `merged ${relativeTime(p.merged_at)}${p.merged_by ? ` by ${p.merged_by}` : ''}`
  if (p.state === 'closed' && p.closed_at) return `closed ${relativeTime(p.closed_at)}`
  return p.opened_at
    ? `opened ${relativeTime(p.opened_at)}`
    : `updated ${relativeTime(p.last_activity)}`
}

function PullRow({
  pr: p,
  onOpen,
  onAgent,
}: {
  pr: PRSummary
  onOpen: () => void
  onAgent: (a: string) => void
}) {
  return (
    <li className="px-3 py-3 sm:px-4">
      <div className="flex items-start gap-2.5">
        <StateIcon state={p.state} className="mt-0.5" />
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-baseline gap-x-2">
            <button
              onClick={onOpen}
              className="min-w-0 text-left text-[13px] font-medium hover:underline"
            >
              {p.title || <span className="italic text-muted">Title waits for approval</span>}
            </button>
            {p.outside && <OutsideBadge />}
            <span className="text-xs text-subtle">{lifeLine(p)}</span>
          </div>
          <div className="mt-0.5 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-muted">
            <span className="font-mono">
              {p.repo}#{p.number}
            </span>
            <span>by {p.author || 'someone'}</span>
            {p.base_ref && (
              <span className="inline-flex items-center gap-1">
                <ArrowRight className="size-3" />
                <span className="font-mono">{p.base_ref}</span>
              </span>
            )}
            {p.agents.map((a) => (
              <button
                key={a}
                onClick={() => onAgent(a)}
                title={`Show only ${a}`}
                className="inline-flex h-5 items-center gap-1 rounded-md bg-accent-soft px-1.5 text-[11px] font-medium text-fg hover:bg-accent/20"
              >
                <Bot className="size-3 text-subtle" />
                {agentLabel(a)}
              </button>
            ))}
          </div>
          <PullFacts pr={p} className="mt-1.5" />
        </div>
        <IconButton
          label={`Open ${p.repo}#${p.number} on the forge`}
          icon={<ExternalLink className="size-3.5" />}
          onClick={() => window.open(safeUrl(p.url), '_blank', 'noopener')}
          className="hidden sm:inline-flex"
        />
      </div>
    </li>
  )
}

/** The counts that say how a pull request went, as small chips. */
function PullFacts({ pr: p, className }: { pr: PRSummary; className?: string }) {
  const sev = (['critical', 'high', 'medium', 'low'] as const).filter((s) => p.findings[s])
  return (
    <div
      className={clsx('flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted', className)}
    >
      {p.hours_open != null && (
        <Fact
          icon={<Clock />}
          title={p.state === 'open' ? 'Open for' : 'Open for, until it closed'}
        >
          {hoursLabel(p.hours_open)}
        </Fact>
      )}
      {p.pushes > 0 && (
        <Fact icon={<GitCommitHorizontal />} title="Pushes after it opened (iterations)">
          {plural(p.pushes, 'push', 'pushes')}
        </Fact>
      )}
      {(p.reviews.bots > 0 || p.reviews.people > 0) && (
        <Fact icon={<Eye />} title="Reviews by bots and by people">
          {[
            p.reviews.bots ? `${p.reviews.bots} by bots` : '',
            p.reviews.people ? `${p.reviews.people} by people` : '',
          ]
            .filter(Boolean)
            .join(', ')}
        </Fact>
      )}
      {p.comments.people + p.comments.bots > 0 && (
        <Fact icon={<MessageSquare />} title="Comments by people (and bots)">
          {p.comments.people}
          {p.comments.bots ? ` (+${p.comments.bots} bot)` : ''}
        </Fact>
      )}
      {sev.length > 0 && (
        <span className="inline-flex items-center gap-1" title="What bots found, by severity">
          {sev.map((s) => (
            <Badge key={s} color={SEVERITY[s].color}>
              {SEVERITY[s].badge}
              {p.findings[s]! > 1 ? ` ×${p.findings[s]}` : ''}
            </Badge>
          ))}
        </span>
      )}
      <MergeFacts pr={p} />
      {p.approvals.length > 0 && (
        <span
          className="inline-flex items-center gap-1 text-ok"
          title={`Approved by ${p.approvals.join(', ')}`}
        >
          <Check className="size-3.5" />
          {p.approvals.join(', ')}
        </span>
      )}
      {p.changes_requested > 0 && (
        <span className="text-warn">{plural(p.changes_requested, 'change request')}</span>
      )}
      {p.agent_seconds > 0 && (
        <Fact icon={<Sparkles />} title="Agents’ time and tokens on its linked issues">
          {duration(p.agent_seconds)}
          {p.input_tokens + p.output_tokens > 0 &&
            ` · ${compact(p.input_tokens + p.output_tokens)} tokens`}
          {p.cost_usd > 0 && ` · ${usd(p.cost_usd)}`}
        </Fact>
      )}
      {p.issues.map((i) => (
        <Link
          key={i.id}
          to="/issue/$key"
          params={{ key: i.key }}
          title={`${i.title} (${i.status})`}
          className={clsx(
            'inline-flex h-5 items-center rounded-md border border-line px-1.5 font-mono text-[11px] hover:bg-hover',
            (i.category === 'done' || i.category === 'canceled') && 'text-subtle line-through',
          )}
        >
          {i.key}
        </Link>
      ))}
    </div>
  )
}

/** CI, the review gate and merge when ready, for pull requests still open. */
export function MergeFacts({ pr: p }: { pr: PRSummary }) {
  if (p.state !== 'open' && p.state !== 'draft') return null
  return (
    <>
      {p.ci && p.ci !== 'none' && (
        <span
          className={clsx(
            'inline-flex items-center gap-1',
            p.ci === 'passed' && 'text-ok',
            p.ci === 'failed' && 'text-danger',
          )}
          title="Checks on its head"
        >
          {p.ci === 'passed' ? (
            <CircleCheck className="size-3.5" />
          ) : p.ci === 'failed' ? (
            <CircleX className="size-3.5" />
          ) : (
            <LoaderCircle className="size-3.5 animate-spin" />
          )}
          {p.ci === 'passed'
            ? 'Checks passed'
            : p.ci === 'failed'
              ? 'Checks failed'
              : 'Checks running'}
        </span>
      )}
      {p.gate && (
        <span
          className={clsx(
            'inline-flex items-center gap-1',
            p.gate === 'success' && 'text-ok',
            p.gate === 'failure' && 'text-danger',
          )}
          title="The review gate: findings that block merging"
        >
          {p.gate === 'failure' ? (
            <ShieldAlert className="size-3.5" />
          ) : p.gate === 'success' ? (
            <ShieldCheck className="size-3.5" />
          ) : (
            <LoaderCircle className="size-3.5 animate-spin" />
          )}
          {p.gate === 'success'
            ? 'Gate open'
            : p.gate === 'failure'
              ? 'Blocked by findings'
              : 'Reviewing'}
        </span>
      )}
      {p.armed && (
        <span className="inline-flex items-center gap-1 text-accent" title="Merges when ready">
          <GitMerge className="size-3.5" />
          Merges when ready
        </span>
      )}
    </>
  )
}

function Fact({ icon, title, children }: { icon: ReactNode; title: string; children: ReactNode }) {
  return (
    <span
      className="inline-flex items-center gap-1 [&>svg]:size-3.5 [&>svg]:text-subtle"
      title={title}
    >
      {icon}
      {children}
    </span>
  )
}

// ---- one pull request ----

function PullSheet({ id, onClose }: { id: number; onClose: () => void }) {
  useEscapeLayer(true, onClose)
  return createPortal(
    <div className="fixed inset-0 z-40 flex justify-end">
      <div className="absolute inset-0 bg-black/20 md:bg-black/10" onClick={onClose} />
      <div
        role="dialog"
        aria-label="Pull request"
        className="animate-sheet relative h-full w-full overflow-y-auto border-l border-line bg-panel shadow-pop md:w-[min(760px,calc(100vw-80px))]"
      >
        <PullDetail id={id} onClose={onClose} />
      </div>
    </div>,
    document.body,
  )
}

/**
 * Where the pull request runs: "In staging since 14:02" (WP-D19). Its head
 * commit (a preview) or the commit it became when merged (a squash or merge
 * commit, what its base deploys; WP-23). Per environment, what runs there
 * now wins, else the latest deploy.
 */
function PullRuns({ repo, shas }: { repo: string; shas: string[] }) {
  const found = useQuery(envAtQuery(repo, shas)).data
  const byEnv = new Map<number, EnvAt>()
  for (const e of shas.flatMap((s) => found?.[s] ?? [])) {
    const had = byEnv.get(e.environment)
    if (!had || (e.current && !had.current) || (e.current === had.current && e.at > had.at))
      byEnv.set(e.environment, e)
  }
  const at = [...byEnv.values()]
  if (!at.length) return null
  return (
    <section>
      <h3 className="mb-2 text-[13px] font-semibold">Where it runs</h3>
      <ul className="divide-y divide-line rounded-xl border border-line">
        {at.map((e) => (
          <li
            key={e.environment}
            className="flex flex-wrap items-center gap-x-2 gap-y-1 px-3 py-2 text-[13px]"
          >
            <Rocket className="size-4 shrink-0 text-muted" />
            <Link
              to="/p/$key/environments/$name"
              params={{ key: e.project, name: e.name }}
              className={clsx(NAME_FONT, 'font-medium hover:underline')}
            >
              {e.current ? `In ${e.name}` : e.name}
            </Link>
            <span className="text-muted">
              {e.current ? `since ${sinceTime(e.at)}` : `deployed ${envAgo(e.at)}, since replaced`}
            </span>
          </li>
        ))}
      </ul>
    </section>
  )
}

function PullDetail({ id, onClose }: { id: number; onClose: () => void }) {
  const d = useQuery(pullQuery(id))
  const projects = useQuery(q.projects()).data ?? []
  const canAct = (projectID: number) =>
    projects.some((p) => p.id === projectID && (p.role === 'owner' || p.role === 'editor'))
  const p = d.data
  return (
    <div className="pb-16">
      <div className="wp-surface-blur sticky top-0 z-[1] flex items-center gap-2 border-b border-line bg-panel/90 px-4 py-2.5 backdrop-blur">
        {p && <StateIcon state={p.state} />}
        <span className="min-w-0 flex-1 truncate font-mono text-xs text-muted">
          {p ? `${p.repo}#${p.number}` : 'Pull request'}
        </span>
        {p && (
          <a
            href={safeUrl(p.url)}
            target="_blank"
            rel="noreferrer"
            className="inline-flex h-7 items-center gap-1 rounded-md px-2 text-xs text-muted hover:bg-hover hover:text-fg"
          >
            <ExternalLink className="size-3.5" />
            Open on {p.forge === 'gitlab' ? 'GitLab' : p.forge === 'github' ? 'GitHub' : 'Forgejo'}
          </a>
        )}
        <IconButton label="Close" onClick={onClose} icon={<X className="size-4" />} />
      </div>
      {d.isPending ? (
        <div className="space-y-3 p-4">
          <Skeleton className="h-8 w-2/3" />
          <Skeleton className="h-24" />
          <Skeleton className="h-48" />
        </div>
      ) : !p ? (
        <Empty icon={<GitPullRequest />} title="Pull request not found">
          It may belong to a project you can’t see, or its repo was detached.
        </Empty>
      ) : (
        <div className="space-y-6 px-4 pt-4 sm:px-6">
          <div>
            <h2 className="text-lg font-semibold tracking-tight [overflow-wrap:anywhere]">
              {p.title || <span className="italic text-muted">Title waits for approval</span>}
            </h2>
            <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-1 text-[13px] text-muted">
              <Badge color={STATE[p.state].color}>{STATE[p.state].label}</Badge>
              {p.outside && <OutsideBadge />}
              <span>
                {p.author || 'Someone'} wants to merge{' '}
                <span className="font-mono text-xs">{p.head_ref || 'a branch'}</span> into{' '}
                <span className="font-mono text-xs">{p.base_ref || 'its base'}</span>
              </span>
            </div>
          </div>

          <dl className="grid grid-cols-2 gap-x-4 gap-y-3 text-[13px] sm:grid-cols-3">
            <Fact2 label="Opened" value={p.opened_at ? dateTime(p.opened_at) : '—'} />
            <Fact2
              label={p.state === 'closed' ? 'Closed' : 'Merged'}
              value={
                p.state === 'merged' && p.merged_at
                  ? `${dateTime(p.merged_at)}${p.merged_by ? ` by ${p.merged_by}` : ''}`
                  : p.state === 'closed' && p.closed_at
                    ? dateTime(p.closed_at)
                    : 'Not yet'
              }
            />
            <Fact2
              label={p.state === 'open' || p.state === 'draft' ? 'Open for' : 'Took'}
              value={hoursLabel(p.hours_open)}
            />
            <Fact2 label="Pushes after opening" value={String(p.pushes)} />
            <Fact2
              label="Reviews"
              value={`${p.reviews.bots} by bots, ${p.reviews.people} by people`}
            />
            <Fact2
              label="Approved by"
              value={p.approvals.length ? p.approvals.join(', ') : 'No one yet'}
            />
            <Fact2
              label="Comments"
              value={`${p.comments.people} by people${p.comments.bots ? `, ${p.comments.bots} by bots` : ''}`}
            />
            {(p.additions != null || p.changed_files != null) && (
              <Fact2
                label="Size"
                value={[
                  p.additions != null ? `+${p.additions} −${p.deletions ?? 0}` : '',
                  p.changed_files != null ? plural(p.changed_files, 'file') : '',
                  p.commits != null ? plural(p.commits, 'commit') : '',
                ]
                  .filter(Boolean)
                  .join(' · ')}
              />
            )}
            <Fact2
              label="Agents"
              value={p.agents.length ? p.agents.map(agentLabel).join(', ') : 'None recorded'}
            />
          </dl>

          <OutsidePR prID={p.id} />
          <PullRuns
            repo={p.repo}
            shas={[p.merge_sha, p.head_sha].filter((s): s is string => !!s)}
          />
          <Shipping pr={p} />
          <Babysitter pr={p} />
          <PullAudits pr={p} />

          {p.agent_time.length > 0 && (
            <section>
              <h3 className="mb-2 text-[13px] font-semibold">Agents’ work on its issues</h3>
              <ul className="divide-y divide-line rounded-xl border border-line">
                {p.agent_time.map((a) => (
                  <li
                    key={a.agent}
                    className="flex flex-wrap items-center gap-x-3 gap-y-1 px-3 py-2 text-[13px]"
                  >
                    <Bot className="size-4 text-subtle" />
                    <span className="min-w-0 flex-1 truncate font-medium" title={a.agent}>
                      {agentLabel(a.agent)}
                      {a.models.length > 0 && (
                        <span className="ml-1.5 font-normal text-subtle">
                          on {a.models.join(', ')}
                        </span>
                      )}
                    </span>
                    <span className="text-xs text-muted">
                      {plural(a.runs, 'run')} · {duration(a.seconds)}
                      {a.input_tokens + a.output_tokens > 0 &&
                        ` · ${compact(a.input_tokens)} in / ${compact(a.output_tokens)} out`}
                      {a.cost_usd > 0 && ` · ${usd(a.cost_usd)}`}
                    </span>
                  </li>
                ))}
              </ul>
            </section>
          )}

          {p.issues.length > 0 && (
            <section>
              <h3 className="mb-2 text-[13px] font-semibold">Linked issues</h3>
              <ul className="space-y-1">
                {p.issues.map((i) => (
                  <li key={i.id}>
                    <Link
                      to="/issue/$key"
                      params={{ key: i.key }}
                      className="flex items-center gap-2 rounded-lg px-2 py-1.5 text-[13px] hover:bg-hover"
                    >
                      <span className="font-mono text-xs text-muted">{i.key}</span>
                      <span className="min-w-0 flex-1 truncate">{i.title}</span>
                      <Badge>{i.status}</Badge>
                    </Link>
                  </li>
                ))}
              </ul>
            </section>
          )}

          {(p.mentions?.length ?? 0) > 0 && (
            <section>
              <h3
                className="mb-2 text-[13px] font-semibold"
                title="Issues its text or comments name without it being their work: not linked, so its merge doesn't finish them"
              >
                Mentioned, not linked
              </h3>
              <div className="flex flex-wrap gap-x-3 gap-y-1 px-2 text-[13px]">
                {p.mentions!.map((i) => (
                  <Link
                    key={i.id}
                    to="/issue/$key"
                    params={{ key: i.key }}
                    title={`${i.title} (${i.status})`}
                    className="text-muted hover:text-fg hover:underline"
                  >
                    <span className="font-mono text-xs">{i.key}</span>
                    {i.verb && i.verb !== 'relates' && (
                      <span className="ml-1 text-[11px] text-subtle">
                        {i.verb.replace('_', ' ')}
                      </span>
                    )}
                  </Link>
                ))}
              </div>
            </section>
          )}

          <section>
            <h3 className="mb-2 text-[13px] font-semibold">What happened</h3>
            {p.timeline.length === 0 ? (
              <p className="text-[13px] text-subtle">Nothing recorded yet.</p>
            ) : (
              <ol className="relative space-y-3 border-l border-line pl-5">
                {p.timeline.map((it, n) => (
                  <TimelineItem key={n} item={it} />
                ))}
              </ol>
            )}
          </section>

          {p.bot_runs.length > 0 && (
            <section>
              <h3 className="mb-2 text-[13px] font-semibold">Bot reviews</h3>
              <ul className="space-y-2">
                {p.bot_runs.map((r) => (
                  <BotRunCard key={r.id} run={r} canAct={canAct(r.project_id)} hidePR />
                ))}
              </ul>
            </section>
          )}
        </div>
      )}
    </div>
  )
}

/** A pull request from outside, waiting for a person (WP-281). */
function OutsideBadge() {
  return (
    <span
      className="inline-flex items-center gap-1 rounded-md bg-warn/15 px-1.5 text-[11px] font-medium text-warn"
      title="Its author isn't a Waypoint person: no agent or bot touches it until a person approves it"
    >
      <Globe className="size-3" aria-hidden />
      From outside
    </span>
  )
}

function Fact2({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-muted">{label}</dt>
      <dd className="break-words">{value}</dd>
    </div>
  )
}

const KIND: Record<PRTimelineItem['kind'], { icon: ReactNode; color?: string; verb: string }> = {
  opened: { icon: <GitPullRequest />, color: 'var(--wp-ok)', verb: 'opened it' },
  pushed: { icon: <GitCommitHorizontal />, verb: 'pushed' },
  ready: { icon: <CircleDot />, color: 'var(--wp-ok)', verb: 'marked it ready for review' },
  reopened: { icon: <RotateCcw />, color: 'var(--wp-ok)', verb: 'reopened it' },
  commented: { icon: <MessageSquare />, verb: 'commented' },
  reviewed: { icon: <Eye />, verb: 'reviewed it' },
  approved: { icon: <Check />, color: 'var(--wp-ok)', verb: 'approved it' },
  changes_requested: { icon: <ShieldAlert />, color: 'var(--wp-warn)', verb: 'asked for changes' },
  merged: { icon: <GitMerge />, color: 'var(--wp-accent)', verb: 'merged it' },
  closed: { icon: <GitPullRequestClosed />, color: 'var(--wp-danger)', verb: 'closed it' },
  bot_review: { icon: <Bot />, color: 'var(--wp-accent)', verb: 'reviewed it' },
  agent_run: { icon: <Sparkles />, color: 'var(--wp-accent)', verb: 'worked on' },
  issue_status: { icon: <ArrowRight />, verb: 'moved' },
}

function TimelineItem({ item: it }: { item: PRTimelineItem }) {
  const k = KIND[it.kind] ?? { icon: <CircleDot />, verb: it.kind }
  let who: ReactNode = it.actor || 'Someone'
  let what: ReactNode = k.verb
  switch (it.kind) {
    case 'pushed':
      what = (
        <>
          pushed <span className="font-mono text-xs">{it.sha?.slice(0, 7)}</span>
        </>
      )
      break
    case 'bot_review':
      who = 'Waypoint'
      what =
        it.outcome === 'done'
          ? 'reviewed it'
          : it.outcome === 'queued' || it.outcome === 'running'
            ? `is reviewing it`
            : `review ${it.outcome}`
      break
    case 'agent_run':
      who = agentLabel(it.agent || 'An agent')
      what = (
        <>
          {it.detail}
          {it.model && <span className="text-subtle"> on {it.model}</span>}
        </>
      )
      break
    case 'issue_status':
      who = it.agent ? agentLabel(it.agent) : it.actor || 'Waypoint'
      what = it.detail
      break
  }
  const facts = [
    it.seconds ? duration(it.seconds) : '',
    it.tokens ? `${compact(it.tokens)} tokens` : '',
    it.kind === 'agent_run' && it.outcome && it.outcome !== 'done' ? it.outcome : '',
  ].filter(Boolean)
  return (
    <li className="relative">
      <span
        className="absolute -left-[29px] top-0 flex size-[17px] items-center justify-center rounded-full border border-line bg-panel [&_svg]:size-2.5"
        style={{ color: k.color ?? 'var(--wp-subtle)' }}
      >
        {k.icon}
      </span>
      <div className="text-[13px]">
        <span className="font-medium">{who}</span>
        {it.bot && it.kind !== 'bot_review' && <Badge className="mx-1">bot</Badge>} {what}
        <span className="ml-2 text-xs text-subtle" title={dateTime(it.at)}>
          {relativeTime(it.at)}
        </span>
        {it.url && (
          <a
            href={safeUrl(it.url)}
            target="_blank"
            rel="noreferrer"
            className="ml-2 text-xs text-subtle hover:text-fg hover:underline"
          >
            view
          </a>
        )}
      </div>
      {(facts.length > 0 || (it.kind === 'bot_review' && it.detail)) && (
        <div className="text-xs text-subtle">
          {[
            it.kind === 'bot_review' ? it.actor : '',
            it.kind === 'bot_review' ? it.detail : '',
            ...facts,
          ]
            .filter(Boolean)
            .join(' · ')}
        </div>
      )}
      {it.body && it.kind !== 'pushed' && (
        <p className="mt-0.5 line-clamp-3 whitespace-pre-line break-words text-xs text-muted">
          {stripComments(it.body)}
        </p>
      )}
    </li>
  )
}

/** Text without HTML comments (bots hide markers in them), removed until none
 * is left so a comment split around another can't survive. An unclosed
 * comment hides the rest of the text, as it would in HTML. */
function stripComments(text: string): string {
  let prev: string
  do {
    prev = text
    text = text.replace(/<!--[\s\S]*?(?:-->|$)/g, '')
  } while (text !== prev)
  return text.trim()
}
