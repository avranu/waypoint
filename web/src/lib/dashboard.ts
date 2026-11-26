import { api, enc } from './api'
import type { GoalCoverage, GoalDrift, Grounding, IssueDetail, IssueRef } from './types'

export interface DashStage {
  name: string
  category: string
  color: string
  count: number
  /** how long its tasks have been in it, typically */
  median_days: number
  /** in it two weeks or more */
  old: number
  /** the in-progress column where work waits longest */
  bottleneck?: boolean
}

export interface DashWeek {
  week: string
  done: number
  added: number
}

export interface DashPace {
  /** the last twelve weeks, oldest first; the last is this week so far */
  weeks: DashWeek[]
  done_4w: number
  done_prev_4w: number
  added_4w: number
  /** median days from filed to done over the last four weeks */
  lead_days: number | null
}

export interface DashGoal {
  id: number
  code?: string
  title: string
  depth: number
  inherited?: boolean
  /** its words read as a task, not an outcome */
  unclear?: boolean
  why?: string
  coverage: GoalCoverage
}

export interface DashGoals {
  scoped: boolean
  goals: DashGoal[]
  suggested: DashGoal[]
  totals: Partial<Record<Grounding, number>>
  untraced: number
  waiting: number
  drift: GoalDrift | null
  ask: string[]
}

export type DashTone = 'bad' | 'warn' | 'info'

export interface DashSignal {
  id: string
  tone: DashTone
  title: string
  count: number
  /** WQL for the tasks behind it */
  query?: string
  /** the page that shows it otherwise */
  where?: 'goals' | 'inbox' | 'reports' | 'board'
  /** what one click does */
  fix?: 'agent' | 'suggest_goals'
  /** the open task already filed for it */
  filed?: IssueRef
}

export interface Dashboard {
  project: string
  name: string
  color: string
  icon: string
  description: string
  open: number
  stages: DashStage[]
  pace: DashPace
  /** absent on the all-projects view */
  goals?: DashGoals
  signals: DashSignal[]
  can_fix: boolean
  can_suggest: boolean
  on_goal: number
  traced: number
  scoped: boolean
}

export interface DashboardAll {
  projects: Dashboard[]
}

export const dashboardApi = {
  fix: (project: string, signal: string) =>
    api.post<{ issue: IssueDetail; created: boolean }>(`/projects/${enc(project)}/dashboard/fix`, {
      signal,
    }),
}

/** Finished per week over the last four weeks. */
export const perWeek = (p: DashPace) => p.done_4w / 4

/** Share of traced open work that serves an approved goal, or null. */
export const onGoalShare = (d: Pick<Dashboard, 'on_goal' | 'traced'>) =>
  d.traced > 0 ? d.on_goal / d.traced : null
