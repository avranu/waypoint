// The list refreshes on cron.changed from the live stream; Run now's request stays open until Hermes answers.

import { useMemo, useState } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { AlertTriangle, CalendarClock, Plus } from 'lucide-react'
import { hermes, type Assistant } from '@/lib/assistant'
import { duration } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Empty, Skeleton } from '@/components/ui/misc'
import { toast, toastError } from '@/components/ui/toast'
import {
  configQuery,
  dropJob,
  jobPath,
  jobsQuery,
  targetsQuery,
  useRunning,
  type CronJob,
} from './schedules/api'
import { hermesZone } from './schedules/schedule'
import { JobEditor } from './schedules/JobEditor'
import { JobRow, jobState, type JobState } from './schedules/JobRow'
import { Blueprints } from './schedules/Blueprints'
import type { AssistantSearch } from './search'

const ORDER: Record<JobState, number> = {
  running: 0,
  scheduled: 0,
  paused: 1,
  error: 2,
  completed: 3,
}

/** Seconds without a scheduler tick after which nothing is running on time (Hermes's own STALE_AFTER). */
const SCHEDULER_STALE_S = 200

export function SchedulesTab({ assistant }: { assistant: Assistant }) {
  const id = assistant.id
  const qc = useQueryClient()
  const jobs = useQuery({ ...jobsQuery(id), refetchInterval: 60_000 })
  const tz = useQuery({ ...configQuery(id), retry: false }).data?.timezone
  const targets = useQuery(targetsQuery(id)).data?.targets
  const running = useRunning()
  const search = useSearch({ from: '/app/assistant/$id' })
  const navigate = useNavigate({ from: '/assistant/$id' })
  const [editing, setEditing] = useState<CronJob | 'new' | null>(null)
  const [deleting, setDeleting] = useState<CronJob | null>(null)

  const list = jobs.data
  const zone = useMemo(
    () =>
      hermesZone(
        tz,
        (list ?? []).flatMap((j) => [j.created_at, j.next_run_at]),
      ),
    [tz, list],
  )
  const deliverNames = useMemo(() => new Map((targets ?? []).map((t) => [t.id, t.name])), [targets])
  const sorted = useMemo(() => {
    const at = (s: string | null) => (s ? new Date(s).getTime() : Infinity)
    return [...(list ?? [])]
      .map((j) => ({ j, s: jobState(j, running.has(`${id}:${j.id}`)) }))
      .sort(
        (a, b) =>
          ORDER[a.s] - ORDER[b.s] ||
          (ORDER[a.s] === 0
            ? at(a.j.next_run_at) - at(b.j.next_run_at)
            : at(b.j.last_run_at) - at(a.j.last_run_at)) ||
          a.j.name.localeCompare(b.j.name),
      )
      .map((x) => x.j)
  }, [list, running, id])

  const openJob = (job: string | undefined) =>
    navigate({ search: (s: AssistantSearch) => ({ ...s, view: job }), replace: true })

  const stale = (list ?? []).reduce<number | null>((worst, j) => {
    const age = j.scheduler_heartbeat_age_s
    if (typeof age !== 'number' || age <= SCHEDULER_STALE_S || !j.enabled) return worst
    return Math.max(worst ?? 0, age)
  }, null)

  const remove = (j: CronJob) =>
    hermes(id, 'DELETE', jobPath(j.id)).then(() => {
      dropJob(qc, id, j.id)
      if (search.view === j.id) openJob(undefined)
      toast(`Deleted “${j.name}”`)
    }, toastError)

  const paused = sorted.filter((j) => j.state === 'paused').length
  const newButton = (
    <Button
      variant="primary"
      size="sm"
      icon={<Plus className="size-3.5" />}
      onClick={() => setEditing('new')}
      className="max-sm:h-9"
    >
      New schedule
    </Button>
  )

  return (
    <div className="space-y-4">
      {jobs.isPending ? (
        <div className="space-y-2">
          <Skeleton className="h-8 w-48" />
          <Skeleton className="h-16" />
          <Skeleton className="h-16" />
        </div>
      ) : jobs.isError ? (
        <div className="rounded-xl border border-line bg-panel">
          <Empty
            icon={<AlertTriangle />}
            title="Couldn't load the schedules"
            action={
              <Button size="sm" onClick={() => jobs.refetch()} loading={jobs.isFetching}>
                Try again
              </Button>
            }
          >
            {jobs.error.message}
          </Empty>
        </div>
      ) : sorted.length === 0 ? (
        <div className="rounded-xl border border-line bg-panel">
          <Empty icon={<CalendarClock />} title="No schedules yet" action={newButton}>
            Have {assistant.name} do something on its own, on a schedule: a morning briefing, an
            inbox check, a weekly review.
          </Empty>
        </div>
      ) : (
        <>
          <div className="flex items-center justify-between gap-3">
            <h2 className="text-[13px] text-muted">
              <span className="font-semibold text-fg">
                {sorted.length} schedule{sorted.length === 1 ? '' : 's'}
              </span>
              {paused > 0 && ` · ${paused} paused`}
            </h2>
            {newButton}
          </div>
          {stale !== null && (
            <div className="flex items-start gap-2 rounded-lg border border-warn/30 bg-warn/10 px-3 py-2 text-[13px]">
              <AlertTriangle className="mt-0.5 size-4 shrink-0 text-warn" />
              <span>
                Hermes's scheduler last ticked {duration(stale)} ago, so nothing runs on time until
                it is back.
              </span>
            </div>
          )}
          <ul className="divide-y divide-line overflow-hidden rounded-xl border border-line bg-panel">
            {sorted.map((j) => (
              <JobRow
                key={j.id}
                assistantId={id}
                job={j}
                zone={zone}
                running={running.get(`${id}:${j.id}`)}
                open={search.view === j.id}
                onToggle={() => openJob(search.view === j.id ? undefined : j.id)}
                onEdit={() => setEditing(j)}
                onDelete={() => setDeleting(j)}
                deliverNames={deliverNames}
              />
            ))}
          </ul>
        </>
      )}
      {!jobs.isError && <Blueprints assistantId={id} onCreated={(j) => openJob(j.id)} />}
      {editing && (
        <JobEditor
          assistantId={id}
          job={editing === 'new' ? undefined : editing}
          zone={zone}
          onClose={() => setEditing(null)}
          onSaved={(j) => openJob(j.id)}
        />
      )}
      <Confirm
        open={!!deleting}
        onClose={() => setDeleting(null)}
        title={`Delete “${deleting?.name}”?`}
        body="It stops running, and its saved output goes with it. Its past runs stay in Chat."
        onConfirm={() => deleting && remove(deleting)}
      />
    </div>
  )
}
