// Objectives (WP-927): every task worked in rounds that you can see, the ones
// that need a person first (a proposal to read, a block to clear, a stall).
// Narrowed by project and by where each stands; met and ended ones on request.

import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import clsx from 'clsx'
import { Repeat, Target } from 'lucide-react'
import { q } from '@/lib/queries'
import { HEALTH, HEALTH_ORDER, needsPerson } from '@/lib/objective'
import type { Objective, ObjectiveHealth } from '@/lib/types'
import { relativeTime } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Select, Switch } from '@/components/ui/input'
import { Empty, LoadError, PageHeader, Skeleton } from '@/components/ui/misc'
import { Explainer } from '@/components/ui/help'
import { HealthBadge, Sentence, stop } from '@/components/Objective'

function Chip({
  on,
  onClick,
  children,
}: {
  on: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-pressed={on}
      className={clsx(
        'h-7 rounded-md px-2.5 text-xs tabular-nums',
        on ? 'bg-accent-soft font-medium text-fg' : 'text-muted hover:bg-hover hover:text-fg',
      )}
    >
      {children}
    </button>
  )
}

function Row({ o }: { o: Objective }) {
  const Icon = o.kind === 'loop' ? Repeat : Target
  return (
    <li>
      <Link
        to="/issue/$key"
        params={{ key: o.key }}
        className="flex items-start gap-3 px-3 py-2.5 text-[13px] hover:bg-hover focus-visible:bg-hover focus-visible:outline-none sm:px-4"
      >
        <Icon className="mt-0.5 size-4 shrink-0 text-subtle" aria-hidden />
        <div className="min-w-0 flex-1 space-y-1">
          <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
            <span className="font-mono text-xs text-subtle">{o.key}</span>
            <span className="min-w-0 font-medium [overflow-wrap:anywhere]">{o.title}</span>
            <HealthBadge objective={o} />
          </div>
          <p className="text-muted [overflow-wrap:anywhere]">
            <Sentence text={o.summary} />
            {stop(o.summary)}
          </p>
          {o.health_note && (
            <p
              className={clsx(
                '[overflow-wrap:anywhere]',
                needsPerson(o) ? 'text-fg' : 'text-muted',
              )}
            >
              <Sentence text={o.health_note} />
            </p>
          )}
          <p className="text-xs text-subtle sm:hidden" title={o.updated_at}>
            {o.state === 'active' && (
              <>
                round {o.round}
                {o.max_rounds ? ` of ${o.max_rounds}` : ''} ·{' '}
              </>
            )}
            {relativeTime(o.updated_at)}
          </p>
          {o.verdict && o.state === 'active' && (
            <p className="text-subtle [overflow-wrap:anywhere]">
              Waypoint’s last reading: {o.verdict.line}
            </p>
          )}
        </div>
        <div className="hidden shrink-0 text-right text-xs text-subtle sm:block">
          {o.state === 'active' && (
            <div>
              round {o.round}
              {o.max_rounds ? ` of ${o.max_rounds}` : ''}
            </div>
          )}
          <div title={o.updated_at}>{relativeTime(o.updated_at)}</div>
        </div>
      </Link>
    </li>
  )
}

export function ObjectivesPage() {
  const [project, setProject] = useState('')
  const [all, setAll] = useState(false)
  const [health, setHealth] = useState<ObjectiveHealth | ''>('')
  const projects = useQuery(q.projects())
  const list = useQuery(q.objectives({ project, all }))
  const rows = list.data ?? []
  const counts = new Map<ObjectiveHealth, number>()
  for (const o of rows) counts.set(o.health, (counts.get(o.health) ?? 0) + 1)
  const shown = rows
    .filter((o) => !health || o.health === health)
    // The server puts those that need a person first; keep them there.
    .sort((a, b) => Number(needsPerson(b)) - Number(needsPerson(a)))
  const waiting = rows.filter(needsPerson).length
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-5xl space-y-4 pb-6">
        <PageHeader
          icon={<Target className="size-5 text-subtle" aria-hidden />}
          title="Objectives"
          subtitle={
            list.data
              ? waiting > 0
                ? `${waiting} need${waiting === 1 ? 's' : ''} you`
                : 'Nothing needs you'
              : undefined
          }
        />
        <div className="space-y-4 px-4 sm:px-6">
          <Explainer id="objectives" label="What an objective is">
            A task with an objective is worked in rounds until a check is met, a reviewer finds
            nothing, or you end it. Waypoint reads every round itself: the commands on the runner of
            the person who confirmed it, then a reviewer’s audit. What the worker measures along the
            way is shown but never decides. Agents propose; a person confirms.
          </Explainer>
          <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
            <div className="w-48">
              <Select
                aria-label="Project"
                value={project}
                onChange={(e) => setProject(e.target.value)}
              >
                <option value="">All projects</option>
                {(projects.data ?? []).map((p) => (
                  <option key={p.key} value={p.key}>
                    {p.name}
                  </option>
                ))}
              </Select>
            </div>
            <div className="flex items-center gap-2 text-[13px]">
              <span>Include met and ended</span>
              <Switch checked={all} onChange={setAll} label="Include met and ended" />
            </div>
          </div>
          <div
            className="flex flex-wrap items-center gap-1"
            role="group"
            aria-label="Where it stands"
          >
            <Chip on={!health} onClick={() => setHealth('')}>
              All {rows.length}
            </Chip>
            {HEALTH_ORDER.filter((h) => counts.get(h)).map((h) => (
              <Chip key={h} on={health === h} onClick={() => setHealth(health === h ? '' : h)}>
                {HEALTH[h].label} {counts.get(h)}
              </Chip>
            ))}
          </div>
          {list.isPending ? (
            <div className="space-y-2" aria-busy="true">
              <Skeleton className="h-16" />
              <Skeleton className="h-16" />
              <Skeleton className="h-16" />
            </div>
          ) : list.isError ? (
            <div className="rounded-xl border border-line bg-panel">
              <LoadError what="Objectives" error={list.error} />
              <div className="pb-4 text-center">
                <Button size="sm" onClick={() => void list.refetch()}>
                  Try again
                </Button>
              </div>
            </div>
          ) : shown.length === 0 ? (
            <div className="rounded-xl border border-line bg-panel">
              <Empty
                icon={<Target />}
                title={health || project ? 'No objectives match' : 'No objectives yet'}
              >
                {!health && !project && (
                  <>
                    Give a task one from its menu (Set an objective…), or have an agent propose one
                    with <code>wp objective set</code>.
                  </>
                )}
              </Empty>
            </div>
          ) : (
            <ul
              aria-label="Objectives"
              className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel"
            >
              {shown.map((o) => (
                <Row key={o.key} o={o} />
              ))}
            </ul>
          )}
        </div>
      </div>
    </div>
  )
}
