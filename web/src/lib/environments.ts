import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, enc, qs } from './api'
import type { UserRef } from './types'
import { toastError } from '@/components/ui/toast'

export type EnvKind = 'production' | 'staging' | 'preview' | 'dev'
export type EnvStatus = 'up' | 'degraded' | 'down' | 'unknown'
export type DeployStatus = 'started' | 'succeeded' | 'failed' | 'detected'

export interface Deployment {
  id: number
  environment_id: number
  commit: string
  commit_url?: string
  branch: string
  version: string
  status: DeployStatus
  started_at: string
  finished_at: string | null
  /** person, agent, ci or detected (seen on the version endpoint) */
  by_kind: 'person' | 'agent' | 'ci' | 'detected'
  by: UserRef | null
  agent?: string
  /** the CI run */
  url?: string
  notes?: string
}

export interface Environment {
  id: number
  project_id: number
  project: string
  project_name: string
  name: string
  kind: EnvKind
  url: string
  health_url: string
  expected_status: number
  interval_s: number
  version_url: string
  commit_field: string
  branch_field: string
  version_field: string
  ephemeral: boolean
  ttl_s: number
  expires_at: string | null
  repo_id: number | null
  repo?: string
  branch: string
  commit: string
  commit_url?: string
  version: string
  deployed_at: string | null
  behind: boolean
  head?: string
  /** Minutes it may run behind its branch before the owners are told; 0: never. */
  lag_alert_m: number
  status: EnvStatus
  status_since: string | null
  latency_ms: number | null
  http_code: number | null
  error: string
  tls_expires_at: string | null
  tls_warning: boolean
  mismatch: boolean
  last_checked_at: string | null
  uptime_24h: number | null
  uptime_30d: number | null
  /** the last 24 hours, one value per hour: percent up, -1 without checks */
  spark: number[]
  last_deploy: Deployment | null
  stopped: boolean
  stopped_at: string | null
  position: number
  created_by: UserRef | null
  created_agent?: string
  created_at: string
  updated_at: string
}

export interface UptimeDay {
  date: string
  checks: number
  ok: number
  uptime: number | null
  latency_ms: number
}

export interface HistoryPoint {
  at: string
  checks: number
  ok: number
  latency_ms: number
  max_latency_ms: number
}

export interface EnvironmentDetail extends Environment {
  deployments: Deployment[]
  days: UptimeDay[]
  history: HistoryPoint[]
}

export interface EnvCounts {
  project: string
  project_name: string
  project_id: number
  total: number
  up: number
  degraded: number
  down: number
  unknown: number
  down_names?: string[]
}

export interface EnvSummary {
  total: number
  up: number
  degraded: number
  down: number
  unknown: number
  projects: EnvCounts[]
}

export interface EnvAt {
  environment: number
  project: string
  name: string
  kind: EnvKind
  status: EnvStatus
  commit: string
  at: string
  current: boolean
}

export interface EnvInput {
  name?: string
  kind?: EnvKind
  url?: string
  health_url?: string
  expected_status?: number
  interval_s?: number
  version_url?: string
  commit_field?: string
  branch_field?: string
  version_field?: string
  ephemeral?: boolean
  ttl_s?: number
  lag_alert_m?: number
}

export const KIND_LABEL: Record<EnvKind, string> = {
  production: 'Production',
  staging: 'Staging',
  preview: 'Preview',
  dev: 'Dev',
}

export const STATUS: Record<EnvStatus | 'stopped', { label: string; color: string }> = {
  up: { label: 'Up', color: 'var(--wp-ok)' },
  degraded: { label: 'Degraded', color: 'var(--wp-warn)' },
  down: { label: 'Down', color: 'var(--wp-danger)' },
  unknown: { label: 'Not checked yet', color: 'var(--wp-subtle)' },
  stopped: { label: 'Stopped', color: 'var(--wp-subtle)' },
}

/** What to show for an environment: its status, or that it was stopped. */
export const statusOf = (e: Pick<Environment, 'status' | 'stopped'>) =>
  STATUS[e.stopped ? 'stopped' : e.status]

/** Down ones first, then degraded, unknown, up; stopped last. */
export const SEVERITY: Record<EnvStatus, number> = { down: 0, degraded: 1, unknown: 2, up: 3 }
const KIND_ORDER: Record<EnvKind, number> = { production: 0, staging: 1, preview: 2, dev: 3 }

export function sortEnvs<T extends Environment>(list: T[], byStatus: boolean): T[] {
  return [...list].sort(
    (a, b) =>
      Number(a.stopped) - Number(b.stopped) ||
      (byStatus ? SEVERITY[a.status] - SEVERITY[b.status] : 0) ||
      KIND_ORDER[a.kind] - KIND_ORDER[b.kind] ||
      a.position - b.position ||
      a.name.localeCompare(b.name),
  )
}

/** Dev and preview environments come and go, so they sit in their own section. */
export const isDev = (e: Pick<Environment, 'kind' | 'ephemeral'>) =>
  e.ephemeral || e.kind === 'dev' || e.kind === 'preview'

