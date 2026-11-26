import { useEffect, useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import { ChevronRight, Plus, Server } from 'lucide-react'
import { q } from '@/lib/queries'
import {
  SEVERITY,
  envsQuery,
  isDev,
  sortEnvs,
  type Environment,
  type EnvStatus,
} from '@/lib/environments'
import { useProject } from './Project'
import { AddEnvironmentDialog, EnvCard, EnvRow, StatusDot } from '@/components/environments'
import { ProjectIcon } from '@/components/issue/icons'
import { Button } from '@/components/ui/button'
import { Tip } from '@/components/ui/help'
import { Empty, LoadError, PageHeader, Section, Skeleton } from '@/components/ui/misc'

/** "2 up · 1 down": counts with a dot each, only the states that occur. */
function Counts({ list }: { list: Environment[] }) {
  const live = list.filter((e) => !e.stopped)
  const n = (s: EnvStatus) => live.filter((e) => e.status === s).length
  const parts: { status: EnvStatus; text: string }[] = (
    [
      ['down', 'down'],
      ['degraded', 'degraded'],
      ['up', 'up'],
      ['unknown', 'not checked'],
    ] as const
  )
    .filter(([s]) => n(s) > 0)
    .map(([s, label]) => ({ status: s, text: `${n(s)} ${label}` }))
  if (!parts.length) return null
  return (
    <span className="flex flex-wrap items-center gap-x-3 gap-y-1 text-[13px] text-muted">
      {parts.map((p) => (
        <span key={p.status} className="inline-flex items-center gap-1.5">
          <StatusDot env={{ status: p.status, stopped: false }} className="!ring-0" />
          {p.text}
        </span>
      ))}
    </span>
  )
}

export function EnvironmentsPage() {
  const query = useQuery(envsQuery(undefined, false))
  const projects = useQuery(q.projects()).data ?? []
  useEffect(() => {
    document.title = 'Environments · Waypoint'
  }, [])
  const groups = useMemo(() => {
    const by = new Map<string, Environment[]>()
    for (const e of query.data ?? []) by.set(e.project, [...(by.get(e.project) ?? []), e])
    const worst = (l: Environment[]) => Math.min(...l.map((e) => SEVERITY[e.status]))
    return [...by.entries()]
      .map(([key, list]) => ({ key, name: list[0].project_name, list: sortEnvs(list, true) }))
      .sort((a, b) => worst(a.list) - worst(b.list) || a.name.localeCompare(b.name))
  }, [query.data])
  const all = query.data ?? []
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto pb-16">
      <div className="mx-auto w-full max-w-screen-2xl">
        <PageHeader
          icon={<Server className="size-5 text-accent" />}
          title="Environments"
          subtitle={<Counts list={all} />}
        />
        <div className="space-y-4 px-4 pb-6 sm:px-6">
          {query.isPending ? (
            <div className="space-y-3">
              <Skeleton className="h-24" />
              <Skeleton className="h-24" />
            </div>
          ) : query.isError ? (
            <LoadError what="Environments" error={query.error} />
          ) : groups.length === 0 ? (
            <Empty icon={<Server />} title="No environments yet">
              <span title="Add them in a project’s Environments tab, or have CI record a deployment with wp record deploy">
                Add the places your code runs in a project’s Environments tab.
              </span>
            </Empty>
          ) : (
            groups.map((g) => {
              const p = projects.find((x) => x.key === g.key)
              return (
                <Section
                  key={g.key}
                  title={
                    <Link
                      to="/p/$key/environments"
                      params={{ key: g.key }}
                      className="inline-flex items-center gap-2 hover:underline"
                    >
                      {p && <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={18} />}
                      {g.name}
                      <ChevronRight className="size-3.5 text-subtle" />
                    </Link>
                  }
                  actions={groups.length > 1 ? <Counts list={g.list} /> : undefined}
                >
                  <div className="divide-y divide-line">
                    {g.list.map((e) => (
                      <EnvRow key={e.id} env={e} />
                    ))}
                  </div>
                </Section>
              )
            })
          )}
        </div>
      </div>
    </div>
  )
}

