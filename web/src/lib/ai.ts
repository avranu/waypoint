import { queryOptions, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, enc } from './api'
import type { ChainModel, UserRef } from './types'

export type ProviderKind = 'opencode' | 'openai' | 'ollama'

export interface AIProvider {
  id: number
  name: string
  kind: ProviderKind
  base_url: string
  has_key: boolean
  created_at: string
}

export interface AIModel {
  id: string
  name: string
  free: boolean
}

export interface AITask {
  task: 'estimate' | 'title' | 'prep' | 'breakdown' | 'relate' | 'review'
  label: string
  description: string
  provider_id: number | null
  model: string
  /** tried in order when the model fails */
  fallbacks: ChainModel[]
}

export interface Estimate {
  points: number
  confidence: 'low' | 'medium' | 'high'
  reason: string
  model: string
}

export interface Breakdown {
  subtasks: { title: string; points?: number }[]
  model: string
}

/** One text in a safety drill: a known-bad one the screen must hold, or a plain one it must clear. */
export interface DrillCase {
  name: string
  label: string
  want: 'hold' | 'clear'
  /** absent when no model answered */
  verdict?: 'hold' | 'clear'
  flags?: string[]
  reason?: string
  model?: string
  error?: string
  pass: boolean
}

/** The weekly safety drill (WP-278): the screen's models against texts with known answers. */
export interface SafetyDrills {
  configured: boolean
  running: boolean
  last?: {
    id: number
    started_by?: UserRef
    started_at: string
    finished_at: string
    passed: number
    failed: number
    errors: number
    cases: DrillCase[]
  }
  models: { model: string; passed: number; total: number; last_at: string }[]
  next_at?: string
  cases: number
}

/** One review arm (WP-365): a context strategy with the verifier on or off. */
export interface ReviewArm {
  key: string
  strategy: string
  verify: boolean
  label: string
  state: 'exploring' | 'in_use' | 'benched' | 'pinned' | 'unavailable'
  why: string
  pin?: 'on' | 'off'
  /** weighted by age and prompt version */
  useful: number
  false_positives: number
  /** the posterior mean, and its 90% upper bound */
  precision: number
  upper: number
  /** tokens relative to the cheapest arm; 1 until measured */
  cost: number
  tokens: number
  runs: number
  last_run?: string
  /** the models its runs were answered with, and how their findings fared */
  models: { model: string; useful: number; false_positives: number }[]
}

export interface ReviewArms {
  arms: ReviewArm[]
  /** per model, in runs with more than one reviewer */
  recall: { model: string; runs: number; alone: number; alone_held: number; missed_held: number }[]
  budget: { chars: number; model?: string; context_length?: number }
  explore_under: number
  bench_bound: number
  benched_odds: number
  /** An LLM gateway is set up and picks each review's model. */
  gateway: boolean
}

/** useful / false / unjudged; precision with its 90% interval once judged */
export interface ReviewJudged {
  useful: number
  false: number
  unjudged: number
  precision: number | null
  low: number | null
  high: number | null
}

export interface ReviewerStat extends ReviewJudged {
  model?: string
  providers?: string[]
  strategy?: string
  prompt_version?: string
  findings: number
  evidence_dropped: number
  withheld: number
  tokens: number
  tokens_per_useful: number
  runs: number
}

export interface VerifierStat {
  model: string
  providers?: string[]
  kept: ReviewJudged
  dropped: number
  posted: ReviewJudged
  drop_accuracy: number | null
  drop_low: number | null
  drop_high: number | null
  tokens: number
  runs: number
}

export interface ReviewArmClass extends ReviewJudged {
  findings: number
  runs: number
  tokens: number
  tokens_per_useful: number
}

/** GET /reviews/stats (WP-371): how review findings held up */
export interface ReviewStats {
  project?: string
  days: number
  since: string
  totals: ReviewerStat
  reviewers: ReviewerStat[]
  verifiers: VerifierStat[]
  verify: { verified: ReviewArmClass; skipped: ReviewArmClass }
}

export const aiq = {
  tasks: () =>
    queryOptions({
      queryKey: ['ai', 'tasks'],
      queryFn: () => api.get<AITask[]>('/ai/tasks'),
      staleTime: 5 * 60_000,
    }),
  providers: () =>
    queryOptions({
      queryKey: ['ai', 'providers'],
      queryFn: () => api.get<AIProvider[]>('/ai/providers'),
    }),
  drill: () =>
    queryOptions({
      queryKey: ['ai', 'safety-drill'],
      queryFn: () => api.get<SafetyDrills>('/ai/safety-drill'),
      refetchInterval: (query) => (query.state.data?.running ? 5_000 : false),
    }),
  reviewArms: () =>
    queryOptions({
      queryKey: ['ai', 'review-arms'],
      queryFn: () => api.get<ReviewArms>('/ai/review-arms'),
    }),
  reviewStats: (days: number) =>
    queryOptions({
      queryKey: ['ai', 'review-stats', days],
      queryFn: () => api.get<ReviewStats>(`/reviews/stats?days=${days}`),
    }),
  models: (id: number) =>
    queryOptions({
      queryKey: ['ai', 'models', id],
      queryFn: () => api.get<AIModel[]>(`/ai/providers/${id}/models`),
      staleTime: 10 * 60_000,
      retry: false,
    }),
}

/** Whether a model is set up for a task, so the UI only offers what works. */
export function useAITask(task: AITask['task']) {
  const tasks = useQuery(aiq.tasks()).data
  return !!tasks?.find((t) => t.task === task && t.provider_id)
}

export function useEstimate(key: string) {
  return useMutation({ mutationFn: () => api.post<Estimate>(`/issues/${enc(key)}/ai/estimate`) })
}

export function useBreakdown(key: string) {
  return useMutation({ mutationFn: () => api.post<Breakdown>(`/issues/${enc(key)}/ai/subtasks`) })
}

export function useEstimateProject(projectKey: string) {
  return useMutation({
    mutationFn: () => api.post<{ queued: number }>(`/projects/${enc(projectKey)}/ai/estimate`),
  })
}

export function useSetAITask() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: {
      task: string
      provider_id: number
      model: string
      fallbacks?: ChainModel[]
    }) =>
      api.put<AITask[]>(`/ai/tasks/${enc(v.task)}`, {
        provider_id: v.provider_id,
        model: v.model,
        fallbacks: v.fallbacks ?? [],
      }),
    onSuccess: (tasks) => qc.setQueryData(aiq.tasks().queryKey, tasks),
  })
}

export function useRunSafetyDrill() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.post<SafetyDrills>('/ai/safety-drill'),
    onSuccess: (d) => qc.setQueryData(aiq.drill().queryKey, d),
  })
}

export function usePinReviewArm() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { arm: string; pin: '' | 'on' | 'off' }) =>
      api.put<ReviewArms>('/ai/review-arms/pins', v),
    onSuccess: (d) => qc.setQueryData(aiq.reviewArms().queryKey, d),
  })
}
