import { useSyncExternalStore } from 'react'
import type { QueryClient } from '@tanstack/react-query'
import { enc } from '@/lib/api'
import { hermes, hermesQuery } from '@/lib/assistant'
import type { StoredSchedule } from './schedule'

export interface CronJob {
  id: string
  name: string
  prompt: string
  skills: string[]
  model: string | null
  provider: string | null
  script: string | null
  no_agent: boolean
  schedule: StoredSchedule
  schedule_display: string
  repeat: { times: number | null; completed: number }
  enabled: boolean
  state: 'scheduled' | 'paused' | 'completed' | 'error' | string
  paused_at: string | null
  paused_reason: string | null
  created_at: string
  next_run_at: string | null
  last_run_at: string | null
  last_status: 'ok' | 'error' | 'delivery_failed' | string | null
  last_error: string | null
  last_delivery_error: string | null
  last_failure: { at: string; detail: string } | null
  failure_streak: number
  deliver: string
  enabled_toolsets: string[] | null
  workdir: string | null
  latest_execution?: {
    status: string
    started_at?: string | null
    claimed_at?: string | null
  } | null
  scheduler_heartbeat_age_s?: number | null
}

/** A run: a cron session (id cron_<job>_<stamp>), or a script's output doc (id cron_output:…). */
export interface CronRun {
  id: string
  source: string
  title: string | null
  preview: string | null
  model: string | null
  started_at: number
  ended_at: number | null
  end_reason?: string | null
  last_active: number
  message_count: number
  tool_call_count: number
  input_tokens: number
  output_tokens: number
  estimated_cost_usd?: number | null
  is_active: boolean
  scheduler_owned?: boolean
}

export interface DeliveryTarget {
  id: string
  name: string
  home_target_set: boolean
}

export interface BlueprintField {
  name: string
  type: 'time' | 'enum' | 'weekdays' | 'text' | string
  label: string
  default: string | null
  options: string[]
  optional: boolean
  help: string
}

export interface Blueprint {
  key: string
  title: string
  description: string
  category: string
  tags: string[]
  fields: BlueprintField[]
  scheduleHuman: string
}

export interface Skill {
  name: string
  description: string
  category: string
  enabled: boolean
}

export interface Toolset {
  name: string
  label: string
  description: string
  enabled: boolean
}

export const jobsQuery = (id: number) => hermesQuery<CronJob[]>(id, 'cron/jobs', undefined, 10_000)

export const runsQuery = (id: number, job: string) =>
  hermesQuery<{ runs: CronRun[] }>(id, `cron/jobs/${enc(job)}/runs`, { limit: 20 }, 10_000)

export const targetsQuery = (id: number) =>
  hermesQuery<{ targets: DeliveryTarget[] }>(id, 'cron/delivery-targets', undefined, 60_000)

export const blueprintsQuery = (id: number) =>
  hermesQuery<{ blueprints: Blueprint[] }>(id, 'cron/blueprints', undefined, 300_000)

export const skillsQuery = (id: number) => hermesQuery<Skill[]>(id, 'skills', undefined, 60_000)

export const toolsetsQuery = (id: number) =>
  hermesQuery<Toolset[]>(id, 'tools/toolsets', undefined, 60_000)

export const modelInfoQuery = (id: number) =>
  hermesQuery<{ model: string; provider: string }>(id, 'model/info', undefined, 60_000)

/** Hermes's config, for its timezone (shared with the settings tab's key). */
export const configQuery = (id: number) =>
  hermesQuery<{ timezone?: string }>(id, 'config', undefined, 60_000)

export const jobPath = (job: string) => `cron/jobs/${enc(job)}`

/** Put a job Hermes returned into the list, so the change shows before the refetch. */
export function putJob(qc: QueryClient, id: number, job: CronJob) {
  qc.setQueryData<CronJob[]>(jobsQuery(id).queryKey, (list) =>
    list?.map((j) => (j.id === job.id ? { ...j, ...job } : j)),
  )
}

export function dropJob(qc: QueryClient, id: number, job: string) {
  qc.setQueryData<CronJob[]>(jobsQuery(id).queryKey, (list) => list?.filter((j) => j.id !== job))
}

export const refreshJobs = (qc: QueryClient, id: number) =>
  qc.invalidateQueries({
    predicate: (q) =>
      q.queryKey[0] === 'assistant' &&
      q.queryKey[1] === id &&
      String(q.queryKey[2]).startsWith('cron/jobs'),
  })

const running = new Map<string, number>()
let snapshot: ReadonlyMap<string, number> = new Map()
const listeners = new Set<() => void>()
const emit = () => {
  snapshot = new Map(running)
  listeners.forEach((l) => l())
}

/** Jobs started with Run now from this page and not finished: key `${assistant}:${job}` → start (ms). */
export function useRunning(): ReadonlyMap<string, number> {
  return useSyncExternalStore(
    (cb) => {
      listeners.add(cb)
      return () => listeners.delete(cb)
    },
    () => snapshot,
  )
}

export async function runNow(qc: QueryClient, id: number, job: CronJob): Promise<CronJob> {
  const key = `${id}:${job.id}`
  running.set(key, Date.now())
  emit()
  setTimeout(() => refreshJobs(qc, id), 1500)
  try {
    const after = await hermes<CronJob>(id, 'POST', `${jobPath(job.id)}/trigger`)
    putJob(qc, id, after)
    return after
  } finally {
    running.delete(key)
    emit()
    refreshJobs(qc, id)
  }
}
