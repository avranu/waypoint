// The Agents page's URL state (WP-D15), kept apart from the page so the
// router can validate it without loading the page.

export type AgentsTab =
  | 'overview'
  | 'queue'
  | 'sessions'
  | 'autopilot'
  | 'activity'
  | 'pulls'
  | 'knowledge'
  | 'usage'
  | 'trends'
  | 'cli'
  | 'kit'
  | 'setup'
  | 'audits'
  | 'audit'

export interface AgentsSearch {
  tab?: AgentsTab
  /** An agent spec: an identity, a family (claude) or any word of one. */
  agent?: string
  /** A project key. */
  project?: string
  /** The period the history tabs cover, in days. */
  days?: number
  /** Pull requests tab: the one shown in detail. */
  pr?: number
  /** Usage tab: one machine, one CLI session id, and (admins) one person or "all". */
  machine?: string
  session?: string
  who?: string
  /** Sessions tab: where it ran and what state it is in. */
  repo?: string
  branch?: string
  state?: string
  /** Audit tab: risky shell commands agents ran, or tool calls the classifier refused, instead of people's decisions. */
  view?: AuditView
}

export type AuditView = 'risky' | 'refused'

export const AGENTS_TABS: AgentsTab[] = [
  'overview',
  'queue',
  'sessions',
  'autopilot',
  'activity',
  'pulls',
  'knowledge',
  'usage',
  'trends',
  'cli',
  'kit',
  'setup',
  'audits',
  'audit',
]
export const PERIODS = [7, 30, 90]
export const DEFAULT_DAYS = 30

export function validateAgentsSearch(s: Record<string, unknown>): AgentsSearch {
  const days = Number(s.days)
  const pr = Number(s.pr)
  return {
    tab: AGENTS_TABS.includes(s.tab as AgentsTab) ? (s.tab as AgentsTab) : undefined,
    agent: typeof s.agent === 'string' && s.agent.trim() ? s.agent.trim() : undefined,
    project: typeof s.project === 'string' && s.project ? s.project : undefined,
    days: PERIODS.includes(days) ? days : undefined,
    pr: Number.isInteger(pr) && pr > 0 ? pr : undefined,
    machine: typeof s.machine === 'string' && s.machine ? s.machine : undefined,
    session: typeof s.session === 'string' && s.session ? s.session : undefined,
    who: typeof s.who === 'string' && s.who ? s.who : undefined,
    repo: typeof s.repo === 'string' && s.repo ? s.repo : undefined,
    branch: typeof s.branch === 'string' && s.branch ? s.branch : undefined,
    state: typeof s.state === 'string' && s.state ? s.state : undefined,
    view: s.view === 'risky' || s.view === 'refused' ? s.view : undefined,
  }
}

/** What every tab is narrowed to. */
export interface HubFilters {
  /** An agent spec ('' = every agent), matched as delegation matches it. */
  agent: string
  /** A project key ('' = every project you can see). */
  project: string
  days: number
  /** Usage tab only. */
  machine?: string
  session?: string
  who?: string
  /** Sessions tab only (machine too). */
  repo?: string
  branch?: string
  state?: string
  /** Audit tab only. */
  view?: AuditView
}

export interface HubTabProps {
  filters: HubFilters
  /** Changes filters (undefined clears one), keeping the tab. */
  setFilters: (f: Partial<AgentsSearch>) => void
}
