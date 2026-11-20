// Agents → Sessions (WP-D16): every agent session your hooks have seen, newest
// first, by machine, repository, branch and state. Each opens its own page.

import { useMemo } from 'react'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { Terminal } from 'lucide-react'
import { api, qs } from '@/lib/api'
import { sessionsQuery, type SessionPage } from '@/lib/sessions'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/input'
import { Empty, LoadError, Skeleton } from '@/components/ui/misc'
import { SessionRow } from './Overview'
import { PauseSessionsButton, SessionPauseBanners, useSessionPauses } from './SessionPause'
import type { HubTabProps } from './search'

const STATES = [
  { id: 'active', label: 'Active' },
  { id: 'working', label: 'Working' },
  { id: 'waiting', label: 'Waiting for you' },
  { id: 'idle', label: 'Idle' },
  { id: 'ended', label: 'Ended' },
  { id: 'all', label: 'Any state' },
]

const PAGE = 30

export function SessionsTab({ filters, setFilters }: HubTabProps) {
  const state = filters.state ?? 'active'
  const f = {
    agent: filters.agent || undefined,
    project: filters.project || undefined,
    machine: filters.machine,
    repo: filters.repo,
    branch: filters.branch,
    state: state === 'all' ? undefined : state,
  }
  const list = useInfiniteQuery({
    queryKey: ['sessions', 'tab', f],
    queryFn: ({ pageParam }) =>
      api.get<SessionPage>(`/sessions${qs({ ...f, limit: PAGE, offset: pageParam || undefined })}`),
    initialPageParam: 0,
    getNextPageParam: (last) => last.next,
    staleTime: 15_000,
    placeholderData: (prev) => prev,
  })
  // Where to pick from: the places the latest sessions ran.
  const places = useQuery(
    sessionsQuery({
      agent: filters.agent || undefined,
      project: filters.project || undefined,
      limit: 200,
    }),
  )
  const facets = useMemo(() => {
    const machines = new Set<string>()
    const repos = new Set<string>()
    const branches = new Set<string>()
    for (const s of places.data?.sessions ?? []) {
      machines.add(s.machine)
      if (s.repo) repos.add(s.repo)
      if (s.branch) branches.add(s.branch)
    }
    for (const [set, v] of [
      [machines, filters.machine],
      [repos, filters.repo],
      [branches, filters.branch],
    ] as const)
      if (v) set.add(v)
    const sorted = (s: Set<string>) => [...s].sort((a, b) => a.localeCompare(b))
    return { machines: sorted(machines), repos: sorted(repos), branches: sorted(branches) }
  }, [places.data, filters.machine, filters.repo, filters.branch])

  const paused = useSessionPauses()
  const sessions = list.data?.pages.flatMap((p) => p.sessions) ?? []
  const total = list.data?.pages[0]?.total ?? 0
  const narrowed = !!(
    filters.machine ||
    filters.repo ||
    filters.branch ||
    filters.agent ||
    filters.project
  )
  return (
    <div className="space-y-3">
      {paused && <SessionPauseBanners pauses={paused} />}
      <div className="flex flex-wrap items-center gap-2">
        <Select
          aria-label="State"
          value={state}
          onChange={(e) =>
            setFilters({ state: e.target.value === 'active' ? undefined : e.target.value })
          }
          className="!w-auto min-w-32"
        >
          {STATES.map((s) => (
            <option key={s.id} value={s.id}>
              {s.label}
            </option>
          ))}
        </Select>
        <FacetSelect
          label="Machine"
          all="All machines"
          value={filters.machine}
          options={facets.machines}
          onChange={(machine) => setFilters({ machine })}
        />
        <FacetSelect
          label="Repository"
          all="All repositories"
          value={filters.repo}
          options={facets.repos}
          onChange={(repo) => setFilters({ repo })}
        />
        <FacetSelect
          label="Branch"
          all="All branches"
          value={filters.branch}
          options={facets.branches}
          onChange={(branch) => setFilters({ branch })}
        />
        <div className="ml-auto flex items-center gap-2">
          {list.data && (
            <span className="text-xs text-subtle">
              {total} {total === 1 ? 'session' : 'sessions'}
            </span>
          )}
          {paused && !paused.some((p) => p.spec === 'any') && <PauseSessionsButton />}
        </div>
      </div>
      {list.isPending ? (
        <Skeleton className="h-40 w-full" />
      ) : list.isError && sessions.length === 0 ? (
        <LoadError what="The sessions" error={list.error} />
      ) : sessions.length === 0 ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty
            icon={<Terminal />}
            title={narrowed || state !== 'active' ? 'No sessions match' : 'No agent sessions yet'}
          >
            {narrowed || state !== 'active' ? (
              'Try another state, machine or branch.'
            ) : (
              <>
                Sessions show up while agents run, once{' '}
                <code className="rounded bg-sunken px-1">wp hook claude</code> is set up on a
                machine.
              </>
            )}
          </Empty>
        </div>
      ) : (
        <ul
          className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel"
          aria-label="Sessions"
        >
          {sessions.map((s) => (
            <li key={s.id}>
              <SessionRow s={s} showWhere />
            </li>
          ))}
        </ul>
      )}
      {list.hasNextPage && (
        <div className="flex justify-center">
          <Button
            size="sm"
            variant="ghost"
            loading={list.isFetchingNextPage}
            onClick={() => list.fetchNextPage()}
          >
            Show more
          </Button>
        </div>
      )}
    </div>
  )
}

function FacetSelect({
  label,
  all,
  value,
  options,
  onChange,
}: {
  label: string
  all: string
  value?: string
  options: string[]
  onChange: (v: string | undefined) => void
}) {
  if (options.length < 2 && !value) return null
  return (
    <Select
      aria-label={label}
      value={value ?? ''}
      onChange={(e) => onChange(e.target.value || undefined)}
      className="!w-auto min-w-32 max-w-56"
    >
      <option value="">{all}</option>
      {options.map((o) => (
        <option key={o} value={o}>
          {o}
        </option>
      ))}
    </Select>
  )
}
