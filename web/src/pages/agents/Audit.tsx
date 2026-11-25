import { useMemo, useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Bot,
  CheckCircle2,
  Download,
  GitPullRequest,
  KeyRound,
  ScrollText,
  Settings2,
  SlidersHorizontal,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { agentLabel } from '@/lib/agents'
import { auditCSVUrl, auditQuery, type AuditCategory, type AuditEntry } from '@/lib/audit'
import { dateTime, relativeTime, safeUrl } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/input'
import { Avatar, Empty, Skeleton } from '@/components/ui/misc'
import { Explainer } from '@/components/ui/help'
import { RiskyCommands } from './Risky'
import { RefusedCalls } from './Refused'
import type { AuditView, HubTabProps } from './search'

const CATEGORIES: { id: AuditCategory | ''; label: string }[] = [
  { id: '', label: 'All' },
  { id: 'approval', label: 'Approvals' },
  { id: 'delegation', label: 'Delegation' },
  { id: 'control', label: 'Agent control' },
  { id: 'settings', label: 'Settings' },
  { id: 'access', label: 'Access' },
]

const CATEGORY_ICON: Record<AuditCategory, { icon: ReactNode; className: string }> = {
  approval: { icon: <CheckCircle2 />, className: 'text-ok' },
  delegation: { icon: <Bot />, className: 'text-accent' },
  control: { icon: <SlidersHorizontal />, className: 'text-warn' },
  settings: { icon: <Settings2 />, className: 'text-muted' },
  access: { icon: <KeyRound />, className: 'text-muted' },
}

export function AuditTab(p: HubTabProps) {
  const view = p.filters.view
  return (
    <div className="space-y-4">
      <div
        role="tablist"
        aria-label="Audit log view"
        className="inline-flex rounded-lg border border-line bg-sunken p-0.5"
      >
        {(
          [
            [undefined, 'Decisions'],
            ['risky', 'Risky commands'],
            ['refused', 'Refused calls'],
          ] as [AuditView | undefined, string][]
        ).map(([v, label]) => (
          <button
            key={label}
            role="tab"
            aria-selected={view === v}
            onClick={() => p.setFilters({ view: v })}
            className={clsx(
              'h-7 rounded-md px-3 text-xs',
              view === v ? 'bg-panel font-medium text-fg shadow-card' : 'text-muted hover:text-fg',
            )}
          >
            {label}
          </button>
        ))}
      </div>
      {view === 'risky' ? (
        <RiskyCommands {...p} />
      ) : view === 'refused' ? (
        <RefusedCalls {...p} />
      ) : (
        <Decisions {...p} />
      )}
    </div>
  )
}

function Decisions({ filters }: HubTabProps) {
  const [category, setCategory] = useState<AuditCategory | ''>('')
  const [user, setUser] = useState('')
  const users = useQuery(q.users()).data ?? []
  const f = {
    project: filters.project || undefined,
    agent: filters.agent || undefined,
    days: filters.days,
    category,
    user: user || undefined,
  }
  const log = useInfiniteQuery(auditQuery(f))
  const entries = useMemo(() => log.data?.pages.flatMap((p) => p.entries) ?? [], [log.data])
  const days = useMemo(() => {
    const m = new Map<string, AuditEntry[]>()
    for (const e of entries) {
      const k = dayLabel(e.at)
      if (!m.has(k)) m.set(k, [])
      m.get(k)!.push(e)
    }
    return [...m.entries()]
  }, [entries])
  const narrowed = !!(category || user || filters.agent || filters.project)

  return (
    <div className="space-y-4">
      <Explainer id="agents:audit" label="What's recorded here">
        What people decided, newest first: reviews and approvals of what agents wrote, design
        decisions, pull request approvals and merges, work delegated to agents or taken back,
        pausing agents and ordering their queue, and changes to automations, AI models, forges,
        tokens and who has access. Edits to issues are in each issue’s <b>History</b>. You see
        decisions in your projects and your own; admins see everything.
      </Explainer>

      <div className="flex flex-wrap items-center gap-2">
        <div
          role="radiogroup"
          aria-label="Kind of decision"
          className="no-scrollbar -mx-1 flex max-w-full gap-1 overflow-x-auto px-1"
        >
          {CATEGORIES.map((c) => (
            <button
              key={c.id}
              role="radio"
              aria-checked={category === c.id}
              onClick={() => setCategory(c.id)}
              className={clsx(
                'h-7 shrink-0 rounded-full border px-3 text-xs',
                category === c.id
                  ? 'border-accent/50 bg-accent-soft font-medium text-fg'
                  : 'border-line text-muted hover:bg-hover hover:text-fg',
              )}
            >
              {c.label}
            </button>
          ))}
        </div>
        <div className="ml-auto flex items-center gap-2">
          <Select
            aria-label="Person"
            value={user}
            onChange={(e) => setUser(e.target.value)}
            className="!w-auto min-w-32 max-w-48"
          >
            <option value="">Everyone</option>
            {users.map((u) => (
              <option key={u.id} value={u.username}>
                {u.display_name || u.username}
              </option>
            ))}
          </Select>
          <a
            href={auditCSVUrl(f)}
            download
            className="inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg border border-line bg-panel px-2.5 text-[13px] font-medium shadow-card hover:bg-hover"
            title="Download these entries as a spreadsheet (CSV)"
          >
            <Download className="size-3.5" />
            Export CSV
          </a>
        </div>
      </div>

      {log.isLoading ? (
        <div className="space-y-2">
          <Skeleton className="h-4 w-24" />
          <Skeleton className="h-40" />
        </div>
      ) : log.isError ? (
        <div className="rounded-xl border border-danger/40 bg-danger/5 px-4 py-3 text-[13px] text-danger">
          Couldn’t load the audit log: {(log.error as Error).message}
        </div>
      ) : entries.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<ScrollText />} title={`Nothing decided in the last ${filters.days} days`}>
            {narrowed
              ? 'Nothing matches these filters. Try All, Everyone or a longer period.'
              : 'Reviews of agents’ work, delegations, pauses, and changes to settings and access appear here as people make them.'}
          </Empty>
        </div>
      ) : (
        <div className="space-y-5">
          {days.map(([day, items]) => (
            <section key={day}>
              <h3 className="mb-1.5 text-xs font-semibold uppercase tracking-wide text-subtle">
                {day}
              </h3>
              <ul className="overflow-hidden rounded-xl border border-line bg-panel">
                {mergeRuns(items).map(({ e, n }) => (
                  <Row key={e.id} e={e} n={n} showProject={!filters.project} />
                ))}
              </ul>
            </section>
          ))}
          {log.hasNextPage && (
            <div className="flex justify-center">
              <Button
                size="sm"
                loading={log.isFetchingNextPage}
                onClick={() => log.fetchNextPage()}
              >
                Load older
              </Button>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

/** Consecutive entries that read the same (a run of identical decisions) become one, with a count. */
function mergeRuns(items: AuditEntry[]): { e: AuditEntry; n: number }[] {
  const same = (a: AuditEntry, b: AuditEntry) =>
    a.action === b.action &&
    a.user?.id === b.user?.id &&
    a.actor === b.actor &&
    a.agent === b.agent &&
    a.about_agent === b.about_agent &&
    a.project?.key === b.project?.key &&
    a.target === b.target &&
    a.detail === b.detail
  const out: { e: AuditEntry; n: number }[] = []
  for (const e of items) {
    const last = out[out.length - 1]
    if (last && same(last.e, e)) last.n++
    else out.push({ e, n: 1 })
  }
  return out
}

function Row({ e, n, showProject }: { e: AuditEntry; n: number; showProject: boolean }) {
  const cat = CATEGORY_ICON[e.category] ?? CATEGORY_ICON.settings
  const s = describe(e)
  const namesProject = !s.noTarget && linksOwnBoard(e)
  const project = showProject && !namesProject ? e.project : null
  const detail = s.detail === undefined ? e.detail : s.detail
  return (
    <li className="flex items-start gap-3 border-b border-line px-3 py-3 last:border-b-0 sm:px-4">
      <span className="relative mt-0.5 shrink-0">
        {e.user ? (
          <Avatar user={e.user} size={28} />
        ) : (
          <span className="flex size-7 items-center justify-center rounded-full bg-sunken text-muted">
            <GitPullRequest className="size-3.5" />
          </span>
        )}
        <span
          className={clsx(
            'absolute -bottom-1 -right-1 flex size-4 items-center justify-center rounded-full bg-raised ring-2 ring-panel [&>svg]:size-2.5',
            cat.className,
          )}
          role="img"
          aria-label={CATEGORIES.find((c) => c.id === e.category)?.label}
          title={CATEGORIES.find((c) => c.id === e.category)?.label}
        >
          {cat.icon}
        </span>
      </span>
      <div className="min-w-0 flex-1">
        <p className="text-[13px] leading-5 [overflow-wrap:anywhere]">
          <Who e={e} /> {s.verb}
          {!s.noTarget && e.target && (
            <>
              {' '}
              <Target e={e} />
            </>
          )}
          {s.after}
          {n > 1 && <span className="ml-1.5 text-xs text-subtle">×{n}</span>}
        </p>
        {detail && <p className="mt-0.5 line-clamp-2 text-[13px] text-muted">{detail}</p>}
        {(project || e.agent) && (
          <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-subtle">
            {project && (
              <Link
                to="/p/$key/board"
                params={{ key: project.key }}
                className="hover:text-fg hover:underline"
              >
                {project.name}
              </Link>
            )}
            {e.agent && (
              <span title="An agent did this through this person’s token">
                through <span className="font-mono">{agentLabel(e.agent)}</span>
              </span>
            )}
          </div>
        )}
      </div>
      <time
        dateTime={e.at}
        title={dateTime(e.at)}
        className="shrink-0 pt-0.5 text-xs text-subtle tabular-nums"
      >
        {relativeTime(e.at)}
      </time>
    </li>
  )
}

function Who({ e }: { e: AuditEntry }) {
  if (e.user) return <b className="font-medium">{e.user.display_name || e.user.username}</b>
  if (e.actor)
    return (
      <span title="An account on the forge">
        <b className="font-medium">{e.actor}</b>
        <span className="text-muted"> on the forge</span>
      </span>
    )
  return <b className="font-medium">Someone</b>
}

/**
 * An entry about the project itself, with no link of its own: its name links
 * to the project's board, as the project line (left out for it) would have.
 */
function linksOwnBoard(e: AuditEntry): boolean {
  return (
    !e.url?.startsWith('/') &&
    !safeUrl(e.url) &&
    e.target_type === 'project' &&
    !!e.project &&
    e.target === e.project.name
  )
}

function Target({ e }: { e: AuditEntry }) {
  const label = <span className="font-medium">{e.target}</span>
  if (e.url?.startsWith('/'))
    return (
      <Link to={e.url} className="text-accent hover:underline">
        {label}
      </Link>
    )
  const href = safeUrl(e.url)
  if (linksOwnBoard(e) && e.project)
    return (
      <Link
        to="/p/$key/board"
        params={{ key: e.project.key }}
        className="text-accent hover:underline"
      >
        {label}
      </Link>
    )
  if (href)
    return (
      <a href={href} target="_blank" rel="noreferrer" className="text-accent hover:underline">
        {label}
      </a>
    )
  return label
}

function AgentChip({ spec }: { spec?: string }) {
  if (!spec) return <>an agent</>
  return (
    <span className="inline-flex items-center gap-1 rounded bg-sunken px-1 font-mono text-[12px]">
      <Bot className="size-3 text-subtle" />
      {spec === 'any' ? 'any agent' : agentLabel(spec)}
    </span>
  )
}

/** One agent or several ("a, b"), as chips. */
function AgentChips({ specs }: { specs?: string }) {
  const list = (specs ?? '')
    .split(',')
    .map((x) => x.trim())
    .filter(Boolean)
  if (list.length <= 1) return <AgentChip spec={list[0]} />
  return (
    <>
      {list.map((a, i) => (
        <span key={a}>
          {i > 0 && (i === list.length - 1 ? ' and ' : ', ')}
          <AgentChip spec={a} />
        </span>
      ))}
    </>
  )
}

interface Sentence {
  verb: ReactNode
  /** After the target. */
  after?: ReactNode
  /** Replaces the entry's own detail line; null hides it. */
  detail?: string | null
  /** The sentence says it all without the target. */
  noTarget?: boolean
}

/** An entry as a sentence: "Robin delegated WP-12 to claude". */
function describe(e: AuditEntry): Sentence {
  const about = e.about_agent
  const was = e.detail?.match(/\(was ([^)]+)\)/)?.[1]
  switch (e.action) {
    case 'review':
      return {
        verb: (
          <>
            approved what <AgentChips specs={about} /> wrote on
          </>
        ),
        detail: e.detail?.startsWith('reviewed a comment') ? 'A comment.' : null,
      }
    case 'design.approve':
      return { verb: 'approved the design note' }
    case 'design.reject':
      return { verb: 'turned down a change to' }
    case 'design.merge':
      return { verb: 'merged an addition into' }
    case 'design.delete':
      return { verb: 'deleted the design note' }
    case 'glossary.lock':
      return { verb: 'locked', after: ' against agents’ edits', detail: null }
    case 'glossary.unlock':
      return { verb: 'unlocked', after: ' for agents to edit', detail: null }
    case 'glossary.delete':
      return { verb: 'deleted the glossary entry', detail: null }
    case 'delegate':
      return {
        verb: 'delegated',
        after: (
          <>
            {' to '}
            <AgentChip spec={about} />
          </>
        ),
        detail: was ? `It was for ${was === 'any' ? 'any agent' : was}.` : null,
      }
    case 'undelegate':
      return {
        verb: 'took',
        after: (
          <>
            {' back from '}
            <AgentChip spec={about} />
          </>
        ),
        detail: null,
      }
    case 'queue.pin':
      return { verb: 'pinned', after: ' in the agent queue' }
    case 'queue.move':
      return { verb: 'moved', after: ' in the agent queue' }
    case 'queue.unpin':
      return { verb: 'unpinned', after: ' from the agent queue' }
    case 'agent.pause':
    case 'agent.resume':
      return {
        verb: e.action === 'agent.pause' ? 'paused' : 'resumed',
        after: (
          <>
            {' '}
            {about || (e.target && e.target !== 'any') ? (
              <AgentChip spec={about || e.target} />
            ) : (
              'all their agents'
            )}
          </>
        ),
        noTarget: true,
      }
    case 'project.agents_paused':
      return { verb: 'changed whether agents may take new work in' }
    case 'claim.release':
      return {
        verb: 'took',
        after: (
          <>
            {' back from '}
            <AgentChip spec={about} />
          </>
        ),
      }
    case 'bot.cancel':
      return { verb: 'cancelled a bot run on' }
    case 'bot.retry':
      return { verb: 'ran a bot again on' }
    case 'issue.delete':
      return { verb: 'deleted' }
    case 'page.delete':
      return { verb: 'deleted the page', detail: null }
    case 'settings.review_note':
      return { verb: 'changed the note on bot reviews', noTarget: true }
    case 'settings.ship_reply':
      return { verb: 'changed the reply agents end with after wp ship', noTarget: true }
    case 'automation.create':
      return { verb: 'added the automation' }
    case 'automation.update':
      return { verb: 'changed the automation' }
    case 'automation.delete':
      return { verb: 'deleted the automation', detail: null }
    case 'workflow.create':
      return { verb: 'created the workflow' }
    case 'workflow.update':
      return { verb: 'changed the workflow' }
    case 'workflow.delete':
      return { verb: 'deleted the workflow' }
    case 'ai.provider.create':
      return { verb: 'added the AI provider' }
    case 'ai.provider.update':
      return { verb: 'changed the AI provider' }
    case 'ai.provider.delete':
      return { verb: 'removed the AI provider' }
    case 'ai.task':
      return { verb: 'set the model for' }
    case 'forge.create':
      return { verb: 'connected the forge' }
    case 'forge.update':
      return { verb: 'changed the forge' }
    case 'forge.delete':
      return { verb: 'disconnected the forge' }
    case 'repo.attach':
      return { verb: 'attached the repo' }
    case 'repo.update':
      return { verb: 'changed the repo' }
    case 'repo.detach':
      return { verb: 'detached the repo' }
    case 'member.set':
      return { verb: e.detail?.startsWith('added') ? 'gave access to' : 'changed the role of' }
    case 'member.remove':
      return e.detail === 'left the project'
        ? { verb: 'left the project', noTarget: true, detail: null }
        : { verb: 'removed', after: ' from the project', detail: null }
    case 'token.create':
      return { verb: 'created the token' }
    case 'token.revoke':
      return { verb: 'revoked the token' }
    case 'device.approve':
      return { verb: 'approved a wp CLI sign-in on' }
    case 'invite.create':
      return { verb: 'invited', detail: null }
    case 'invite.revoke':
      return { verb: 'revoked the invite for', detail: null }
    case 'user.create':
      return { verb: 'created the account' }
    case 'user.update':
      return { verb: 'changed the account of' }
    case 'user.delete':
      return { verb: 'deleted the account of' }
    case 'user.reset_link':
      return { verb: 'made a password reset link for', detail: null }
    case 'pr.approved':
      return { verb: 'approved' }
    case 'pr.changes_requested':
      return { verb: 'requested changes on' }
    case 'pr.merged':
      return { verb: 'merged' }
    case 'pr.closed':
      return { verb: 'closed' }
  }
  return { verb: e.action.replaceAll('.', ' ').replaceAll('_', ' ') }
}

function dayLabel(iso: string) {
  const d = new Date(iso)
  const today = new Date()
  const y = new Date()
  y.setDate(today.getDate() - 1)
  if (d.toDateString() === today.toDateString()) return 'Today'
  if (d.toDateString() === y.toDateString()) return 'Yesterday'
  return d.toLocaleDateString(undefined, {
    weekday: 'long',
    month: 'short',
    day: 'numeric',
    ...(d.getFullYear() === today.getFullYear() ? {} : { year: 'numeric' }),
  })
}