export function ProjectEnvironmentsPage() {
  const project = useProject()
  const query = useQuery(envsQuery(project.key, true))
  const [adding, setAdding] = useState(false)
  const canEdit = project.role === 'owner' || project.role === 'editor'
  const list = useMemo(() => sortEnvs(query.data ?? [], false), [query.data])
  const main = list.filter((e) => !e.stopped && !isDev(e))
  const dev = list.filter((e) => !e.stopped && isDev(e))
  const stopped = list.filter((e) => e.stopped)
  useEffect(() => {
    document.title = `Environments · ${project.name} · Waypoint`
  }, [project.name])
  return (
    <div className="min-h-0 flex-1 overflow-y-auto pb-20">
      <div className="mx-auto w-full max-w-screen-2xl px-4 pt-4 sm:px-6">
        <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
          <div className="flex min-w-0 flex-col gap-0.5">
            <h2 className="text-base font-semibold">Environments</h2>
            <Counts list={list} />
          </div>
          {canEdit && list.length > 0 && (
            <Button
              variant="primary"
              size="sm"
              icon={<Plus className="size-3.5" />}
              onClick={() => setAdding(true)}
            >
              Add environment
            </Button>
          )}
        </div>
        {query.isPending ? (
          <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,21rem),1fr))] gap-3">
            <Skeleton className="h-44" />
            <Skeleton className="h-44" />
          </div>
        ) : list.length === 0 ? (
          <div className="rounded-xl border border-dashed border-line">
            <Empty
              icon={<Server />}
              title="No environments yet"
              action={
                canEdit && (
                  <Button
                    variant="primary"
                    icon={<Plus className="size-4" />}
                    onClick={() => setAdding(true)}
                  >
                    Add environment
                  </Button>
                )
              }
            >
              <span title="CI can record deployments with wp record deploy, and a cluster can register dev environments with wp env up">
                Add production and staging to see whether they are up and what they run.
              </span>
            </Empty>
          </div>
        ) : (
          <div className="space-y-6">
            {main.length > 0 && (
              <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,21rem),1fr))] gap-3">
                {main.map((e) => (
                  <EnvCard key={e.id} env={e} />
                ))}
              </div>
            )}
            <section aria-labelledby="dev-envs">
              <div className="mb-2 flex items-center justify-between gap-2">
                <h3 id="dev-envs" className="text-[13px] font-semibold">
                  Dev environments
                </h3>
              </div>
              {dev.length === 0 ? (
                <p className="flex items-center gap-1.5 text-[13px] text-muted">
                  None right now.
                  <Tip label="How dev environments appear">
                    A cluster or CI creates them with{' '}
                    <code className="font-mono text-xs">wp env up NAME</code>
                    {project.settings.dev_env_url
                      ? `, at ${project.settings.dev_env_url}`
                      : '; set a dev environment address in the project’s settings so the name is enough'}
                    .
                  </Tip>
                </p>
              ) : (
                <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,21rem),1fr))] gap-3">
                  {dev.map((e) => (
                    <EnvCard key={e.id} env={e} />
                  ))}
                </div>
              )}
            </section>
            {stopped.length > 0 && (
              <details className="group rounded-xl border border-line bg-panel">
                <summary className="flex h-10 cursor-pointer list-none items-center gap-2 px-4 text-[13px] text-muted hover:text-fg [&::-webkit-details-marker]:hidden">
                  <ChevronRight className="size-3.5 transition-transform group-open:rotate-90" />
                  <span className="whitespace-nowrap">Stopped ({stopped.length})</span>
                  <span className="hidden text-xs text-subtle sm:inline">
                    dev environments are forgotten after a week
                  </span>
                </summary>
                <div className="grid grid-cols-[repeat(auto-fill,minmax(min(100%,21rem),1fr))] gap-3 border-t border-line p-3">
                  {stopped.map((e) => (
                    <EnvCard key={e.id} env={e} />
                  ))}
                </div>
              </details>
            )}
          </div>
        )}
      </div>
      <AddEnvironmentDialog project={project.key} open={adding} onClose={() => setAdding(false)} />
    </div>
  )
}
