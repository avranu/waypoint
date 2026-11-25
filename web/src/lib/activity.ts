import { queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'

export interface ActivityTotals {
  tasks_done: number
  runs: number
  seconds: number
  input_tokens: number
  output_tokens: number
  cost_usd: number
  prs_opened: number
  prs_merged: number
  issues_created: number
  comments: number
  bot_reviews: number
}

/** One agent identity's share of the period. */
export interface AgentActivity {
  identity: string
  family: string
  models: string[]
  runs: number
  done: number
  released: number
  /** Its claim lapsed or its clock ran out. */
  expired: number
  seconds: number
  input_tokens: number
  output_tokens: number
  cost_usd: number
  prs_merged: number
  issues_created: number
  comments: number
  last_active: string | null
}

export interface ActivityDay {
  /** YYYY-MM-DD in your time zone. */
  date: string
  seconds: number
  tokens: number
  done: number
}

/** What one model did for the review bots. */
export interface BotActivity {
  model: string
  reviews: number
  findings: number
  /** Calls that didn't answer, so the next model was tried. */
  failed: number
  seconds: number
}

export interface ActivityPR {
  url: string
  repo: string
  number: number
  title: string
  state: string
}

export interface ActivityRun {
  id: number
  agent: string
  model?: string
  step?: string
  issue: {
    key: string
    title: string
    project_key: string
    status: string
    status_category: string
  }
  started_at: string
  ended_at: string | null
  seconds: number
  outcome: '' | 'done' | 'diagnosed' | 'released' | 'expired' | 'paused' | 'timeout'
  input_tokens: number
  output_tokens: number
  cost_usd: number
  prs: ActivityPR[]
}

export interface Activity {
  from: string
  to: string
  totals: ActivityTotals
  agents: AgentActivity[]
  daily: ActivityDay[]
  bots: BotActivity[]
  runs: ActivityRun[]
}

export const activityQuery = (f: { agent: string; project: string; days: number }) =>
  queryOptions({
    queryKey: ['agents', 'activity', f.agent, f.project, f.days],
    queryFn: () =>
      api.get<Activity>(
        `/agents/activity${qs({ agent: f.agent || undefined, project: f.project || undefined, days: f.days })}`,
      ),
    staleTime: 30_000,
    placeholderData: (prev) => prev,
  })

/** 12,400 → 12k; 1.2M. */
export function tokens(n: number) {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (n >= 10_000) return `${Math.round(n / 1000)}k`
  if (n >= 1000) return `${(n / 1000).toFixed(1)}k`
  return String(n)
}

/** $0.42, $12, <$0.01. */
export function usd(n: number) {
  if (n <= 0) return '$0'
  if (n < 0.01) return '<$0.01'
  return n >= 100 ? `$${Math.round(n)}` : `$${n.toFixed(2)}`
}
