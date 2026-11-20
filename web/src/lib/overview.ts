// The Agents overview (WP-D16): which agents work where, what they finished,
// what comes next, what waits on a person; one request for the whole tab.
// Recent changes page on their own, and a session's usage has its own call.

import { infiniteQueryOptions, queryOptions } from '@tanstack/react-query'
import { api, qs } from './api'
import type { UserRef } from './types'
import type { Waiting } from './requests'
import type { AgentSession, SessionState } from './sessions'
import type { PRSummary, PRTotals } from './pulls'
import type { EnvSummary } from './environments'
import type {
  UsageBreakdown,
  UsageRefusals,
  UsageSummary,
  UsageTool,
  UsageTotals,
} from './usage-analytics'

export interface OverviewItem {
  /** issue, pr, request, session, review, design */
  kind: string
  ref?: string
  title: string
  detail?: string
  /** an app path */
  url?: string
  /** who did it; for a request, the agent that asked */
  agent?: string
  /** a request: the person who decided it */
  by?: string
  at: string
}

export interface SummaryPart {
  kind: string
  label: string
  count: number
}

export interface SummaryList {
  count: number
  parts: SummaryPart[]
  /** the top few, linked */
  items: OverviewItem[]
}

export interface OverviewSummary {
  doing: SummaryList
  /** what agents did: issues closed, pull requests merged */
  done: SummaryList
  /** requests people decided: their work, not the agents' */
  resolved: SummaryList
  deferred: SummaryList
  waiting: SummaryList
}

export interface NextItem {
  key: string
  title: string
  project: string
  priority: number
  why: string
  /** place in what next hands out */
  position: number
  pinned?: boolean
}

export interface OverviewNext {
  /** the agent the work was delegated to */
  agent: string
  items: NextItem[]
  /** ready issues beyond those listed */
  more: number
}

export interface OverviewClaim {
  key: string
  title: string
  project: string
  agent: string
  since: string
  seconds: number
}

export interface BranchSession {
  id: number
  agent: string
  state: SessionState
  title?: string
}

export interface OverviewBranch {
  repo: string
  forge: string
  branch: string
  head_sha: string
  pushed_at: string
  pusher?: string
  created?: boolean
  ci?: string
  pr: {
    id: number
    number: number
    url: string
    title: string
    state: string
    gate?: string
    armed?: boolean
  } | null
  sessions: BranchSession[]
}

export type ChangeKind = 'issue' | 'page' | 'glossary' | 'design'
export const CHANGE_KINDS: { id: ChangeKind; label: string }[] = [
  { id: 'issue', label: 'Issues' },
  { id: 'page', label: 'Pages' },
  { id: 'glossary', label: 'Glossary' },
  { id: 'design', label: 'Design' },
]

export interface Change {
  kind: ChangeKind
  /** created, edited, moved to Done, commented on… */
  verb: string
  ref: string
  title: string
  url: string
  project: string
  by?: UserRef
  /** the agent behind it, or an automation's name */
  agent?: string
  at: string
}

export interface ChangeList {
  changes: Change[]
  /** pass as before to get the next page */
  next_before?: number
}

export interface Overview {
  at: string
  days: number
  agent: string
  project: string
  waiting: Waiting
  sessions: AgentSession[]
  sessions_total: number
  next: OverviewNext[]
  claims: OverviewClaim[]
  pulls: { open: PRSummary[]; total: number; totals: PRTotals }
  branches: OverviewBranch[]
  changes: ChangeList
  done: OverviewItem[]
  resolved: OverviewItem[]
  environments: EnvSummary | null
  usage: UsageSummary | null
  summary: OverviewSummary
}

export interface OverviewFilters {
  agent: string
  project: string
  days: number
}

export const overviewQuery = (f: OverviewFilters) =>
  queryOptions({
    queryKey: ['agents', 'overview', f.agent, f.project, f.days],
    queryFn: () =>
      api.get<Overview>(
        `/agents/overview${qs({ agent: f.agent || undefined, project: f.project || undefined, days: f.days })}`,
      ),
    staleTime: 15_000,
    placeholderData: (prev) => prev,
  })

/** Changes by kind, a page at a time (the first page also comes with the overview). */
export const changesQuery = (f: { kind: ChangeKind | ''; agent: string; project: string }) =>
  infiniteQueryOptions({
    queryKey: ['changes', f.kind, f.agent, f.project],
    queryFn: ({ pageParam }) =>
      api.get<ChangeList>(
        `/changes${qs({
          kind: f.kind || undefined,
          agent: f.agent || undefined,
          project: f.project || undefined,
          before: pageParam || undefined,
          limit: 15,
        })}`,
      ),
    initialPageParam: 0,
    getNextPageParam: (last) => last.next_before || undefined,
    staleTime: 15_000,
  })

export interface SessionUsage {
  has_data: boolean
  /** it is another person's session: usage is private to each person */
  private?: boolean
  totals: UsageTotals
  models: UsageBreakdown[]
  /** the session itself and its subagents */
  kinds: UsageBreakdown[]
  tools: UsageTool[]
  bash: UsageTool[]
  refusals: UsageRefusals
}

export const sessionUsageQuery = (id: number) =>
  queryOptions({
    queryKey: ['session', id, 'usage'],
    queryFn: () => api.get<SessionUsage>(`/sessions/${id}/usage`),
    staleTime: 60_000,
  })
