// Audits (WP-348, WP-ADR20): a model, the one the gateway picks by default
// (WP-871), reads a checkout from the asker's runner (a repository, branch,
// commit, range, what landed on a branch lately (WP-548) or a pull request)
// and reports findings; it never changes anything. Not the audit log
// (lib/audit.ts), which records what people decided.

import { queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { UserRef } from './types'

export type AuditTarget = 'recent' | 'repo' | 'branch' | 'commit' | 'range' | 'pr'
export type AuditStatus = 'queued' | 'running' | 'done' | 'failed' | 'limited' | 'cancelled'
export type FindingSeverity = 'critical' | 'high' | 'medium' | 'low'
export type FindingState = 'open' | 'confirmed' | 'fixed' | 'rejected'

export interface AuditCounts {
  total: number
  open: number
  critical: number
  high: number
  medium: number
  low: number
}

export interface AuditFinding {
  /** "12.3": audit 12, finding 3. */
  id: string
  ordinal: number
  title: string
  path?: string
  line?: number
  severity: FindingSeverity
  problem: string
  confirm?: string
  state: FindingState
  note?: string
  issue?: string
  by?: string
  at?: string
  /** The line on the forge. */
  url?: string
}

export interface AuditAttempt {
  model: string
  job?: number
  round: number
  /** answered, unreadable, busy, spent, timeout, model, unavailable, error, cancelled, stopped */
  outcome: string
  error?: string
  at: number
}

export interface CodeAudit {
  id: number
  project: string
  project_name: string
  repo: string
  target: AuditTarget
  ref?: string
  base?: string
  pr_id?: number
  pr_number?: number
  pr_url?: string
  title: string
  instructions: string
  /** A recent target's period as asked (7d, 2026-10-28), and its start. */
  since?: string
  since_at?: string
  /** Criteria (WP-548): focus areas' keys, paths read or left out. */
  focus: string[] | null
  paths: string[] | null
  exclude: string[] | null
  /** The lowest severity reported; none: all. */
  min_severity?: FindingSeverity
  /** The most findings asked for; none: the default. */
  max_findings?: number
  /** What the runner read: the change's ends and its commits. */
  from_sha?: string
  to_sha?: string
  commit_count?: number
  models: string[]
  post_comment: boolean
  fix: boolean
  status: AuditStatus
  slot: number
  job_id?: number
  job_state?: string
  attempts: AuditAttempt[]
  retries: number
  retry_at?: string
  model?: string
  summary?: string
  answer?: string
  error?: string
  comment_url?: string
  fix_issue?: string
  requested_by?: UserRef
  agent?: string
  via?: string
  automation?: string
  created_at: string
  started_at?: string
  finished_at?: string
  counts: AuditCounts
  findings?: AuditFinding[]
  url: string
  can_edit: boolean
}

export interface AuditDefaults {
  instructions: string
  models: string[]
  targets: AuditTarget[]
  severities: FindingSeverity[]
  states: FindingState[]
  clis: string[]
  focus: AuditFocus[]
  periods: { value: string; label: string }[]
  /** The default period for recent changes: 7d. */
  since: string
  /** Findings by default, and the most anyone can ask for. */
  max_findings: number
  findings_cap: number
  paths_cap: number
  /** An LLM gateway is set up, so an audit can name it. */
  gateway: boolean
}

export interface AuditFocus {
  key: string
  label: string
  /** What the model is told. */
  text: string
}

export interface AuditInput {
  project?: string
  repo?: string
  target: AuditTarget
  ref?: string
  base?: string
  /** A pull request's number in the repository, or pr_id: its id in Waypoint. */
  pr?: number
  pr_id?: number
  /** Recent changes: 24h, 7d, 2w, 1mo or a date (2026-10-28). */
  since?: string
  focus?: string[]
  paths?: string[]
  exclude?: string[]
  min_severity?: FindingSeverity | ''
  max_findings?: number
  instructions?: string
  models?: string[]
  comment?: boolean
  fix?: boolean
}

export const ACTIVE: AuditStatus[] = ['queued', 'running']

export interface AuditListFilter {
  project?: string
  pr_id?: number
  status?: AuditStatus | 'active' | ''
  limit?: number
}

export const auditsQuery = (f: AuditListFilter = {}) =>
  queryOptions({
    queryKey: ['audits', f],
    queryFn: () =>
      api.get<CodeAudit[]>(
        `/audits${qs({ project: f.project, pr_id: f.pr_id, status: f.status, limit: f.limit })}`,
      ),
    staleTime: 15_000,
    placeholderData: (prev) => prev,
  })

export const codeAuditQuery = (id: number) =>
  queryOptions({
    queryKey: ['audit', id],
    queryFn: () => api.get<CodeAudit>(`/audits/${id}`),
    staleTime: 15_000,
  })

export const auditDefaultsQuery = () =>
  queryOptions({
    queryKey: ['audits', 'defaults'],
    queryFn: () => api.get<AuditDefaults>('/audits/defaults'),
    staleTime: Infinity,
  })

export const requestAudit = (in_: AuditInput) =>
  api.post<CodeAudit>('/audits', { ...in_, via: 'web' })
export const rerunAudit = (id: number) => api.post<CodeAudit>(`/audits/${id}/rerun`, { via: 'web' })
export const cancelAudit = (id: number) => api.post<CodeAudit>(`/audits/${id}/cancel`)
export const fixAudit = (id: number) => api.post<CodeAudit>(`/audits/${id}/fix`)
export const markFindings = (ids: string[], state: FindingState, note = '') =>
  api.post<AuditFinding[]>('/audits/findings', { ids, state, note })

// ---- words ----

export const STATUS: Record<
  AuditStatus,
  { label: string; color: string; tone: 'ok' | 'warn' | 'danger' | 'muted' | 'accent' }
> = {
  queued: { label: 'Queued', color: 'var(--wp-subtle)', tone: 'muted' },
  running: { label: 'Running', color: 'var(--wp-accent)', tone: 'accent' },
  done: { label: 'Done', color: 'var(--wp-ok)', tone: 'ok' },
  failed: { label: 'Failed', color: 'var(--wp-danger)', tone: 'danger' },
  limited: { label: 'Out of quota', color: 'var(--wp-warn)', tone: 'warn' },
  cancelled: { label: 'Cancelled', color: 'var(--wp-subtle)', tone: 'muted' },
}

export const SEVERITY: Record<FindingSeverity, { label: string; color: string }> = {
  critical: { label: 'Critical', color: 'var(--wp-danger)' },
  high: { label: 'High', color: 'var(--wp-danger)' },
  medium: { label: 'Medium', color: 'var(--wp-warn)' },
  low: { label: 'Low', color: 'var(--wp-subtle)' },
}

export const FINDING_STATE: Record<FindingState, { label: string; color: string }> = {
  open: { label: 'Open', color: 'var(--wp-accent)' },
  confirmed: { label: 'Confirmed', color: 'var(--wp-warn)' },
  fixed: { label: 'Fixed', color: 'var(--wp-ok)' },
  rejected: { label: 'Rejected', color: 'var(--wp-subtle)' },
}

export const TARGETS: { value: AuditTarget; label: string; hint: string }[] = [
  { value: 'recent', label: 'Recent', hint: 'What landed on a branch over a period' },
  { value: 'pr', label: 'Pull request', hint: 'What a pull request changes, at its head now' },
  { value: 'branch', label: 'Branch', hint: 'A branch against its base' },
  { value: 'commit', label: 'Commit', hint: 'One commit against its parent' },
  { value: 'range', label: 'Range', hint: 'The changes between two refs' },
  { value: 'repo', label: 'Repository', hint: 'The whole tree at a ref' },
]

const OUTCOME: Record<string, string> = {
  answered: 'answered',
  unreadable: 'answered in a way that couldn’t be read',
  busy: 'was busy',
  spent: 'was out of quota',
  timeout: 'ran out of time',
  model: 'refused or failed',
  unavailable: 'isn’t on the runner',
  error: 'failed',
  cancelled: 'was cancelled',
  stopped: 'stopped with its runner',
  nothing: 'had nothing to read',
}
export const outcomeText = (o: string) => OUTCOME[o] ?? o

/** "2 high, 1 low" */
export function severityText(c: AuditCounts): string {
  return (['critical', 'high', 'medium', 'low'] as const)
    .filter((k) => c[k] > 0)
    .map((k) => `${c[k]} ${k}`)
    .join(', ')
}

/** "cursor/grok-4.7-high" → { cli: 'cursor', model: 'grok-4.7-high' } */
export function splitModel(m: string): { cli: string; model?: string } {
  const i = m.indexOf('/')
  return i < 0 ? { cli: m } : { cli: m.slice(0, i), model: m.slice(i + 1) }
}
export const joinModel = (m: { cli: string; model?: string }) =>
  m.model?.trim() ? `${m.cli}/${m.model.trim()}` : m.cli

/** "grok-4.7-high" for "cursor/grok-4.7-high"; a bare CLI stays as it is.
 * The gateway before it has picked a model ("gateway/audit") is "Gateway". */
export function modelShort(m: string): string {
  const { cli, model } = splitModel(m)
  if (cli === 'gateway' && (!model || model === 'audit')) return 'Gateway'
  if (cli === 'gateway' && model === 'free') return 'Gateway free'
  return model || m
}

/** "Medium or worse" for a floor; everything when there's none. */
export const MIN_SEVERITY: { value: FindingSeverity | ''; label: string }[] = [
  { value: '', label: 'Everything' },
  { value: 'medium', label: 'Medium or worse' },
  { value: 'high', label: 'High or worse' },
  { value: 'critical', label: 'Critical only' },
]

/** One per line or comma: "internal/core, web/src" → ['internal/core', 'web/src'] */
export const splitPaths = (v: string) =>
  v
    .split(/[\n,]/)
    .map((p) => p.trim())
    .filter(Boolean)

/** An audit asked for anything more than what to read and the sentence. */
export const hasCriteria = (a: CodeAudit) =>
  !!(a.focus?.length || a.paths?.length || a.exclude?.length || a.min_severity || a.max_findings)

/** "the last 7 days", "since 28 Oct 2026": a recent audit's period. */
export function sinceText(spec?: string, at?: string): string {
  const m = spec?.match(/^(\d+)(h|d|w|mo)$/)
  if (m) {
    const n = Number(m[1])
    const unit = { h: 'hour', d: 'day', w: 'week', mo: 'month' }[m[2] as 'h' | 'd' | 'w' | 'mo']
    return n === 1 ? `the last ${unit}` : `the last ${n} ${unit}s`
  }
  const d = at ? new Date(at) : spec ? new Date(spec) : null
  if (!d || Number.isNaN(d.getTime())) return spec ?? ''
  return `since ${d.toLocaleDateString(undefined, { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' })}`
}
