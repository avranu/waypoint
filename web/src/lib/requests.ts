import {
  infiniteQueryOptions,
  queryOptions,
  useMutation,
  useQueryClient,
} from '@tanstack/react-query'
import { api, enc, qs } from './api'
import type { IssueRef, UserRef } from './types'
import { toast, toastError } from '@/components/ui/toast'

export type RequestKind = 'question' | 'change' | 'command' | 'permission' | 'action'

export type RequestState =
  | 'open'
  | 'answered'
  | 'approved'
  | 'denied'
  | 'done'
  | 'dismissed'
  | 'withdrawn'
  | 'applying'
  | 'applied'
  | 'failed'
  | 'conflict'
  /** a hook's request the agent moved on from; result.reason says why (WP-66) */
  | 'expired'

export interface Choice {
  label: string
  description?: string
}

export interface Question {
  question: string
  header?: string
  choices?: Choice[]
  multi?: boolean
  default?: string
}

export interface Answer {
  choices?: string[]
  text?: string
}

export interface RequestFile {
  path: string
  base_sha?: string
  delete?: boolean
  new?: boolean
  /** path is absolute, outside any git repo: only a runner can write it */
  outside?: boolean
  additions: number
  deletions: number
  /** unified diff; only on a single request, not in lists */
  diff?: string
}

export interface CellRun {
  id: number
  command: string
  state: 'queued' | 'running' | 'done' | 'failed' | 'cancelled' | 'expired'
  exit_code?: number
  output?: string
  truncated?: boolean
  error?: string
  run_by?: UserRef
  created_at: string
  finished_at?: string
  duration_ms?: number
  /** queued: what its runner job waits for (WP-503) */
  waiting?: string
}

export interface RequestCell {
  n: number
  command: string
  dir?: string
  machine?: string
  /** newest first */
  runs: CellRun[]
}

export interface RequestResult {
  commit?: string
  branch?: string
  pr_url?: string
  pr_title?: string
  pr_number?: number
  reason?: string
  conflicts?: string[]
  /** The approved change altered no file (WP-948); reason says why. */
  no_change?: boolean
  /** The allow rule a person added with a permission approval (WP-233): pr_url adds it, or it was already there; unset when the rule picked couldn't be added, and reason says why. */
  rule?: string
}

/** An allow rule a person may add when approving a refused call (WP-233). */
export interface PermissionRule {
  rule: string
  /** what it lets through, in a sentence */
  covers: string
}

export interface PersonalRequestProject {
  id: number
  key: string
  name: string
}

export interface WRequest {
  id: number
  /** R-12 */
  code: string
  /** WP:R-12, or me:R-3 outside a project */
  ref: string
  number: number
  project?: PersonalRequestProject
  kind: RequestKind
  title: string
  body?: string
  state: RequestState
  user: UserRef
  agent?: string
  session?: {
    id: number
    machine: string
    repo?: string
    branch?: string
    cwd?: string
    title?: string
    state: string
  }
  issue?: IssueRef
  questions?: Question[]
  answers?: Answer[]
  files?: RequestFile[]
  message?: string
  tool?: { name: string; input?: unknown; reason?: string }
  /** an open permission request's allow rules, narrowest first, for rule_repo's .claude/settings.json */
  rules?: PermissionRule[]
  rule_repo?: string
  cells?: RequestCell[]
  /** Conditions that close the request by themselves, with their last check (WP-126). */
  watches?: {
    summary: string
    state: 'watching' | 'satisfied'
    note?: string
    checked_at?: string
  }[]
  /** What the classifier refused in its session in the hour before it was filed (WP-232); a person's read only. */
  refused?: ToolRefusal[]
  answer?: string
  decision?: string
  note?: string
  via?: string
  /** filed by Waypoint from a question at the end of the agent's turn (WP-96) */
  auto?: boolean
  decided_by?: UserRef
  decided_at?: string
  delivered_at?: string
  /** dismissed without a note, so the agent isn't told (WP-197) */
  quiet?: boolean
  snoozed_until?: string
  apply_path?: 'runner' | 'pr' | ''
  result: RequestResult
  error?: string
  can_decide: boolean
  runner?: { machine: string; online: boolean }
  created_at: string
  updated_at: string
  url: string
}

/**
 * A tool call Claude Code's auto-mode classifier refused (WP-232, ADR115):
 * logged for audit, not a request.
 */
