// Agents → Audit → Risky commands (WP-647): shell commands agents ran that
// delete, discard or overwrite what can't be had back, or reach
// production, another machine, a cluster or a disk, as `wp usage sync`
// found them in Claude Code's transcripts.

import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { ShieldAlert } from 'lucide-react'
import { q } from '@/lib/queries'
import { dateTime, relativeTime } from '@/lib/format'
import {
  usageRiskyQuery,
  type RiskyFilters,
  type UsageFilters,
  type UsageRiskyRow,
} from '@/lib/usage-analytics'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/input'
import { Badge, Empty, Skeleton } from '@/components/ui/misc'
import { Explainer } from '@/components/ui/help'
import type { HubTabProps } from './search'

export const RISKY_KINDS: Record<string, string> = {
  delete: 'Deletes',
  discard: 'Discarded work',
  force_push: 'Force pushes',
  push_main: 'Pushes to main',
  branch_delete: 'Branch deletes',
  history: 'History rewrites',
  sql: 'SQL',
  cluster: 'Clusters',
  deploy: 'Deploys',
  service: 'Services',
  docker: 'Docker',
  disk: 'Disks',
  power: 'Reboots',
  permissions: 'Permissions',
  remote_script: 'Remote scripts',
}

const LEVELS: { id: RiskyFilters['level']; label: string }[] = [
  { id: '', label: 'All' },
  { id: 'high', label: 'High' },
  { id: 'medium', label: 'Medium' },
]

const OUTCOME: Record<UsageRiskyRow['outcome'], string> = {
  ran: 'ran',
  error: 'ran and failed',
  refused: 'refused',
  unknown: 'no result',
}

const SOURCES: Record<string, string> = {
  classifier: 'the classifier',
  hook: 'a hook',
  safety_check: 'the safety check',
  rule: 'a rule',
  user: 'you',
}

