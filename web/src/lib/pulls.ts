// Pull requests on the Agents page (WP-D15): where each went, its pushes,
// reviews and approvals, what bots found and what agents spent; and the
// note bot reviews start with.

import { queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { BotFinding, BotRun } from './types'

export type PRState = 'open' | 'draft' | 'merged' | 'closed'

export interface PRCounts {
  people: number
  bots: number
}

export interface PRIssue {
  id: number
  key: string
  title: string
  status: string
  category: string
  /** the relationship a mention states (mentions only) */
  verb?: string
}

export interface PRSummary {
  id: number
  forge: 'github' | 'forgejo' | 'gitlab'
  repo: string
  number: number
  title: string
  url: string
  author: string
  /** From someone who isn't a Waypoint person, waiting for a person to approve it (WP-281). */
  outside?: boolean
  /** Its text's coding agent, agents that linked it, agents that worked its issues. */
  agents: string[]
  state: PRState
  base_ref: string
  head_ref: string
  head_sha?: string
  /** The commit it became when merged: what its base's deployments run (WP-23). */
  merge_sha?: string
  opened_at: string | null
  merged_at: string | null
  closed_at: string | null
  merged_by?: string
  /** To its merge or close, or until now while open. */
  hours_open: number | null
  /** New heads after it opened. */
  pushes: number
  comments: PRCounts
  reviews: PRCounts
  approvals: string[]
  changes_requested: number
  findings: Partial<Record<'critical' | 'high' | 'medium' | 'low', number>>
  issues: PRIssue[]
  /** issues its text names that aren't its work, so not linked (WP-379) */
  mentions?: PRIssue[]
  agent_seconds: number
  input_tokens: number
  output_tokens: number
  cost_usd: number
  additions: number | null
  deletions: number | null
  changed_files: number | null
  commits: number | null
  last_activity: string
  /** CI on its head, the review gate and merge when ready (WP-D21). */
  ci?: CIState
  gate?: GateState
  armed?: boolean
}

export interface PRTotals {
  opened: number
  merged: number
  closed: number
  open_now: number
  median_hours_to_merge: number | null
  bot_reviews: number
  findings: number
  human_approvals: number
}

export interface PRList {
  totals: PRTotals
  pulls: PRSummary[]
}

export type PRTimelineKind =
  | 'opened'
  | 'pushed'
  | 'ready'
  | 'reopened'
  | 'commented'
  | 'reviewed'
  | 'approved'
  | 'changes_requested'
  | 'merged'
  | 'closed'
  | 'bot_review'
  | 'agent_run'
  | 'issue_status'

export interface PRTimelineItem {
  at: string
  kind: PRTimelineKind
  actor?: string
  bot?: boolean
  agent?: string
  model?: string
  sha?: string
  url?: string
  body?: string
  issue?: string
  detail?: string
  seconds?: number
  tokens?: number
  run_id?: number
  outcome?: string
}

export interface PRAgentTime {
  agent: string
  models: string[]
  runs: number
  seconds: number
  input_tokens: number
  output_tokens: number
  cost_usd: number
}

export interface PRDetail extends PRSummary {
  timeline: PRTimelineItem[]
  agent_time: PRAgentTime[]
  bot_runs: BotRun[]
  shipping: PRShipping | null
}

// ---- merge gates (WP-D21) ----

export type CIState = 'pending' | 'passed' | 'failed' | 'none' | ''
export type GateState = 'pending' | 'success' | 'failure' | ''
export type CheckState = 'pending' | 'success' | 'failed'

export interface PRCheck {
  name: string
  state: CheckState
  url?: string
  description?: string
  has_log?: boolean
}

export interface PRGate {
  enabled: boolean
  /** Findings at or above this severity block. */
  severity: BotFinding['severity']
  state: GateState
  note: string
  sha: string
  open: number
  below: number
}

export interface FindingEvent {
  old: string
  state: BotFinding['state']
  kind: 'person' | 'forge' | 'verify' | 'reply' | 'agent' | 'system'
  by: string
  sha?: string
  note?: string
  at: string
}

export interface PRFinding extends BotFinding {
  head_sha?: string
  created_at: string
  updated_at: string
  history?: FindingEvent[]
}

export interface PRArmed {
  at: string
  by?: string
  agent?: string
  style?: string
  /** The automerge label armed it. */
  label?: boolean
  /** The repo's Merge automatically setting armed it. */
  setting?: boolean
  /** That setting's base branches, absent for any: "into release/**". */
  scope?: string
  /** Why it hasn't merged yet, when it tried. */
  note?: string
}

export interface PRShipping {
  ci: CIState
  checks: PRCheck[]
  gate: PRGate
  findings: PRFinding[]
  mergeable: boolean | null
  /** only_waiting: an armed merge waits only for reviews to settle; a person may merge now */
  readiness: { ready: boolean; reasons: string[]; only_waiting?: boolean }
  armed: PRArmed | null
  /** The viewer may merge or arm it. */
  can_merge: boolean
  runs?: { id: number; kind: string; status: string }[]
  /** Waypoint holds it as a draft first: "draft" or "ready (full run pending)" */
  stage?: string
}

export type MergeStyle = '' | 'merge' | 'squash' | 'rebase'

export interface MergeResult {
  merged: boolean
  armed: boolean
  reasons?: string[]
}

export const checkPull = (id: number) => api.post<PRDetail>(`/pulls/${id}/check`)
export const mergePull = (id: number, body: { style: MergeStyle; when_ready: boolean }) =>
  api.post<MergeResult>(`/pulls/${id}/merge`, body)
export const disarmPull = (id: number) => api.del<void>(`/pulls/${id}/merge`)
export const dismissFinding = (id: number, note = '') =>
  api.post<BotFinding>(`/findings/${id}/dismiss`, { note })
export const reopenFinding = (id: number) => api.post<BotFinding>(`/findings/${id}/reopen`)

// ---- babysitting (WP-134) ----

export type BabysitState =
  | 'watching'
  | 'working'
  | 'waiting_review'
  | 'ready'
  | 'needs_person'
  | 'resting'
  | 'merged'
  | 'stopped'
  | 'gave_up'

export type CommentVerdict =
  | 'new'
  | 'seen'
  | 'accepted'
  | 'rejected'
  | 'partial'
  | 'needs_person'
  | 'fixing'
  | 'fixed'
  | 'verified'
  | 'dismissed'
  | 'deferred'

export interface BabysitRound {
  n: number
  sha: string
  at: string
  checks?: string[]
  conflict?: boolean
  comments?: string[]
  /** runner: an agent job; session: handed to the session that started it. */
  to: 'runner' | 'session'
  job?: number
  why?: string
  outcome?: string
  /** set on the stronger model's round that takes over a head's round that found nothing to fix */
  model?: string
  /** how its agent ended, shortened */
  answer?: string
  pushed?: boolean
  /** what the round came to (WP-438); nothing_pushed when its agent pushed nothing */
  class?: RoundClass
  /** what it was given, whatever came of it */
  work_class?: RoundClass
  /** each comment, finding and failed check it was given, with its class */
  ledger?: LedgerEntry[]
  /** the assessor's round (WP-439): the stronger model at high effort, fresh, with everything */
  assessor?: boolean
  effort?: string
  /** the question its agent ended with for a person, and its reason no agent can settle it */
  question?: string
  because?: string
  /** what its agent said it waits for outside the pull request */
  waiting?: string
  /** why Waypoint took the pull request out of the merge line after it, for a person's answer */
  parked?: string
  park_sha?: string
  /** the person's answer, or after a rest what the next round is told */
  reply?: string
  /** a park nobody was asked about (WP-930): when it tries again, unless what it is over changes first */
  rest_until?: string
  /** when that rest re-ran its failing checks: their result ends it */
  rerun_at?: string
}

export type RoundClass = 'new' | 'repeat' | 'caused' | 'ci_or_conflict' | 'base' | 'nothing_pushed'

export interface LedgerEntry {
  /** c12, an audit finding's 12.3, a check's name, or "conflict" */
  id: string
  kind: 'comment' | 'finding' | 'check' | 'conflict'
  path?: string
  /** the function or type it is in */
  symbol?: string
  class: RoundClass
  /** repeat: the earlier comment or finding it makes again */
  of?: string
  /** repeat: the round that fixed it; caused: the round whose change it is on */
  round?: number
  why?: string
}

export interface Babysit {
  id: number
  pr_id: number
  state: BabysitState
  active: boolean
  /** What it waits on, in words. */
  wait?: string
  round: number
  max_rounds: number
  model: string
  owner: { id: number; username: string; display_name: string }
  started_by?: string
  agent?: string
  via?: string
  rounds: BabysitRound[]
  end_reason?: string
  created_at: string
  ended_at?: string
}

export interface ReviewComment {
  /** c12: what wp pr ack takes. */
  id: string
  kind: 'inline' | 'review' | 'comment' | 'finding' | 'relayed'
  author: string
  bot?: boolean
  path?: string
  line?: number
  body: string
  url?: string
  outdated?: boolean
  verdict: CommentVerdict
  note?: string
  reactions: string[]
  gone?: boolean
  dealt: boolean
  created_at: string
}

export interface BabysitView {
  babysit: Babysit | null
  comments: ReviewComment[]
  can_edit: boolean
}

export const babysitQuery = (prID: number) =>
  queryOptions({
    queryKey: ['pulls', 'babysit', prID],
    queryFn: () => api.get<BabysitView>(`/pulls/${prID}/babysit`),
    staleTime: 30_000,
  })

export const startBabysit = (prID: number) =>
  api.post<Babysit>(`/pulls/${prID}/babysit`, { via: 'web' })
export const stopBabysit = (prID: number) => api.del<Babysit>(`/pulls/${prID}/babysit`)
export const dismissComment = (id: string, note = '') =>
  api.post<ReviewComment>(`/review-comments/${id}/dismiss`, { note })

export type PRStateFilter = '' | 'open' | 'merged' | 'closed'

export const pullsQuery = (f: {
  project: string
  agent: string
  days: number
  state: PRStateFilter
  q: string
}) =>
  queryOptions({
    queryKey: ['pulls', 'list', f],
    queryFn: () =>
      api.get<PRList>(
        `/pulls${qs({
          project: f.project || undefined,
          agent: f.agent || undefined,
          state: f.state || undefined,
          q: f.q || undefined,
          days: f.days,
        })}`,
      ),
    staleTime: 30_000,
    placeholderData: (prev) => prev,
  })

export const pullQuery = (id: number) =>
  queryOptions({
    queryKey: ['pulls', 'one', id],
    queryFn: () => api.get<PRDetail>(`/pulls/${id}`),
    staleTime: 30_000,
  })

/** Text from outside on a pull request, waiting for a person (WP-281). */
export interface OutsidePRText {
  id: number
  pr_id: number
  kind: 'pull' | 'inline' | 'review' | 'comment'
  author: string
  url?: string
  path?: string
  line?: number
  /** the pull request's title (kind pull) */
  title?: string
  body: string
  /** the code approving a pull request covers */
  head_sha?: string
  created_at: string
  updated_at: string
}

export const outsidePRQuery = (prID: number) =>
  queryOptions({
    queryKey: ['pulls', 'outside', prID],
    queryFn: () => api.get<OutsidePRText[]>(`/pulls/${prID}/outside`),
    staleTime: 30_000,
  })

export const decideOutsidePR = (id: number, approve: boolean) =>
  api.post(`/outside-pr/${id}/${approve ? 'approve' : 'discard'}`)

// ---- the review note (Admin → AI) ----

export interface ReviewNote {
  note: string
  default: string
  /** An admin worded it. */
  custom: boolean
}

export const reviewNoteQuery = () =>
  queryOptions({
    queryKey: ['ai', 'review-note'],
    queryFn: () => api.get<ReviewNote>('/admin/review-note'),
    staleTime: 5 * 60_000,
  })

export const setReviewNote = (note: string) => api.put<ReviewNote>('/admin/review-note', { note })

/** The note as a review shows it, with these models named. */
export const renderNote = (note: string, models: string) =>
  (note.trim() || '').replaceAll('{models}', models)

// ---- formatting ----

/** "3h", "2.5d", "40m": how long a pull request was open. */
export function hoursLabel(h: number | null | undefined) {
  if (h == null) return '—'
  if (h < 1) return `${Math.max(1, Math.round(h * 60))}m`
  if (h < 48) return `${h < 10 ? Math.round(h * 10) / 10 : Math.round(h)}h`
  const d = h / 24
  return `${d < 10 ? Math.round(d * 10) / 10 : Math.round(d)}d`
}

/** 1.2k, 34k, 1.1M. */
export function compact(n: number) {
  if (n < 1000) return String(n)
  if (n < 1_000_000) return `${Math.round(n / 100) / 10}k`.replace('.0k', 'k')
  return `${Math.round(n / 100_000) / 10}M`.replace('.0M', 'M')
}

export function usd(n: number) {
  if (!n) return ''
  return n < 0.01 ? '<$0.01' : n < 10 ? `$${n.toFixed(2)}` : `$${Math.round(n)}`
}
