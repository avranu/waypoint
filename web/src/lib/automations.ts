// Automations that run in steps (WP-D18): the catalog the editor draws its
// fields from, runs and their steps, tests, and the admin side (system
// limits). Rules and automations themselves are typed in types.ts.

import { infiniteQueryOptions, queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { Automation, Rule, UserRef, WEvent } from './types'

// ---- the catalog ----

export type ParamType =
  | 'text'
  | 'long'
  | 'template'
  | 'number'
  | 'duration'
  | 'select'
  | 'bool'
  | 'user'
  | 'status'
  | 'query'
  | 'repo'
  | 'glob'
  | 'globs'
  | 'models'
  | 'agents'
  | 'runner'
  | 'cron'
  | 'json'
  | 'url'
  | 'secret'
  | 'field'
  | 'time'
  | 'weekday'
  | 'list'

export interface ParamOption {
  value: string
  label: string
}

export interface ParamDef {
  name: string
  label: string
  type: ParamType
  required?: boolean
  /** The editor asks for it, but rules saved without it still load and run. */
  needed?: boolean
  default?: unknown
  options?: ParamOption[]
  min?: number
  max?: number
  help?: string
  placeholder?: string
  secret?: boolean
  /** Sits behind "more options". */
  advanced?: boolean
  /** Shown only while another setting has one of these values. */
  when?: { param: string; in: string[] }
  /** A models list that may also name a coding agent on the owner's runner. */
  runners?: boolean
}

export interface OutputDef {
  name: string
  type: string
  doc?: string
}

export interface TriggerDef {
  name: string
  label: string
  group: string
  /** Finishes "When …"; {param} shows that setting in the sentence. */
  phrase: string
  doc?: string
  params?: ParamDef[]
  event: string
  /** About an issue, so "only if" and issue actions work. */
  issue?: boolean
  pr?: boolean
  admin?: boolean
  timed?: boolean
  provides?: OutputDef[]
}

export interface ActionDef {
  name: string
  label: string
  group: string
  phrase: string
  doc?: string
  params?: ParamDef[]
  /** What later steps can use: {{.Steps.<id>.<Name>}}. */
  outputs?: OutputDef[]
  /** A long step: the run is carried by a background worker. */
  long?: boolean
  /** Runs on a runner: the runners of the person who last saved the automation. */
  runner?: boolean
  /** "issue": changes the trigger's issue. "pr": works on a pull request. */
  needs?: 'issue' | 'pr' | ''
}

export interface Preset {
  id: string
  label: string
  template: string
  /** Asks which earlier step. */
  step?: boolean
}

export interface Catalog {
  triggers: TriggerDef[]
  actions: ActionDef[]
  trigger_groups: string[]
  action_groups: string[]
  presets: Preset[]
  funcs: OutputDef[]
}

export const catalogQuery = () =>
  queryOptions({
    queryKey: ['automationCatalog'],
    queryFn: () => api.get<Catalog>('/automation-catalog'),
    staleTime: Infinity,
  })

/** The sentence a trigger or action reads as: literal text and settings. */
export type PhrasePart = { lit: string } | { param: string }

export function phraseParts(phrase: string): PhrasePart[] {
  const out: PhrasePart[] = []
  let at = 0
  for (const m of phrase.matchAll(/\{(\w+)\}/g)) {
    if (m.index > at) out.push({ lit: phrase.slice(at, m.index) })
    out.push({ param: m[1] })
    at = m.index + m[0].length
  }
  if (at < phrase.length) out.push({ lit: phrase.slice(at) })
  return out
}

/** The words before a phrase's first setting: what a dropdown shows. */
export function leadText(phrase: string): string {
  const i = phrase.indexOf('{')
  const lead = (i < 0 ? phrase : phrase.slice(0, i)).replace(/\($/, '').trim()
  return lead || phrase.replace(/\{\w+\}/g, '…')
}

/** A phrase with its settings filled in by `fill`, for descriptions. */
export function fillPhrase(phrase: string, fill: (param: string) => string): string {
  return phrase
    .replace(/\{(\w+)\}/g, (_, p: string) => fill(p))
    .replace(/\s+/g, ' ')
    .trim()
}

/** Whether a setting shows, given the others' values. */
export function paramShown(p: ParamDef, vals: Record<string, unknown>): boolean {
  if (!p.when) return true
  const v = vals[p.when.param]
  return p.when.in.includes(v === undefined || v === null ? '' : String(v))
}

/** Settings that take more room than a chip: they sit in a box under the sentence. */
export const BLOCK_TYPES: ParamType[] = [
  'template',
  'long',
  'list',
  'globs',
  'json',
  'models',
  'agents',
  'url',
  'secret',
]

// ---- runs ----

export type RunStatus =
  'queued' | 'running' | 'waiting' | 'succeeded' | 'failed' | 'cancelled' | 'skipped'

export const runActive = (s: string) => s === 'queued' || s === 'running' || s === 'waiting'

export interface RunStep {
  step_id: string
  iteration: number
  position: number
  type: string
  label?: string
  /** running, waiting, succeeded, failed, skipped, cancelled; in a test also would_run */
  status: string
  attempts: number
  long?: boolean
  inputs?: Record<string, unknown>
  outputs?: Record<string, unknown>
  log?: string
  error?: string
  started_at: string | null
  finished_at: string | null
  duration_ms?: number
}

export interface AutomationRun {
  id: number
  automation_id: number
  automation?: string
  /** 0: an admin automation */
  project_id: number
  status: RunStatus
  /** event, schedule, manual, api, cli, webhook, rerun … */
  via: string
  trigger: string
  summary: string
  event?: WEvent
  inputs?: Record<string, unknown>
  actor?: UserRef
  agent?: string
  durable: boolean
  concurrency: string
  rerun_of?: number
  error?: string
  attempts: number
  created_at: string
  started_at: string | null
  finished_at: string | null
  duration_ms?: number
  /** Only on one run, not on lists. */
  steps?: RunStep[]
  /** The rule as it ran, even after the automation was edited. */
  rule?: Rule
}

export interface TestResult {
  summary: string
  event?: WEvent
  /** No recent event matched; sample data stood in. */
  sample: boolean
  note?: string
  steps: RunStep[] | null
  error?: string
}

const PAGE = 30

export const automationRunsQuery = (id: number) =>
  infiniteQueryOptions({
    queryKey: ['automationRuns', id],
    queryFn: ({ pageParam }) =>
      api.get<AutomationRun[]>(
        `/automations/${id}/runs${qs({ limit: PAGE, before: pageParam || undefined })}`,
      ),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.length >= PAGE ? last[last.length - 1].id : undefined),
  })

/** One run with its steps. While `watch` is set and the run is going, check back every few seconds. */
export const automationRunQuery = (id: number, watch = false) =>
  queryOptions({
    queryKey: ['automationRun', id],
    queryFn: () => api.get<AutomationRun>(`/automation-runs/${id}`),
    refetchInterval: (query) => {
      const s = query.state.data?.status
      return watch && s && runActive(s) ? 4000 : false
    },
  })

export const cronQuery = (expr: string, tz: string) =>
  queryOptions({
    queryKey: ['automationCron', expr, tz],
    queryFn: () => api.get<{ next: string[] }>(`/automation-cron${qs({ expr, tz, n: 5 })}`),
    retry: false,
    staleTime: 60_000,
  })

// ---- admin ----

export const adminAutomationsQuery = () =>
  queryOptions({
    queryKey: ['automations', '@admin'],
    queryFn: () => api.get<Automation[]>('/admin/automations'),
  })

export interface ResourceStatus {
  resource: 'disk' | 'database' | 'memory' | 'transcripts'
  label: string
  /** free (disk, memory) or used (database) */
  bytes: number
  total?: number
  limit_mb: number
  low: boolean
  /** transcripts: pieces stored, the size cap in MB (0: none) and uploads refused for it */
  count?: number
  cap_mb?: number
  refused?: number
}

export interface ResourceLimits {
  disk_free_mb: number
  database_mb: number
  memory_free_mb: number
  transcripts_mb: number
}

export const adminSystemQuery = () =>
  queryOptions({
    queryKey: ['admin', 'system'],
    queryFn: () =>
      api.get<{ resources: ResourceStatus[]; limits: ResourceLimits }>('/admin/system'),
    refetchInterval: 30_000,
  })

export function bytesText(n: number): string {
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024
    i++
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`
}
