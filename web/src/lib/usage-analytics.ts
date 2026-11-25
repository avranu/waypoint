import { infiniteQueryOptions, queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import { viz } from '@/components/charts'
import type { IssueRef } from './types'

export interface UsageTotals {
  messages: number
  /** Uncached input tokens. */
  input: number
  output: number
  /** 5 minute and 1 hour writes together. */
  cache_write: number
  cache_write_1h: number
  cache_read: number
  cost_usd: number
  /** Cache reads / all input; null without input. */
  hit_rate: number | null
  unpriced_tokens?: number
}

export interface UsageDay extends UsageTotals {
  date: string
  model: string
}

export interface UsageBreakdown extends UsageTotals {
  key: string
  label: string
  sub?: string
  project?: string
  last_at?: string
  compactions?: number
  /** sessions: the machine it ran on, and the agent session Waypoint holds for it */
  machine?: string
  session_id?: number
}

export interface UsageTool {
  group: 'builtin' | 'mcp' | 'skill' | 'command' | 'subagent' | 'bash'
  name: string
  calls: number
  errors: number
  refusals: number
  prompts: number
  bytes: number
  items?: UsageTool[]
}

export interface UsageUnused {
  kind: 'mcp' | 'skill' | 'command' | 'agent'
  name: string
  scope?: string
  machine: string
}

export interface UsageRefusalRow {
  id: number
  at: string
  tool: string
  detail: string
  reason: string
  source: string
  excerpt: string
  session: string
  repo: string
  machine: string
}

export interface UsageRefusals {
  total: number
  reasons: { reason: string; source: string; count: number }[]
  daily: { date: string; count: number }[]
  commands: { tool: string; detail: string; count: number; reasons: string[]; rule?: string }[]
  recent: UsageRefusalRow[]
}

export interface QuotaWindow {
  span: 'five_hour' | 'seven_day'
  label: string
  seconds: number
  percent: number
  resets_at: string
  sampled_at: string
  machine: string
  status: 'on_track' | 'will_run_out' | 'at_limit' | 'too_early' | 'reset'
  projected: number
  runs_out_at: string | null
  even: number
  /** When the even pace began: the window's start, or the first reading after the last reset. */
  paced_from: string
  /** Resets inside the window, oldest first. */
  resets: string[]
  history: { at: string; percent: number; resets_at: string }[]
}

export interface UsageAdvice {
  id: string
  kind: string
  level: 'warn' | 'info'
  title: string
  evidence: string[]
  action: { kind: 'rule' | 'note'; text: string; snippet?: string }
}

export interface UsageMachine {
  name: string
  last_sync_at: string | null
  last_status_at: string | null
  wp_version: string
  parsed: number
  skipped: number
  unknown: Record<string, number>
  /** the transcript archive this machine reported; absent: it has not */
  transcripts?: UsageTranscripts
}

/** null: the machine has not said (never 0 or false) */
export interface UsageTranscripts {
  pending_bytes: number
  pending_files: number
  dropped: Record<string, number>
  last_piece_at: string | null
  reported_at: string | null
  off: boolean | null
  pass_ms: number | null
  pass_max_ms: number | null
  stop: string | null
  cut: number | null
}

export interface UsageOverview {
  from: string
  to: string
  days: number
  who: string
  has_data: boolean
  totals: UsageTotals
  sessions: number
  daily: UsageDay[]
  models: UsageBreakdown[]
  repos: UsageBreakdown[]
  machines: UsageBreakdown[]
  kinds: UsageBreakdown[]
  top_sessions: UsageBreakdown[]
  tools: UsageTool[]
  bash: UsageTool[]
  unused: UsageUnused[]
  no_inventory: boolean
  refusals: UsageRefusals
  quota: { windows: QuotaWindow[] } | null
  suggestions: UsageAdvice[]
  reporting: UsageMachine[]
  people?: { username: string; display_name: string }[]
  prices_edit: boolean
  unpriced_models: string[]
  truncated?: boolean
}

export interface UsageSummary {
  has_data: boolean
  days: number
  totals: UsageTotals
  quota: { windows: QuotaWindow[] } | null
  last_sync: string | null
}

export interface UsageFilters {
  agent: string
  project: string
  days: number
  machine?: string
  session?: string
  who?: string
}

export const usageParams = (f: UsageFilters) => ({
  agent: f.agent || undefined,
  project: f.project || undefined,
  days: f.days,
  machine: f.machine || undefined,
  session: f.session || undefined,
  user: f.who || undefined,
})

export const usageOverviewQuery = (f: UsageFilters) =>
  queryOptions({
    queryKey: [
      'usage',
      'overview',
      f.agent,
      f.project,
      f.days,
      f.machine ?? '',
      f.session ?? '',
      f.who ?? '',
    ],
    queryFn: () => api.get<UsageOverview>(`/usage/overview${qs(usageParams(f))}`),
    staleTime: 30_000,
    placeholderData: (prev) => prev,
  })

export const usageSummaryQuery = () =>
  queryOptions({
    queryKey: ['usage', 'summary'],
    queryFn: () => api.get<UsageSummary>('/usage/summary'),
    staleTime: 60_000,
  })

export const usageRefusalsQuery = (f: UsageFilters, before: number) =>
  queryOptions({
    queryKey: [
      'usage',
      'refusals',
      f.agent,
      f.project,
      f.days,
      f.machine ?? '',
      f.session ?? '',
      f.who ?? '',
      before,
    ],
    queryFn: () =>
      api.get<UsageRefusalRow[]>(`/usage/refusals${qs({ ...usageParams(f), before, limit: 25 })}`),
    staleTime: 30_000,
  })

export type RiskyLevel = 'high' | 'medium'
export type RiskyOutcome = 'ran' | 'error' | 'refused' | 'unknown'

export interface UsageRiskyRow {
  id: number
  at: string
  kind: string
  level: RiskyLevel
  label: string
  /** The command's start, secrets hidden. */
  command: string
  /** The risky part, when the start does not show it. */
  segment?: string
  outcome: RiskyOutcome
  /** Who refused it: classifier, hook, rule, user. */
  source?: string
  session: string
  repo: string
  machine: string
  model: string
  /** The task the session held when it ran. */
  issue?: IssueRef
}

export interface UsageRiskyPage {
  rows: UsageRiskyRow[]
  /** The period's counts, before the level, kind and outcome filters (first page only). */
  counts?: { high: number; medium: number; refused: number; kinds: Record<string, number> }
}

export interface RiskyFilters {
  level: '' | RiskyLevel
  kind: string
  outcome: '' | 'ran' | 'refused'
}

export const usageRiskyQuery = (f: UsageFilters, r: RiskyFilters) =>
  infiniteQueryOptions({
    queryKey: [
      'usage',
      'risky',
      f.agent,
      f.project,
      f.days,
      f.machine ?? '',
      f.session ?? '',
      f.who ?? '',
      r.level,
      r.kind,
      r.outcome,
    ],
    queryFn: ({ pageParam }) =>
      api.get<UsageRiskyPage>(
        `/usage/risky${qs({
          ...usageParams(f),
          level: r.level || undefined,
          kind: r.kind || undefined,
          outcome: r.outcome || undefined,
          before: pageParam || undefined,
          limit: 50,
        })}`,
      ),
    initialPageParam: 0,
    getNextPageParam: (last) =>
      last.rows.length < 50 ? undefined : last.rows[last.rows.length - 1].id,
    staleTime: 30_000,
  })

export const usageBreakdownQuery = (f: UsageFilters, by: string, limit: number, enabled: boolean) =>
  queryOptions({
    queryKey: [
      'usage',
      'breakdown',
      by,
      limit,
      f.agent,
      f.project,
      f.days,
      f.machine ?? '',
      f.session ?? '',
      f.who ?? '',
    ],
    queryFn: () =>
      api.get<UsageBreakdown[]>(`/usage/breakdown${qs({ ...usageParams(f), by, limit })}`),
    staleTime: 30_000,
    enabled,
  })

export type TrendSum =
  | 'cost'
  | 'tokens'
  | 'cache_read'
  | 'messages'
  | 'sessions'
  | 'tool_calls'
  | 'tool_errors'
  | 'subagents'
  | 'tasks'
export type TrendAvg = 'avg_context' | 'over_200k' | 'hit_rate' | 'cost_per_session'

export interface UsageTrendRepo {
  /** '' = no repository; '*' = every other repo. */
  repo: string
  label: string
  project?: string
  cost_usd: number
  series: Partial<Record<TrendSum, number[]>>
}

export interface UsageMark {
  /** The reader's day. */
  date: string
  at: string
  kind: 'instructions' | 'cli'
  repo?: string
  title: string
  url?: string
  author?: string
  /** Commits (or versions) that day. */
  count: number
  paths?: string[]
}

export interface UsageTrendPR {
  repo: string
  number: number
  title: string
  url: string
  merged_at: string
}

export interface UsageTrendTask extends UsageTotals {
  key: string
  title: string
  done: boolean
  resolved_at?: string
  points?: number
  sessions: number
  subagents: number
  tool_calls: number
  tool_errors: number
  active_hours: number
  compactions: number
  avg_context: number | null
  first_at: string
  last_at: string
  prs: number
  lines: number
  files: number
  commits: number
}

export interface UsageFactor {
  key: string
  label: string
  /** Spearman's rank correlation with cost; null: too few tasks, or no spread. */
  rho: number | null
  n: number
  enough: boolean
}

export interface UsageTrends {
  from: string
  to: string
  days: number
  who: string
  has_data: boolean
  dates: string[]
  sums: Record<TrendSum, number[]>
  avgs: Record<TrendAvg, (number | null)[]>
  repos: UsageTrendRepo[]
  marks: UsageMark[]
  prs: UsageTrendPR[]
  tasks: UsageTrendTask[]
  task_rows: number
  no_task: UsageTotals
  factors: UsageFactor[]
}

export interface UsageCliPeriod {
  sessions: number
  main_sessions: number
  bash: number
  wp: number
  help: number
  help_main: number
  usage_errors: number
  curl_forge: number
  sleep: number
  sleep_ci: number
  gh_tea: number
  bash_per_session: number | null
  wp_per_session: number | null
  help_rate: number | null
  error_rate: number | null
  help_per_main: number | null
  curl_per_session: number | null
  sleep_per_session: number | null
}

export interface UsageCliCount {
  name: string
  n: number
}

export interface UsageCliFollow extends UsageCliCount {
  share: number | null
}

export interface UsageCliCommand {
  head: string
  calls: number
  help: number
  errors: number
  flags: UsageCliCount[]
  follows: UsageCliFollow[]
}

export interface UsageCliRepo {
  repo: string
  sessions: number
  wp: number
  help: number
  errors: number
  wp_per_session: number | null
  help_rate: number | null
  error_rate: number | null
}

export interface UsageCli {
  days: number
  who: string
  repo?: string
  has_data: boolean
  total: UsageCliPeriod
  weeks: (UsageCliPeriod & { week: string })[]
  commands: UsageCliCommand[]
  unknown_flags: { head: string; flag: string; n: number }[]
  unknown_commands: UsageCliCount[]
  error_classes: UsageCliCount[]
  repos: UsageCliRepo[]
}

export const usageCliQuery = (f: UsageFilters & { repo?: string }) =>
  queryOptions({
    queryKey: ['usage', 'cli', f.days, f.who ?? '', f.repo ?? ''],
    queryFn: () =>
      api.get<UsageCli>(
        `/usage/cli${qs({ days: f.days, user: f.who || undefined, repo: f.repo || undefined })}`,
      ),
    staleTime: 30_000,
    placeholderData: (prev) => prev,
  })

export const usageTrendsQuery = (f: UsageFilters) =>
  queryOptions({
    queryKey: [
      'usage',
      'trends',
      f.agent,
      f.project,
      f.days,
      f.machine ?? '',
      f.session ?? '',
      f.who ?? '',
    ],
    queryFn: () => api.get<UsageTrends>(`/usage/trends${qs(usageParams(f))}`),
    staleTime: 30_000,
    placeholderData: (prev) => prev,
  })

export interface CompareSide {
  n: number
  median: number
  mean: number
  total: number
}

export interface CompareStat {
  key: string
  label: string
  unit: 'usd' | 'tokens' | 'count' | 'ratio' | 'hours'
  before: CompareSide
  after: CompareSide
  /** after median / before median − 1; null when before is 0. */
  change: number | null
  /** Mann–Whitney: how likely a difference this size is by chance. */
  p: number | null
}

export interface UsageCompare {
  at: string
  window: number
  repo?: string
  who: string
  before: { from: string; to: string; days: number }
  after: { from: string; to: string; days: number }
  sessions: CompareStat[]
  tasks: CompareStat[]
  daily: CompareStat[]
  /** Fewer samples than this on a side: too few to say. */
  min_n: number
}

export const usageCompareQuery = (f: UsageFilters, at: string, window: number, repo: string) =>
  queryOptions({
    queryKey: [
      'usage',
      'compare',
      at,
      window,
      repo,
      f.agent,
      f.project,
      f.machine ?? '',
      f.session ?? '',
      f.who ?? '',
    ],
    queryFn: () =>
      api.get<UsageCompare>(
        `/usage/compare${qs({ ...usageParams(f), days: undefined, at, window, repo: repo || undefined })}`,
      ),
    staleTime: 30_000,
    enabled: !!at,
    placeholderData: (prev) => prev,
  })

export interface ModelPrice {
  model: string
  input: number
  output: number
  cache_write_5m: number
  cache_write_1h: number
  cache_read: number
  updated_at: string | null
}

export const modelPricesQuery = () =>
  queryOptions({
    queryKey: ['usage', 'prices'],
    queryFn: () => api.get<ModelPrice[]>('/usage/prices'),
  })

/** Opus / Sonnet / Haiku / Other: the series of the token charts. */
export function modelFamily(model: string): 'Opus' | 'Sonnet' | 'Haiku' | 'Other' {
  const m = model.toLowerCase()
  if (m.includes('opus')) return 'Opus'
  if (m.includes('sonnet')) return 'Sonnet'
  if (m.includes('haiku')) return 'Haiku'
  return 'Other'
}

/** Colour follows the family, never its rank, so a filter doesn't repaint it. */
export const FAMILY_COLOR: Record<string, string> = {
  Opus: viz.blue,
  Sonnet: viz.orange,
  Haiku: viz.aqua,
  Other: viz.yellow,
}
export const FAMILIES = ['Opus', 'Sonnet', 'Haiku', 'Other']

/** 12,400 → 12.4k; 1.2M; 3.4B. */
export function tok(n: number) {
  const a = Math.abs(n)
  if (a >= 1e9) return `${(n / 1e9).toFixed(1)}B`
  if (a >= 1e6) return `${(n / 1e6).toFixed(a >= 1e7 ? 0 : 1)}M`
  if (a >= 1e4) return `${Math.round(n / 1e3)}k`
  if (a >= 1e3) return `${(n / 1e3).toFixed(1)}k`
  return String(Math.round(n))
}

export const pct = (v: number | null | undefined, digits = 0) =>
  v === null || v === undefined ? '–' : `${(v * 100).toFixed(digits)}%`

export function bytesText(n: number) {
  if (n >= 1 << 30) return `${(n / (1 << 30)).toFixed(1)} GB`
  if (n >= 1 << 20) return `${(n / (1 << 20)).toFixed(1)} MB`
  if (n >= 1 << 10) return `${Math.round(n / (1 << 10))} KB`
  return `${n} B`
}

/** Fresh tokens: what was newly read or written, leaving out cache re-reads. */
export const fresh = (t: UsageTotals) => t.input + t.output + t.cache_write

/** "Thu 15:00" in the viewer's time zone. */
export function weekdayTime(iso: string) {
  return new Date(iso).toLocaleString(undefined, {
    weekday: 'short',
    hour: '2-digit',
    minute: '2-digit',
  })
}

/** The pace line: how the window is going, in words. */
export function paceText(w: QuotaWindow): { text: string; tone: 'ok' | 'warn' | 'bad' | 'muted' } {
  switch (w.status) {
    case 'at_limit':
      return { text: `At the limit; resets ${weekdayTime(w.resets_at)}`, tone: 'bad' }
    case 'will_run_out':
      return {
        text: `Will run out ${weekdayTime(w.runs_out_at ?? w.resets_at)} at the recent rate (${Math.round(w.projected)}% by the reset)`,
        tone: 'bad',
      }
    case 'on_track':
      return { text: `On track: about ${Math.round(w.projected)}% by the reset`, tone: 'ok' }
    case 'too_early':
      return { text: 'Too early in the window to tell', tone: 'muted' }
    default:
      return { text: 'The window has reset since the last report', tone: 'muted' }
  }
}

export interface LedgerStats {
  calls: number
  /** Input + cache write + cache read per call, averaged. */
  avg_context: number
  over_200k: number
  over_300k: number
  share_200k: number | null
  share_300k: number | null
  /** Of all cache reads, those made by calls over 300k. */
  read_share_300k: number | null
  cold_starts: number
  cold_share: number | null
  cold_tokens: number
  cold_extra_est_usd: number
}

export interface LedgerDay {
  date: string
  calls: number
  avg_context: number
  share_300k: number | null
  cold_starts: number
}

export interface LedgerRow extends LedgerStats {
  key: string
  label: string
  sub?: string
  task?: string
  machine?: string
  cost_usd: number
  cache_read: number
  sessions?: number
  resolved_at?: string
}

export interface LedgerSpawn {
  type: string
  forks: number
  fresh: number
  fork_context: number
  fresh_context: number
}

export interface LedgerYield {
  done: number
  measured: number
  totals: UsageTotals
  models: (UsageTotals & { model: string; items: number })[]
}

export interface LedgerPrepGroup extends LedgerYield {
  group: 'shown' | 'held_out' | 'none'
  tool_calls: number
}

export interface UsageLedger {
  from: string
  to: string
  days: number
  who: string
  overall: LedgerStats
  daily: LedgerDay[]
  spawns: LedgerSpawn[]
  sessions: LedgerRow[]
  machines: LedgerRow[]
  tasks: LedgerRow[]
  closed_tasks: LedgerYield
  merged_prs: LedgerYield
  /** Closed tasks split by task prep (WP-212): shown to the agent, held out, or none. */
  prep?: LedgerPrepGroup[]
  /** Messages from before the ledger that `wp usage sync` hasn't re-read yet. */
  unmeasured_messages: number
  truncated?: boolean
}

/** What X4 measured by hand on 2026-10-14 (Waypoint workspace, 7 days). */
export const LEDGER_BASELINE = {
  avgContext: 279_000,
  share300k: 0.43,
  readShare300k: 0.7,
} as const
