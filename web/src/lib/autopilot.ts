import { queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { IssueRef } from './types'

export type MergePolicy = 'auto' | 'always' | 'never'
export type ReviewSkip = 'on' | 'shadow' | 'off'

/** What a person can set, for all projects or one; an absent field inherits and 0 turns a limit off. */
export interface AutopilotLimits {
  max_hourly_usd?: number
  week_ahead?: number
  week_max?: number
  week_end_hours?: number
  five_hour_max?: number
  merge?: MergePolicy
  grace_seconds?: number
  /** an adversarial review of each finished task before the session moves on (WP-285) */
  review?: boolean
  /** what the review asks; empty or unset: the default */
  review_prompt?: string
  /** review triage (WP-692): on skips low-risk reviews, shadow only records, off never skips */
  review_skip?: ReviewSkip
  /** the reviewer's brief for every review round (WP-1320); '' or unset: the default */
  review_text?: string
  /** who reviews a round: auto, session, or a model as cli/model (gateway/MODEL, claude/sonnet) */
  review_model?: string
  // Every criterion set must hold for a task to be taken.
  /** low, medium, high or urgent; '' any */
  min_priority?: Priority | ''
  /** due within so many days, or overdue; 0 off */
  due_within_days?: number
  /** a person wrote or approved the task */
  approved?: boolean
  /** a published plan names it, or its prep is ready */
  planned?: boolean
  /** a query the tasks must also match */
  query?: string
  /** starts in an hour (1-60, the loop guard) */
  per_hour?: number
  /** starts in a calendar day; 0 off */
  per_day?: number
  /** least minutes between two starts; 0 off */
  spacing_minutes?: number
  /** sessions under way in the project at a time: autopilot's, and yours while at a task; 0: starts none of its own */
  at_once?: number
  /** the longest the merge policy holds a task, in hours */
  merge_wait_hours?: number
  /** "09:00-17:00" in your time zone; '' any time */
  hours?: string
  /** "mon-fri"; '' every day */
  days?: string
  /** start sessions past at_once over the week's last week_end_hours, paced to use the quota by the reset */
  spend_left?: boolean
  /** while it does, autopilot sessions at once across your projects */
  spend_at_once?: number
}

export type Priority = 'low' | 'medium' | 'high' | 'urgent'

export const MIN_PRIORITY_LABEL: Record<Priority, string> = {
  low: 'Low or above',
  medium: 'Medium or above',
  high: 'High or above',
  urgent: 'Urgent only',
}

/** The longest criteria query the server takes. */
export const MAX_CRITERIA_QUERY = 500

/** The longest review prompt the server takes. */
export const MAX_REVIEW_PROMPT = 2000

/** The longest reviewer's brief the server takes (WP-1320). */
export const MAX_REVIEW_TEXT = 12000

/** One thing a round's reviewer can be (WP-1320): the value saved and what it is. */
export interface ReviewModelOption {
  value: string
  label: string
  note?: string
}

/** Who can review a round, and the brief a reviewer gets when none is set. */
export interface ReviewModelOptions {
  modes: ReviewModelOption[]
  gateway: ReviewModelOption[]
  runners: ReviewModelOption[]
  default_text: string
  max_text: number
}

export type StepState = 'due' | 'waiting' | 'starting' | 'started' | 'stopped' | 'failed'

export interface AutopilotStep {
  id: number
  state: StepState
  reason?: string
  detail?: string
  due_at: string
  /** absent: a session of autopilot's own that hasn't started yet */
  session?: { id: number; title: string; machine: string; url: string } | null
  done?: IssueRef
  next?: IssueRef
  next_session?: number
  job?: number
  /** started to use what's left of the week (WP-262) */
  spend?: boolean
  created_at: string
  updated_at: string
}

export interface AutopilotProject {
  project_id: number
  project_key: string
  project_name: string
  enabled: boolean
  /** this project's own overrides */
  limits: AutopilotLimits
  effective: AutopilotLimits
  /** the effective criteria in words; '' any ready task */
  criteria: string
  /** the effective pace in words; '' Waypoint's (8 an hour, any time) */
  pace: string
  steps: AutopilotStep[]
  /** your open sessions here that autopilot isn't doing anything with (WP-307) */
  sessions: AutopilotOpenSession[]
}

/** One of your open sessions in a project, busy or idle (WP-307). */
export interface AutopilotOpenSession {
  id: number
  title: string
  machine: string
  url: string
  state: 'working' | 'waiting' | 'idle'
  /** it (re)started and nothing was typed in it yet: it sits at an empty prompt */
  fresh?: boolean
  /** the task it is working on */
  task?: IssueRef
  /** autopilot can start the next task in it now; else why not */
  startable: boolean
  why?: string
  last_seen_at: string
}

/** What the Autopilot tab says about one of your open sessions. */
export function openSessionLine(x: AutopilotOpenSession): string {
  if (x.task) return `Working on ${x.task.key}: autopilot starts the next task after it.`
  if (x.state === 'waiting') return 'Waiting on you.'
  if (x.fresh) return 'Fresh, at an empty prompt.'
  if (x.state === 'working') return 'Working.'
  return 'Idle.'
}

export interface AutopilotMeter {
  hourly_usd: number
  week_percent?: number
  week_even?: number
  week_resets_at?: string
  week_cap?: number
  five_hour_percent?: number
  five_hour_resets_at?: string
  quota_sampled_at?: string
  quota_unknown?: boolean
  measured_at: string
  paused_because?: string
  /** you turned the quota limits off until then */
  quota_off_until?: string
  /** while they are off: what they would pause for now */
  quota_held?: string
}

export interface AutopilotOverview {
  /** what you set for all projects */
  defaults: AutopilotLimits
  /** defaults with Waypoint's filled in */
  effective: AutopilotLimits
  /** Waypoint's own */
  waypoint: AutopilotLimits
  /** your defaults' criteria and pace in words */
  criteria: string
  pace: string
  projects: AutopilotProject[]
  meter: AutopilotMeter
  unreviewed: number
  /** the week's-end plan (WP-262) */
  spend?: SpendPlan
}

/** Use what's left at the week's end (WP-262): the plan now. */
export interface SpendPlan {
  on: boolean
  in_window: boolean
  /** when the week's end begins, before it does */
  opens_at?: string
  resets_at?: string
  percent?: number
  /** points an hour, over the last hour */
  per_hour?: number
  /** where the week lands at the reset at that pace */
  projected?: number
  target?: number
  /** you're using Claude Code: it leaves you room */
  person_active?: boolean
  running: number
  cap: number
  wants: boolean
  start: boolean
  /** the plan in words */
  why: string
  started: SpendStart[]
}

export interface SpendStart {
  step: number
  project: string
  task?: IssueRef
  state: StepState
  session?: number
  started_at: string
}

export interface TaskSummary {
  id: number
  project?: string
  issue?: IssueRef
  session: number
  session_url: string
  machine?: string
  agent?: string
  body: string
  asks: string[]
  request?: string
  request_url?: string
  /** autopilot moved the session on after it */
  autopilot: boolean
  created_at: string
  updated_at: string
  reviewed_at?: string
}

export interface TaskSummaryPage {
  summaries: TaskSummary[]
  unreviewed: number
  more: boolean
}

export interface LimitsInput {
  limits: AutopilotLimits
  /** only the given limits change */
  merge?: boolean
  /** clear them all: inherit */
  reset?: boolean
  /** your defaults only: the quota limits are off until then; a past time turns them back on */
  quota_off_until?: string
}

export const REVIEW_SKIP_LABEL: Record<ReviewSkip, string> = {
  on: 'Skip low-risk changes',
  shadow: 'Record only, review everything',
  off: 'Review everything',
}

export const MERGE_LABEL: Record<MergePolicy, string> = {
  auto: 'Only related tasks wait',
  always: 'Always wait for open PRs',
  never: 'Never wait',
}

/** A live step still waits for its turn (or a person). */
export const stepLive = (s: AutopilotStep) => s.state === 'due' || s.state === 'waiting'

/** Reasons that mean a limit or the pace, not the session, is what holds it. */
const LIMIT_REASONS = new Set([
  'hourly',
  'week',
  'week_max',
  'five_hour',
  'rate',
  'schedule',
  'pace',
  'at_once',
  'quota_guard',
])
export const stepPaused = (s: AutopilotStep) => !!s.reason && LIMIT_REASONS.has(s.reason)

/** "40 s", "3 min", "2 h 5 min": the time left until `iso`, or '' once it has passed. */
export function untilText(iso: string, now: number): string {
  const sec = Math.round((new Date(iso).getTime() - now) / 1000)
  if (sec <= 0) return ''
  if (sec < 90) return `${sec} s`
  const min = Math.round(sec / 60)
  if (min < 90) return `${min} min`
  const h = Math.floor(min / 60)
  return min % 60 ? `${h} h ${min % 60} min` : `${h} h`
}

/** One line on what autopilot is doing, or did, about the step's session. */
export function stepLine(s: AutopilotStep, now: number): string {
  const next = s.next ? s.next.key : 'The next task'
  const detail = s.detail ?? ''
  switch (s.state) {
    case 'due': {
      const left = untilText(s.due_at, now)
      if (s.reason === 'prompted') {
        const used = detail ? detail[0].toUpperCase() + detail.slice(1) : 'You used the session'
        return `${used}: it moves on once it sits idle${left ? ` (in ${left} if left alone)` : ''}.`
      }
      return left ? `${next} starts in ${left}` : `${next} starts any moment`
    }
    case 'waiting':
      return stepPaused(s)
        ? `Paused: ${detail.replace(/^paused: /, '') || s.reason}`
        : `Waiting: ${detail || s.reason || 'for something to finish'}`
    case 'starting':
      if (!s.next && detail.includes('adversarial review'))
        return detail[0].toUpperCase() + detail.slice(1)
      return `Starting ${s.next ? s.next.key : 'the next task'}${detail ? ': ' + detail : ''}`
    case 'started':
      return `Started ${s.next ? s.next.key : 'the next task'}`
    case 'stopped':
      return `Stopped${detail ? ': ' + detail : ''}`
    case 'failed':
      return `Failed${detail ? ': ' + detail : ''}`
  }
}

/** One day of what autopilot and your agents did (days are in your time zone). */
export interface AutopilotDay {
  /** YYYY-MM-DD */
  day: string
  /** tasks agents finished */
  finished: number
  /** of those, the ones autopilot moved the session on after */
  moved_on: number
  /** sessions autopilot started a next task in */
  started: number
  /** spend at list price, all sessions */
  spend_usd: number
}

export interface AutopilotActivity {
  days: AutopilotDay[]
}

/** The last days of work, oldest first, for the charts at the top of Agents → Autopilot. */
export const autopilotActivityQuery = (days = 14) =>
  queryOptions({
    queryKey: ['autopilot', 'activity', days],
    queryFn: () =>
      api.get<AutopilotActivity>(
        `/autopilot/activity${qs({ days, offset: -new Date().getTimezoneOffset() })}`,
      ),
    staleTime: 60_000,
    refetchInterval: 120_000,
    placeholderData: (prev) => prev,
  })

/** The short words a step or open session is listed by: what it is doing now. */
export type RowTone = 'live' | 'wait' | 'pause' | 'run' | 'done' | 'stop' | 'fail' | 'idle' | 'you'

export interface RowBrief {
  tone: RowTone
  /** one short line */
  label: string
  /** the whole sentence, for the tooltip */
  full: string
}

const cap = (t: string) => (t ? t[0].toUpperCase() + t.slice(1) : t)

/** A step as one short line, with the whole sentence kept for a tooltip. */
export function stepBrief(s: AutopilotStep, now: number): RowBrief {
  const full = stepLine(s, now)
  const next = s.next ? s.next.key : 'Next task'
  const detail = (s.detail ?? '').replace(/^paused: /, '')
  switch (s.state) {
    case 'due': {
      const left = untilText(s.due_at, now)
      if (s.reason === 'prompted')
        return {
          tone: 'you',
          label: `You used it: moves on once idle${left ? ` (${left})` : ''}`,
          full,
        }
      return { tone: 'live', label: left ? `${next} in ${left}` : `${next} now`, full }
    }
    case 'waiting':
      return stepPaused(s)
        ? { tone: 'pause', label: cap(detail || s.reason || 'Paused'), full }
        : { tone: 'wait', label: cap(detail || s.reason || 'Waiting'), full }
    case 'starting':
      return { tone: 'run', label: full, full }
    case 'started':
      return { tone: 'done', label: `Started ${s.next ? s.next.key : 'next task'}`, full }
    case 'stopped':
      return { tone: 'stop', label: s.detail ? cap(s.detail) : 'Stopped', full }
    case 'failed':
      return { tone: 'fail', label: s.detail ? `Failed: ${s.detail}` : 'Failed', full }
  }
}

/** An open session as one short line. */
export function openSessionBrief(x: AutopilotOpenSession): RowBrief {
  const full = openSessionLine(x)
  if (x.task) return { tone: 'run', label: `Working on ${x.task.key}`, full }
  if (x.state === 'waiting') return { tone: 'you', label: 'Waiting on you', full }
  if (x.fresh) return { tone: 'idle', label: 'Fresh, empty prompt', full }
  if (x.state === 'working') return { tone: 'run', label: 'Working', full }
  return { tone: 'idle', label: 'Idle', full }
}