export interface ToolRefusal {
  id: number
  at: string
  last_at: string
  /** refused this many times in its session, each within ten minutes of the last */
  repeats: number
  source: string
  tool: string
  /** the Bash command, file path or URL; hidden from readers who mustn't see command text */
  command?: string
  reason?: string
  agent?: string
  project?: PersonalRequestProject
  session?: WRequest['session']
  issue?: IssueRef
  /** the same call later ran in its session */
  ran_at?: string
  /** the permission request it was filed as, before WP-232 */
  request?: string
  /** allow rules a person may add for it, narrowest first, to rule_repo's .claude/settings.json (WP-233) */
  rules?: PermissionRule[]
  rule_repo?: string
  /** the rule a person picked from it: rule_pr adds it, or it was already there */
  rule?: string
  rule_pr?: string
  /** why a rule picked wasn't added */
  rule_reason?: string
}

/** An allow rule with the period's refused calls it would have let through (WP-232, WP-233). */
export interface RuleSuggestion {
  rule: string
  covers: string
  tool: string
  repo: string
  /** the newest refused call it covers: adding the rule goes through it */
  refusal: number
  refusals: number
  calls: number
  sessions: number
  ran: number
  added?: boolean
  pr?: string
}

export interface RefusalPage {
  refusals: ToolRefusal[]
  /** the whole period's, on the first page */
  counts?: {
    refusals: number
    calls: number
    sessions: number
    ran: number
    tools: Record<string, number>
  }
  /** rules that would have let the period's repeated refusals through, on the first page */
  rules?: RuleSuggestion[]
  more: boolean
}

export interface RefusalFilters {
  project?: string
  agent?: string
  days: number
  session?: number
}

/** The refused-calls log, newest first, a page at a time. */
export const refusalsQuery = (f: RefusalFilters) =>
  infiniteQueryOptions({
    queryKey: ['refusals', f.project ?? '', f.agent ?? '', f.days, f.session ?? 0],
    queryFn: ({ pageParam }) =>
      api.get<RefusalPage>(
        `/refusals${qs({
          project: f.project || undefined,
          agent: f.agent || undefined,
          days: f.days,
          session: f.session || undefined,
          before: pageParam || undefined,
          limit: 50,
        })}`,
      ),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.more ? last.refusals.at(-1)?.id : undefined),
    staleTime: 15_000,
    placeholderData: (prev) => prev,
  })

export interface RequestPage {
  requests: WRequest[]
  total: number
  next?: number
}

export interface Waiting {
  requests: WRequest[]
  /** design notes and edits to approve */
  design: number
  /** agent-written text to review */
  review: number
  pulls: number
  /** issues agents closed whose close no person has reviewed (WP-272); not in total */
  closes?: number
  /** issues holding text from outside for you to read and approve (WP-279) */
  outside?: number
  /** open pull requests holding text from outside for you to approve (WP-281) */
  outside_pulls?: number
  /** repos of projects you own that no webhook delivery has come from (count once) */
  hooks: HookProblem[]
  /** what the Inbox badge counts, besides unread notifications */
  total: number
}

/** A linked repo whose forge webhook isn't getting through (WP-31). */
export interface HookProblem {
  /** a project of yours it is linked to */
  project: string
  repo_id: number
  repo: string
  /** failed: Waypoint couldn't add it; silent: added or linked, but nothing came */
  state: 'failed' | 'silent'
  note?: string
}

export interface RequestFilters {
  state?: string
  mine?: boolean
  session?: number
  project?: string
  kind?: RequestKind | ''
  agent?: string
  limit?: number
}

export const waitingQuery = () =>
  queryOptions({
    queryKey: ['waiting'],
    queryFn: () => api.get<Waiting>('/waiting'),
    staleTime: 30_000,
  })

export const requestsQuery = (f: RequestFilters = {}) =>
  queryOptions({
    queryKey: ['requests', f],
    queryFn: () =>
      api.get<RequestPage>(
        `/requests${qs({
          state: f.state,
          mine: f.mine ? 1 : undefined,
          session: f.session,
          project: f.project,
          kind: f.kind || undefined,
          agent: f.agent,
          limit: f.limit,
        })}`,
      ),
    staleTime: 15_000,
  })

export const requestQuery = (ref: string) =>
  queryOptions({
    queryKey: ['request', ref],
    queryFn: () => api.get<WRequest>(`/requests/${enc(ref)}`),
    staleTime: 10_000,
  })

export interface Decision {
  decision: 'answer' | 'approve' | 'deny' | 'done' | 'dismiss'
  answers?: Answer[]
  note?: string
  /** dismissing an auto-filed question that wasn't one */
  not_a_question?: boolean
  apply_path?: 'runner' | 'pr'
  /** a permission approval: also add this allow rule (WP-233) */
  rule?: string
}