export function RiskyCommands({ filters, setFilters }: HubTabProps) {
  const [r, setR] = useState<RiskyFilters>({ level: '', kind: '', outcome: '' })
  const me = useQuery(q.me()).data
  const users = useQuery({ ...q.users(), enabled: !!me?.is_admin }).data ?? []
  const f: UsageFilters = {
    agent: filters.agent,
    project: filters.project,
    days: filters.days,
    who: filters.who,
  }
  const list = useInfiniteQuery(usageRiskyQuery(f, r))
  const rows = useMemo(() => list.data?.pages.flatMap((p) => p.rows) ?? [], [list.data])
  const counts = list.data?.pages[0]?.counts
  const kinds = Object.entries(counts?.kinds ?? {}).sort((a, b) => b[1] - a[1])
  const narrowed = !!(r.level || r.kind || r.outcome)

  return (
    <div className="space-y-4">
      <Explainer id="agents:risky" label="What counts as risky">
        Shell commands agents ran that delete, discard or overwrite what can’t be had back, or reach
        production, another machine, a cluster or a disk, found by plain patterns in Claude Code’s
        transcripts when <code>wp usage sync</code> runs. <b>High</b>: production, other machines,
        shared history, deletes outside any checkout. <b>Medium</b>: local work or state a person
        might want back. A high one that ran notifies you. The whole command is kept (older rows:
        its first 300 characters), with obvious secrets hidden, visible to you and admins only. Also{' '}
        <code>wp usage risky</code>.
      </Explainer>

      {counts && (
        <p className="text-[13px] text-muted">
          Last {filters.days} days: <b className="font-medium text-danger">{counts.high} high</b>,{' '}
          <b className="font-medium text-warn">{counts.medium} medium</b>
          {counts.refused > 0 && <> ({counts.refused} refused before they ran)</>}.
        </p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <div role="radiogroup" aria-label="Risk" className="flex gap-1">
          {LEVELS.map((l) => (
            <button
              key={l.id}
              role="radio"
              aria-checked={r.level === l.id}
              onClick={() => setR({ ...r, level: l.id })}
              className={clsx(
                'h-7 shrink-0 rounded-full border px-3 text-xs',
                r.level === l.id
                  ? 'border-accent/50 bg-accent-soft font-medium text-fg'
                  : 'border-line text-muted hover:bg-hover hover:text-fg',
              )}
            >
              {l.label}
            </button>
          ))}
        </div>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          <Select
            aria-label="Kind"
            value={r.kind}
            onChange={(e) => setR({ ...r, kind: e.target.value })}
            className="!w-auto min-w-32 max-w-48"
          >
            <option value="">All kinds</option>
            {kinds.map(([k, n]) => (
              <option key={k} value={k}>
                {RISKY_KINDS[k] ?? k} ({n})
              </option>
            ))}
            {r.kind && !counts?.kinds[r.kind] && (
              <option value={r.kind}>{RISKY_KINDS[r.kind] ?? r.kind}</option>
            )}
          </Select>
          <Select
            aria-label="Outcome"
            value={r.outcome}
            onChange={(e) => setR({ ...r, outcome: e.target.value as RiskyFilters['outcome'] })}
            className="!w-auto min-w-32 max-w-48"
          >
            <option value="">Ran or refused</option>
            <option value="ran">Ran</option>
            <option value="refused">Refused</option>
          </Select>
          {me?.is_admin && (
            <Select
              aria-label="Person"
              value={filters.who ?? ''}
              onChange={(e) => setFilters({ who: e.target.value || undefined })}
              className="!w-auto min-w-32 max-w-48"
            >
              <option value="">Me</option>
              <option value="all">Everyone</option>
              {users.map((u) => (
                <option key={u.id} value={u.username}>
                  {u.display_name || u.username}
                </option>
              ))}
            </Select>
          )}
        </div>
      </div>

      {list.isLoading ? (
        <div className="space-y-2">
          <Skeleton className="h-4 w-24" />
          <Skeleton className="h-40" />
        </div>
      ) : list.isError ? (
        <div className="rounded-xl border border-danger/40 bg-danger/5 px-4 py-3 text-[13px] text-danger">
          Couldn’t load risky commands: {(list.error as Error).message}
        </div>
      ) : rows.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty
            icon={<ShieldAlert />}
            title={`No risky commands in the last ${filters.days} days`}
          >
            {narrowed
              ? 'Nothing matches these filters. Try All, or a longer period.'
              : 'Force pushes, deletes, production commands and the like appear here once wp usage sync reads the sessions that ran them.'}
          </Empty>
        </div>
      ) : (
        <>
          <ul className="overflow-hidden rounded-xl border border-line bg-panel">
            {rows.map((x) => (
              <Row key={x.id} x={x} />
            ))}
          </ul>
          {list.hasNextPage && (
            <div className="flex justify-center">
              <Button
                size="sm"
                loading={list.isFetchingNextPage}
                onClick={() => list.fetchNextPage()}
              >
                Load older
              </Button>
            </div>
          )}
        </>
      )}
    </div>
  )
}

function Row({ x }: { x: UsageRiskyRow }) {
  const outcome =
    OUTCOME[x.outcome] +
    (x.outcome === 'refused' && x.source ? ` by ${SOURCES[x.source] ?? x.source}` : '')
  return (
    <li className="border-b border-line px-3 py-3 last:border-b-0 sm:px-4">
      <div className="flex items-start gap-2">
        <Badge
          color={x.level === 'high' ? 'var(--wp-danger)' : 'var(--wp-warn)'}
          className="mt-px shrink-0 capitalize"
        >
          {x.level}
        </Badge>
        <p className="min-w-0 flex-1 text-[13px] leading-5 [overflow-wrap:anywhere]">
          <span className="font-medium">{x.label}</span>
          <span className={clsx('ml-1.5', x.outcome === 'refused' ? 'text-ok' : 'text-muted')}>
            · {outcome}
          </span>
        </p>
        <time
          dateTime={x.at}
          title={dateTime(x.at)}
          className="shrink-0 pt-0.5 text-xs text-subtle tabular-nums"
        >
          {relativeTime(x.at)}
        </time>
      </div>
      <pre className="mt-1.5 max-h-24 overflow-auto whitespace-pre-wrap break-all rounded-md bg-sunken px-2 py-1.5 font-mono text-xs text-muted">
        {x.command}
        {x.segment && (
          <>
            {'\n'}
            <span className="text-fg">…{x.segment}</span>
          </>
        )}
      </pre>
      <div className="mt-1 flex flex-wrap items-center gap-x-2 gap-y-0.5 text-xs text-subtle">
        {x.repo && <span className="font-mono">{x.repo}</span>}
        {x.issue && (
          <Link
            to="/issue/$key"
            params={{ key: x.issue.key }}
            className="hover:text-fg hover:underline"
            title={x.issue.title}
          >
            {x.issue.key}
          </Link>
        )}
        <span>{x.machine}</span>
        {x.model && <span className="font-mono">{x.model}</span>}
      </div>
    </li>
  )
}