export const CHECK_INTERVALS: { value: number; label: string }[] = [
  { value: 0, label: 'Not checked' },
  { value: 30, label: 'Every 30 seconds' },
  { value: 60, label: 'Every minute' },
  { value: 300, label: 'Every 5 minutes' },
  { value: 900, label: 'Every 15 minutes' },
  { value: 3600, label: 'Every hour' },
]

export const LAG_ALERTS: { value: number; label: string }[] = [
  { value: 0, label: 'Never' },
  { value: 30, label: 'After 30 minutes' },
  { value: 45, label: 'After 45 minutes' },
  { value: 60, label: 'After an hour' },
  { value: 120, label: 'After 2 hours' },
  { value: 360, label: 'After 6 hours' },
  { value: 1440, label: 'After a day' },
]

export const intervalLabel = (s: number) =>
  CHECK_INTERVALS.find((i) => i.value === s)?.label ??
  (s % 60 === 0 ? `Every ${s / 60} minutes` : `Every ${s} seconds`)

/** Who did a deployment: a person, an agent, CI (with its run), or the version endpoint. */
export function deployWho(d: Deployment): string {
  switch (d.by_kind) {
    case 'detected':
      return 'Seen on the version endpoint'
    case 'agent':
      return d.agent || 'An agent'
    case 'ci':
      return d.by ? `CI (${d.by.display_name})` : 'CI'
    default:
      return d.by?.display_name ?? 'Someone'
  }
}

/** "14:02" today, "Oct 12, 14:02" otherwise. */
export function sinceTime(iso: string | null | undefined): string {
  if (!iso) return ''
  const d = new Date(iso)
  const time = d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
  if (d.toDateString() === new Date().toDateString()) return time
  return `${d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })}, ${time}`
}

/** 99.98 → "99.98%", 100 → "100%". */
export const pct = (v: number | null | undefined) =>
  v == null ? '–' : `${v >= 100 ? 100 : v.toFixed(v >= 99 ? 2 : 1).replace(/\.0+$/, '')}%`

/** "5 min ago", "3 h ago", "4 d ago". */
export function ago(iso: string | null | undefined): string {
  if (!iso) return ''
  const s = (Date.now() - new Date(iso).getTime()) / 1000
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.round(s / 60)} min ago`
  if (s < 86400) return `${Math.round(s / 3600)} h ago`
  return `${Math.round(s / 86400)} d ago`
}

/** Inter turns 3x4 into a multiplication sign; names like a23x42s must stay as typed. */
export const NAME_FONT = "[font-feature-settings:'calt'_0]"

export const shortSha = (sha: string) => sha.slice(0, 7)

const POLL = 30_000

export const envsQuery = (project: string | undefined, stopped = false) =>
  queryOptions({
    queryKey: ['environments', 'list', project ?? '', stopped],
    queryFn: () =>
      api.get<Environment[]>(`/environments${qs({ project, stopped: stopped || undefined })}`),
    staleTime: 15_000,
    refetchInterval: POLL,
  })

export const envQuery = (project: string, name: string) =>
  queryOptions({
    queryKey: ['environments', 'detail', project, name],
    queryFn: () =>
      api.get<EnvironmentDetail>(`/projects/${enc(project)}/environments/${enc(name)}`),
    staleTime: 15_000,
    refetchInterval: POLL,
  })

export const envSummaryQuery = () =>
  queryOptions({
    queryKey: ['environments', 'summary'],
    queryFn: () => api.get<EnvSummary>('/environments/summary'),
    staleTime: 30_000,
    refetchInterval: 60_000,
  })

/** Where commits of a repo run (for pull requests). */
export const envAtQuery = (repo: string, shas: string[]) =>
  queryOptions({
    queryKey: ['environments', 'at', repo, shas.join(',')],
    queryFn: () =>
      api.get<Record<string, EnvAt[]>>(`/environments/at${qs({ repo, sha: shas.join(',') })}`),
    enabled: !!repo && shas.length > 0,
    staleTime: 30_000,
  })

const useRefresh = () => {
  const qc = useQueryClient()
  return () => qc.invalidateQueries({ queryKey: ['environments'] })
}

export function useCreateEnv(project: string) {
  const refresh = useRefresh()
  return useMutation({
    mutationFn: (in_: EnvInput) =>
      api.post<EnvironmentDetail>(`/projects/${enc(project)}/environments`, in_),
    onError: toastError,
    onSuccess: refresh,
  })
}

export function useUpdateEnv(id: number) {
  const refresh = useRefresh()
  return useMutation({
    mutationFn: (in_: EnvInput) => api.patch<EnvironmentDetail>(`/environments/${id}`, in_),
    onError: toastError,
    onSuccess: refresh,
  })
}

export function useEnvAction(id: number, action: 'stop' | 'check') {
  const refresh = useRefresh()
  return useMutation({
    mutationFn: () => api.post<EnvironmentDetail>(`/environments/${id}/${action}`),
    onError: toastError,
    onSuccess: refresh,
  })
}

export function useDeleteEnv(id: number) {
  const refresh = useRefresh()
  return useMutation({
    mutationFn: () => api.del(`/environments/${id}`),
    onError: toastError,
    onSuccess: refresh,
  })
}
