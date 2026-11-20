// Hermes's analytics (GET /api/analytics/usage and /models), with every
// number that SQL may hand back as null made a number.

export interface UsageDay {
  day: string
  input_tokens: number | null
  output_tokens: number | null
  cache_read_tokens: number | null
  reasoning_tokens: number | null
  estimated_cost: number | null
  actual_cost: number | null
  sessions: number | null
  api_calls: number | null
}

export interface UsageModel {
  model: string
  input_tokens: number | null
  output_tokens: number | null
  estimated_cost: number | null
  sessions: number | null
  api_calls: number | null
}

export interface UsageTask {
  task: string
  input_tokens: number | null
  output_tokens: number | null
  estimated_cost: number | null
  api_calls: number | null
  models: string[] | null
}

export interface UsageReport {
  daily: UsageDay[] | null
  by_model: UsageModel[] | null
  by_task: UsageTask[] | null
  totals: {
    total_input: number | null
    total_output: number | null
    total_cache_read: number | null
    total_reasoning: number | null
    total_estimated_cost: number | null
    total_actual_cost: number | null
    total_sessions: number | null
    total_api_calls: number | null
  } | null
  period_days: number
  skills?: {
    summary?: {
      total_skill_loads: number | null
      total_skill_edits: number | null
      total_skill_actions: number | null
      distinct_skills_used: number | null
    } | null
    top_skills?:
      | {
          skill: string
          view_count: number | null
          manage_count: number | null
          total_count: number | null
          percentage: number | null
          last_used_at: number | null
        }[]
      | null
  } | null
  tools?: { tool: string; count: number | null; percentage: number | null }[] | null
}

export interface ModelRow {
  model: string
  provider: string
  input_tokens: number | null
  output_tokens: number | null
  cache_read_tokens: number | null
  reasoning_tokens: number | null
  estimated_cost: number | null
  actual_cost: number | null
  sessions: number | null
  api_calls: number | null
  tool_calls: number | null
  last_used_at: number | null
  avg_tokens_per_session: number | null
}

export interface ModelsReport {
  models: ModelRow[] | null
}

/** null, undefined and NaN as 0. */
export const n = (v: number | null | undefined) =>
  typeof v === 'number' && !Number.isNaN(v) ? v : 0

/** $0.42, $12, <$0.01; tiny costs keep a digit more than usd() does. */
export function money(v: number | null | undefined): string {
  const x = n(v)
  if (x <= 0) return '$0'
  if (x < 0.01) return '<$0.01'
  if (x < 1) return `$${x.toFixed(3).replace(/0$/, '')}`
  return x >= 100 ? `$${Math.round(x).toLocaleString()}` : `$${x.toFixed(2)}`
}

/** YYYY-MM-DD of a local date. */
export function ymd(d: Date): string {
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

/** The period's days, oldest first, ending today. */
export function periodDays(days: number): string[] {
  const out: string[] = []
  const d = new Date()
  d.setDate(d.getDate() - (days - 1))
  for (let i = 0; i < days; i++) {
    out.push(ymd(d))
    d.setDate(d.getDate() + 1)
  }
  return out
}

/** title_generation → Title generation */
export const taskLabel = (t: string) => {
  const s = t.replace(/[_-]+/g, ' ').trim()
  return s ? s.charAt(0).toUpperCase() + s.slice(1) : 'Other'
}

/** "just now", "29m ago", "3h ago", "2d ago", then the date: for narrow columns. */
export function ago(sec: number | null | undefined): string {
  if (!sec) return '–'
  const s = Date.now() / 1000 - sec
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.round(s / 60)}m ago`
  if (s < 86400) return `${Math.round(s / 3600)}h ago`
  if (s < 86400 * 7) return `${Math.round(s / 86400)}d ago`
  return new Date(sec * 1000).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
}

export type UsageTotals = NonNullable<UsageReport['totals']>

/** Totals summed from the days, for when Hermes sent none. */
export function totalsFromDaily(daily: UsageDay[] | null): UsageTotals {
  const sum = (f: (d: UsageDay) => number | null) => (daily ?? []).reduce((a, d) => a + n(f(d)), 0)
  return {
    total_input: sum((d) => d.input_tokens),
    total_output: sum((d) => d.output_tokens),
    total_cache_read: sum((d) => d.cache_read_tokens),
    total_reasoning: sum((d) => d.reasoning_tokens),
    total_estimated_cost: sum((d) => d.estimated_cost),
    total_actual_cost: sum((d) => d.actual_cost),
    total_sessions: sum((d) => d.sessions),
    total_api_calls: sum((d) => d.api_calls),
  }
}
