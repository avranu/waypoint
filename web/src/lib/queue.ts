import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { api, enc, qs } from './api'
import type { Issue, UserRef } from './types'
import { toastError } from '@/components/ui/toast'

export type WaitReason =
  | 'step'
  | 'claimed'
  | 'project_paused'
  | 'backlog'
  | 'hold'
  | 'released'
  | 'blocked'
  | 'subtasks'
  | 'review'
  | 'safety'
  | 'objective'

export interface QueueItem {
  issue: Issue
  project_name: string
  workflow?: string
  step_no?: number
  steps?: number
  /** place in what next hands out (1 = next); absent while waiting */
  position?: number
  pinned?: boolean
  /** what puts it there: "Pinned at #2 by Robin", "Urgent · due Oct 14" */
  why?: string
  reason?: WaitReason
  detail?: string
  blockers?: string[]
  /** the agents the filter covers that may take it */
  takers?: string[]
  delegated_by?: UserRef
  delegated_at?: string
  pinned_by?: UserRef
  /** working: time and tokens on the current claim */
  seconds?: number
  input_tokens?: number
  output_tokens?: number
}

export interface AgentPause {
  id: number
  spec: string
  note?: string
  /** running sessions included: they start no agents, finish their step and stop (WP-146) */
  sessions?: boolean
  by: UserRef | null
  created_at: string
}

export interface AgentQueue {
  agent: string
  /** one agent: Next is exactly what next hands it */
  exact: boolean
  covers: string[]
  pauses: AgentPause[]
  paused_projects: string[]
  working: QueueItem[]
  next: QueueItem[]
  waiting: QueueItem[]
}

export const queueQuery = (agent: string, project: string) =>
  queryOptions({
    queryKey: ['agents', 'queue', agent, project],
    queryFn: () =>
      api.get<AgentQueue>(
        `/agents/queue${qs({ agent: agent || undefined, project: project || undefined })}`,
      ),
    staleTime: 15_000,
  })

export const pausesQuery = () =>
  queryOptions({
    queryKey: ['agents', 'pauses'],
    queryFn: () => api.get<AgentPause[]>('/agents/pauses'),
  })

/**
 * Where to hold an issue in the queue: `position` is its place (1 = next) in
 * the list you're looking at; `top` is position 1; none pins it where it is.
 * Placing a task moves only that task (and the pins it passes); every other
 * task keeps following the automatic order. `unpin` hands it back to that order.
 */
export type QueueMove =
  | { key: string; position?: number; top?: boolean; unpin?: undefined }
  | { key: string; unpin: true }

const invalidateAgents = (qc: ReturnType<typeof useQueryClient>) =>
  qc.invalidateQueries({ queryKey: ['agents'] })

/**
 * Moves an issue in the queue. The list is reordered at once (optimistic),
 * then refetched, so what you see settles on the server's order.
 */
export function useQueueMove(agent: string, project: string) {
  const qc = useQueryClient()
  const key = queueQuery(agent, project).queryKey
  return useMutation({
    mutationFn: ({ reorder: _shown, ...m }: QueueMove & { reorder?: QueueItem[] }) =>
      api.post('/agents/queue/order', {
        ...m,
        agent: agent || undefined,
        project: project || undefined,
      }),
    onMutate: async (m) => {
      if (!m.reorder) return
      await qc.cancelQueries({ queryKey: key })
      const prev = qc.getQueryData(key)
      qc.setQueryData(key, (q) => (q ? { ...q, next: m.reorder! } : q))
      return { prev }
    },
    onError: (e, _m, ctx) => {
      if (ctx?.prev) qc.setQueryData(key, ctx.prev)
      toastError(e)
    },
    onSettled: () => invalidateAgents(qc),
  })
}

export function usePauseAgents() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (p: { spec: string; note?: string; sessions?: boolean }) =>
      api.post<AgentPause>('/agents/pauses', p),
    onError: toastError,
    onSettled: () => invalidateAgents(qc),
  })
}

export function useResumeAgents() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (spec: string) =>
      api.del<{ resumed: number }>(`/agents/pauses${qs({ spec: spec || undefined })}`),
    onError: toastError,
    onSettled: () => invalidateAgents(qc),
  })
}

/** A person takes a task back from the agent holding it. */
export function useReleaseClaim() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (key: string) => api.post(`/agents/release/${enc(key)}`),
    onError: toastError,
    onSettled: () => {
      invalidateAgents(qc)
      qc.invalidateQueries({ queryKey: ['issue'] })
    },
  })
}