/** Every query a request touches. */
function refresh(qc: ReturnType<typeof useQueryClient>) {
  qc.invalidateQueries({ queryKey: ['request'] })
  qc.invalidateQueries({ queryKey: ['requests'] })
  qc.invalidateQueries({ queryKey: ['waiting'] })
  qc.invalidateQueries({ queryKey: ['sessions'] })
  qc.invalidateQueries({ queryKey: ['session'] })
}

/** Writes the answer from the server into the caches, and drops the request from the waiting list at once. */
function settle(qc: ReturnType<typeof useQueryClient>, r: WRequest) {
  qc.setQueryData<WRequest>(['request', r.ref], r)
  qc.setQueryData<WRequest>(['request', String(r.id)], r)
  qc.setQueryData<Waiting>(['waiting'], (w) =>
    w && r.state !== 'open'
      ? {
          ...w,
          requests: w.requests.filter((x) => x.id !== r.id),
          total: Math.max(0, w.total - (w.requests.some((x) => x.id === r.id) ? 1 : 0)),
        }
      : w,
  )
  refresh(qc)
}

export function useDecide(ref: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (d: Decision) => api.post<WRequest>(`/requests/${enc(ref)}/decide`, d),
    onSuccess: (r) => settle(qc, r),
    onError: (e) => (toastError(e), refresh(qc)),
  })
}

export function useSnooze(ref: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (minutes: number) =>
      api.post<WRequest>(`/requests/${enc(ref)}/snooze`, minutes > 0 ? { minutes } : {}),
    onSuccess: (r) => {
      qc.setQueryData<WRequest>(['request', ref], r)
      refresh(qc)
    },
    onError: (e) => (toastError(e), refresh(qc)),
  })
}

/** Add an allow rule a person picked for a refused call, by pull request (WP-233). */
export function useAllowRefused() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: number; rule: string }) =>
      api.post<ToolRefusal>(`/refusals/${v.id}/rule`, { rule: v.rule }),
    onSuccess: (f) => {
      if (f.rule_pr) toast(`Pull request opened for ${f.rule}`, { href: f.rule_pr })
      else if (f.rule) toast(f.rule_reason || `${f.rule} is already allowed`)
      else toast(f.rule_reason || 'The rule wasn’t added', { kind: 'error' })
    },
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['refusals'] })
      qc.invalidateQueries({ queryKey: ['request'] })
    },
    onError: toastError,
  })
}

export function useRunCell(ref: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { n: number; command: string }) =>
      api.post<WRequest>(`/requests/${enc(ref)}/cells/${v.n}/run`, { command: v.command }),
    onSuccess: (r) => qc.setQueryData<WRequest>(['request', ref], r),
    onError: (e) => (toastError(e), refresh(qc)),
  })
}

/** How a state reads: a word and a tone for its badge. */
export function stateLabel(
  r: Pick<WRequest, 'state' | 'kind' | 'via'> & { result?: RequestResult },
): {
  text: string
  tone: 'open' | 'ok' | 'bad' | 'muted' | 'busy'
} {
  switch (r.state) {
    case 'open':
      return { text: 'Waiting on you', tone: 'open' }
    case 'answered':
      return { text: r.via === 'terminal' ? 'Answered in the terminal' : 'Answered', tone: 'ok' }
    case 'approved':
      return { text: 'Approved', tone: 'ok' }
    case 'applying':
      return { text: 'Applying', tone: 'busy' }
    case 'applied':
      return r.result?.no_change
        ? { text: 'Changed nothing', tone: 'muted' }
        : { text: 'Applied', tone: 'ok' }
    case 'done':
      return { text: 'Done', tone: 'ok' }
    case 'denied':
      return { text: 'Denied', tone: 'bad' }
    case 'failed':
      return { text: 'Failed', tone: 'bad' }
    case 'conflict':
      return { text: 'Conflict', tone: 'bad' }
    case 'dismissed':
      return { text: 'Dismissed', tone: 'muted' }
    case 'expired':
      return { text: 'Expired', tone: 'muted' }
    default:
      return { text: 'Withdrawn', tone: 'muted' }
  }
}

/** A request waiting longer than this reads as old: the agent has likely moved on. */
export const OLD_REQUEST_MS = 24 * 3600 * 1000

export function isOldRequest(r: Pick<WRequest, 'created_at'>, now = Date.now()): boolean {
  return now - new Date(r.created_at).getTime() > OLD_REQUEST_MS
}

export const kindLabel: Record<RequestKind, string> = {
  question: 'Question',
  change: 'File change',
  command: 'Command',
  permission: 'Permission',
  action: 'To do',
}
