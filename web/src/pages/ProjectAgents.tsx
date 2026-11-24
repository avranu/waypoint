import { lazy, Suspense, useState, type ReactNode } from 'react'
import { Link } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CirclePause, CirclePlay, GitPullRequest, Settings2 } from 'lucide-react'
import { api, enc } from '@/lib/api'
import { invalidateProject } from '@/lib/queries'
import { agentLabel } from '@/lib/agents'
import { relativeTime } from '@/lib/format'
import { kindLabel, requestsQuery } from '@/lib/requests'
import { pullsQuery } from '@/lib/pulls'
import { sessionsQuery } from '@/lib/sessions'
import type { Project } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { toast, toastError } from '@/components/ui/toast'
import { requestIcon } from '@/components/request-icons'
import { RequestAge } from '@/components/WaitingOnYou'
import { QueueTab } from './agents/Queue'
import { ProjectAutopilot } from './agents/Autopilot'
import { SessionRow } from './agents/Overview'
import { MergeFacts } from './agents/Pulls'
import { DEFAULT_DAYS, type HubFilters } from './agents/search'
import { useProject } from './Project'

const RequestSheet = lazy(() => import('./Request').then((m) => ({ default: m.RequestSheet })))

export function ProjectAgentsPage() {
  const project = useProject()
  const [request, setRequest] = useState<string | null>(null)
  const filters: HubFilters = { agent: '', project: project.key, days: DEFAULT_DAYS }
  const canEdit = project.role === 'owner' || project.role === 'editor'
  return (
    <div className="min-h-0 flex-1 overflow-y-auto pb-16">
      <div className="mx-auto w-full max-w-5xl space-y-6 px-4 pt-4 sm:px-6">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h2 className="text-[15px] font-semibold tracking-tight">Agents</h2>
          <div className="flex items-center gap-1">
            {canEdit && <PauseButton project={project} />}
            <Link
              to="/p/$key/settings"
              params={{ key: project.key }}
              search={{ tab: 'bots' }}
              className="inline-flex h-8 items-center gap-1.5 rounded-lg px-2 text-[13px] text-muted hover:bg-hover hover:text-fg"
            >
              <Settings2 className="size-3.5" /> Bots
            </Link>
          </div>
        </div>
        <Requests project={project.key} onOpen={setRequest} />
        {canEdit && (
          <Block title="Autopilot">
            <ProjectAutopilot projectKey={project.key} />
          </Block>
        )}
        <Sessions project={project.key} />
        <Block title="Queue">
          <QueueTab filters={filters} setFilters={() => {}} embedded />
        </Block>
        <Pulls project={project.key} />
      </div>
      {request && (
        <Suspense fallback={null}>
          <RequestSheet code={request} onClose={() => setRequest(null)} />
        </Suspense>
      )}
    </div>
  )
}

function Block({
  title,
  count,
  action,
  children,
}: {
  title: string
  count?: number
  action?: ReactNode
  children: ReactNode
}) {
  return (
    <section aria-label={title} className="space-y-2">
      <div className="flex items-center justify-between gap-2">
        <h3 className="flex items-center gap-2 text-xs font-semibold uppercase tracking-wide text-subtle">
          {title}
          {count != null && <span className="font-normal normal-case">{count}</span>}
        </h3>
        {action}
      </div>
      {children}
    </section>
  )
}

/** Agents may take new work here: the project's own switch (General settings). */
function PauseButton({ project }: { project: Project }) {
  const qc = useQueryClient()
  const paused = !!project.settings.agents_paused
  const toggle = useMutation({
    mutationFn: () =>
      api.patch<Project>(`/projects/${enc(project.key)}`, {
        settings: { ...project.settings, agents_paused: !paused },
      }),
    onSuccess: (p) => {
      qc.setQueryData(['project', p.key], p)
      invalidateProject(qc, project.key)
      qc.invalidateQueries({ queryKey: ['agents'] })
      toast(paused ? 'Agents take work here again' : 'Agents paused in this project')
    },
    onError: toastError,
  })
  return (
    <Button
      size="sm"
      icon={paused ? <CirclePlay className="size-3.5" /> : <CirclePause className="size-3.5" />}
      loading={toggle.isPending}
      onClick={() => toggle.mutate()}
    >
      {paused ? 'Resume agents' : 'Pause agents'}
    </Button>
  )
}

function Requests({ project, onOpen }: { project: string; onOpen: (ref: string) => void }) {
  const list = useQuery(requestsQuery({ project, state: 'open' })).data?.requests ?? []
  if (!list.length) return null
  return (
    <Block title="Needs you" count={list.length}>
      <ul className="overflow-hidden rounded-xl border border-line bg-panel">
        {list.map((r) => (
          <li key={r.id} className="border-b border-line last:border-b-0">
            <button
              onClick={() => onOpen(r.ref)}
              className="flex w-full items-center gap-3 px-4 py-3 text-left hover:bg-hover"
            >
              <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-accent-soft text-fg [&>svg]:size-4">
                {requestIcon[r.kind]}
              </span>
              <span className="min-w-0 flex-1">
                <span className="block truncate text-[14px] font-medium">{r.title}</span>
                <span className="block truncate text-xs text-muted">
                  {kindLabel[r.kind]}
                  {r.agent ? ` · ${agentLabel(r.agent)}` : ''}
                </span>
              </span>
              <RequestAge r={r} />
            </button>
          </li>
        ))}
      </ul>
    </Block>
  )
}

function Sessions({ project }: { project: string }) {
  const data = useQuery(sessionsQuery({ project, state: 'active', limit: 10 })).data
  if (!data?.sessions.length) return null
  return (
    <Block
      title="Sessions now"
      count={data.total}
      action={
        <Link
          to="/agents"
          search={{ tab: 'sessions', project }}
          className="text-xs text-muted hover:text-fg"
        >
          All sessions
        </Link>
      }
    >
      <ul
        className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel"
        aria-label="Sessions"
      >
        {data.sessions.map((s) => (
          <li key={s.id}>
            <SessionRow s={s} showWhere />
          </li>
        ))}
      </ul>
    </Block>
  )
}

function Pulls({ project }: { project: string }) {
  const all = useQuery(pullsQuery({ project, agent: '', days: DEFAULT_DAYS, state: 'open', q: '' }))
    .data?.pulls
  const pulls = (all ?? []).filter((p) => p.agents.length > 0)
  if (!pulls.length) return null
  return (
    <Block
      title="Agent pull requests"
      count={pulls.length}
      action={
        <Link
          to="/agents"
          search={{ tab: 'pulls', project }}
          className="text-xs text-muted hover:text-fg"
        >
          All pull requests
        </Link>
      }
    >
      <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel">
        {pulls.map((pr) => (
          <li key={pr.id}>
            <Link
              to="/agents"
              search={{ tab: 'pulls', pr: pr.id, project }}
              className="flex items-start gap-3 px-4 py-3 hover:bg-hover"
            >
              <GitPullRequest className="mt-0.5 size-4 shrink-0 text-subtle" />
              <span className="min-w-0 flex-1">
                <span className="block truncate text-[13px] font-medium">{pr.title}</span>
                <span className="block truncate text-xs text-subtle">
                  {pr.repo}#{pr.number} · {agentLabel(pr.agents[0])}
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
    </Block>
  )
}
