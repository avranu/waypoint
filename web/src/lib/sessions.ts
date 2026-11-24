import { queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { Action, IssueRef, UserRef } from './types'
import type { AutomationRun } from './automations'
import type { WRequest } from './requests'

export type SessionState = 'working' | 'waiting' | 'idle' | 'ended'

export interface SessionReview {
  task: string
  stage:
    | 'queued'
    | 'audit_queued'
    | 'audit'
    | 'starting'
    | 'running'
    | 'waiting'
    | 'failed'
    | 'skipped'
    | 'interrupted'
  label: string
  detail?: string
  since: string
  audit?: number
}

export function reviewFaulted(r: SessionReview): boolean {
  return r.stage === 'failed' || r.stage === 'interrupted'
}

/** Still under way, so it has a running clock. */
export function reviewLive(r: SessionReview): boolean {
  return !reviewFaulted(r) && r.stage !== 'skipped'
}

/** Something is really working on it, not waiting its turn. */
export function reviewWorking(r: SessionReview): boolean {
  return r.stage === 'audit' || r.stage === 'starting' || r.stage === 'running'
}

export interface AgentSession {
  id: number
  /** the CLI's own session id */
  cli_id: string
  cli: string
  machine: string
  user: UserRef
  agent: string
  model?: string
  cwd: string
  repo?: string
  branch?: string
  worktree?: boolean
  project?: { id: number; key: string; name: string }
  /** the task it holds */
  issue?: IssueRef
  title?: string
  /** idle after 30 minutes unheard, ended after 24 hours */
  state: SessionState
  /** with state waiting: idle_prompt, permission_prompt, … */
  wait_reason?: string
  /** only the owner (and admins) get prompts */
  last_prompt?: string
  last_update?: string
  input_tokens: number
  output_tokens: number
  started_at: string
  last_seen_at: string
  ended_at?: string
  /** open requests, not snoozed */
  open_requests: number
  /** what its last stop said still runs ("2 background tasks"); "?" when its
   * Claude Code doesn't say; "requests" when only open requests hold it */
  pending?: string
  /** it truly finished (WP-D22) and hasn't worked since */
  finished_at?: string
  /** steps queued for when it finishes; only its person sees them */
  follow_up?: FollowUpBrief
  review?: SessionReview
  /** its prompt cache and what Waypoint decided about keeping it warm (WP-192) */
  cache?: SessionCache
  /** "task": a launcher (wp start, a runner) bound it to `tasks` from birth;
   * "yours": started by hand (WP-34); absent for sessions from before then */
  kind?: 'task' | 'yours'
  /** the tasks a task session is bound to */
  tasks?: string[]
  /** the context its last model call carried, at its last stop */
  context_tokens: number
  url: string
}

/** Past this much context, a session that finished a task is better replaced
 * by a fresh one (core's BigContext, WP-34). */
export const BIG_CONTEXT = 150_000

export interface SessionCache {
  /** the tier its last calls wrote: "1h" or "5m" */
  ttl?: string
  /** for its latest stop: warm (pinged), cool, compacting, compacted, failed */
  decision?: 'warm' | 'cool' | 'compacting' | 'compacted' | 'failed'
  /** why, in words */
  note?: string
  /** keep-warm pings since its last real turn */
  pings: number
  at?: string
}

export type FollowUpState = 'none' | 'armed' | 'running' | 'succeeded' | 'failed' | 'cancelled'

export interface FollowUpBrief {
  state: FollowUpState
  steps: number
}

/** Steps queued on a session that run once the agent truly finishes (WP-D22). */
export interface FollowUp {
  session: number
  automation?: number
  state: FollowUpState
  steps: Action[]
  /** why an armed follow-up hasn't run yet */
  held?: string
  run?: AutomationRun
  runners?: { owner?: UserRef; total: number; online: number }
  updated_at?: string
}

/** What a session still runs, in words for a person. */
export function pendingText(s: Pick<AgentSession, 'pending' | 'open_requests'>): string {
  if (!s.pending) return ''
  if (s.pending === '?') return 'can’t tell when it finishes'
  if (s.pending === 'requests')
    return `waiting on ${s.open_requests === 1 ? 'a request' : `${s.open_requests || 'its'} requests`}`
  return `still running ${s.pending}`
}

/** What Waypoint did with a session's prompt cache at its latest stop. */
export function cacheText(c?: SessionCache): string {
  switch (c?.decision) {
    case 'warm':
      return c.pings > 1 ? `cache kept warm · ${c.pings} pings` : 'cache kept warm'
    case 'cool':
      return 'cache left to cool'
    case 'compacting':
      return 'compacting before its cache cools'
    case 'compacted':
      return 'compacted · cache cooling'
    case 'failed':
      return 'couldn’t keep its cache warm'
  }
  return ''
}

export interface SessionItem {
  kind: string
  label: string
  detail?: string
  url?: string
  at: string
}

export interface TimelineEntry {
  id: number
  kind: string
  label?: string
  detail?: string
  text?: string // The whole prompt, when it is longer than detail.
  url?: string
  at: string
}

export interface SessionSummary {
  session: AgentSession
  doing: { title?: string; task?: IssueRef; last_prompt?: string; last_update?: string }
  done: {
    issues_closed: SessionItem[]
    issues_created: SessionItem[]
    comments: number
    pages: SessionItem[]
    pull_requests: SessionItem[]
    commits: SessionItem[]
  }
  deferred: { issues: SessionItem[]; requests: WRequest[] }
  waiting: WRequest[]
  requests: WRequest[]
  runs: {
    issue: string
    title: string
    started_at: string
    ended_at?: string
    outcome?: string
    input_tokens: number
    output_tokens: number
  }[]
  /** newest first */
  timeline: TimelineEntry[]
}

export interface SessionFilters {
  user?: string
  machine?: string
  repo?: string
  branch?: string
  project?: string
  /** working, waiting, idle, ended, or active (all but ended) */
  state?: string
  agent?: string
  limit?: number
  offset?: number
}

export interface SessionPage {
  sessions: AgentSession[]
  total: number
  next?: number
}

export const sessionsQuery = (f: SessionFilters = {}) =>
  queryOptions({
    queryKey: ['sessions', f],
    queryFn: () => api.get<SessionPage>(`/sessions${qs({ ...f })}`),
    staleTime: 15_000,
  })

export const sessionQuery = (id: number) =>
  queryOptions({
    queryKey: ['session', id],
    queryFn: () => api.get<SessionSummary>(`/sessions/${id}`),
    staleTime: 15_000,
  })
