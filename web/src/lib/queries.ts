import {
  infiniteQueryOptions,
  queryOptions,
  useMutation,
  useQuery,
  useQueryClient,
  type FetchQueryOptions,
  type QueryClient,
  type QueryKey,
} from '@tanstack/react-query'
import { useEffect, useMemo } from 'react'
import { api, enc, qs } from './api'
import { splitLongTitle } from './quickadd'
import { tracked } from './drafts'
import { moveProject, projectTree } from './agents'
import { usageParams, type UsageFilters, type UsageLedger } from './usage-analytics'
import type { ExperimentResults, Experiments, ExperimentStartInput } from './experiments'
import type {
  RelationSuggestion,
  RemoteBranches,
  APIToken,
  TokenPermissionCatalog,
  Automation,
  AutomationTemplate,
  Backlog,
  BotRun,
  Board,
  Burndown,
  Comment,
  OutsideText,
  Objective,
  ObjectiveAction,
  ObjectiveInput,
  DiscoveredRepo,
  EpicProgress,
  FlowDay,
  Forecast,
  Forge,
  Issue,
  IssueDetail,
  InboxSummary,
  InboxView,
  Page,
  PendingInvite,
  Project,
  ProjectRepo,
  SearchResult,
  Session,
  Sprint,
  TimeEntry,
  WorkLog,
  GraphPath,
  GraphView,
  TimeReport,
  User,
  UserPrefs,
  UserRef,
  VelocityEntry,
  View,
  WEvent,
  Webhook,
  WebhookDelivery,
  OrgHook,
  WeekStat,
} from './types'
import type { FollowUp } from './sessions'
import type { KitCheckout, KitInfo, KitSource } from './kit'
import type { AgentFileContent, AgentSetup } from './agent-setup'
import type { EmailSettings } from './email'
import type { ShipReply } from './ship-reply'
import type { AutopilotText } from './autopilot-text'
import type { StallPolicies } from './stalled-jobs'
import type { SandboxChange, SandboxKey, SandboxMachine, SandboxPolicy } from './sandbox'
import type { CIBoard, CIHistory, CIHistoryBucket, CIPoolRunner } from './ci'
import type { Delivery } from './delivery'
import type { CatchUp } from './catch-up'
import type { Dashboard, DashboardAll } from './dashboard'
import type {
  AutopilotOverview,
  LimitsInput,
  ReviewModelOptions,
  TaskSummaryPage,
} from './autopilot'

const MIN = 60_000

/** Where a task session for a project can start: one of your runners and the checkout you last worked in on its machine. */
export interface TaskSessionPlace {
  project_key: string
  runner_id: number
  runner: string
  online: boolean
  dir: string
}

/** What the queue's Start button queued: a session job on that runner. */
export interface TaskSessionStart {
  key: string
  job: { id: number; state: string }
  place: TaskSessionPlace
}

export const q = {
  me: () =>
    queryOptions({
      queryKey: ['me'],
      queryFn: () => api.get<User>('/me'),
      staleTime: 5 * MIN,
      retry: false,
    }),
  users: () =>
    queryOptions({
      queryKey: ['users'],
      queryFn: () => api.get<UserRef[]>('/users'),
      staleTime: 10 * MIN,
    }),
  projects: () =>
    queryOptions({
      queryKey: ['projects'],
      queryFn: () => api.get<Project[]>('/projects'),
      staleTime: MIN,
    }),
  project: (key: string) =>
    queryOptions({
      queryKey: ['project', key],
      queryFn: () => api.get<Project>(`/projects/${enc(key)}`),
      staleTime: MIN,
    }),
  board: (key: string, filter = '') =>
    queryOptions({
      queryKey: ['board', key, filter],
      queryFn: () => api.get<Board>(`/projects/${enc(key)}/board${qs({ q: filter })}`),
    }),
  backlog: (key: string, filter = '') =>
    queryOptions({
      queryKey: ['backlog', key, filter],
      queryFn: () => api.get<Backlog>(`/projects/${enc(key)}/backlog${qs({ q: filter })}`),
    }),
  activity: (key: string) =>
    queryOptions({
      queryKey: ['activity', key],
      queryFn: () => api.get<WEvent[]>(`/projects/${enc(key)}/activity?limit=50`),
    }),
  issue: (key: string) =>
    queryOptions({
      queryKey: ['issue', key],
      queryFn: () => api.get<IssueDetail>(`/issues/${enc(key)}`),
    }),
  comments: (key: string) =>
    queryOptions({
      queryKey: ['comments', key],
      queryFn: () => api.get<Comment[]>(`/issues/${enc(key)}/comments`),
    }),
  /** text from outside waiting on an issue (WP-279) */
  outside: (key: string) =>
    queryOptions({
      queryKey: ['outside', key],
      queryFn: () => api.get<OutsideText[]>(`/issues/${enc(key)}/outside`),
    }),
  history: (key: string) =>
    queryOptions({
      queryKey: ['history', key],
      queryFn: () => api.get<WEvent[]>(`/issues/${enc(key)}/history`),
    }),
  issueTime: (key: string) =>
    queryOptions({
      queryKey: ['issueTime', key],
      queryFn: () => api.get<TimeEntry[]>(`/issues/${enc(key)}/time`),
    }),
  /** A task's work log (WP-285). */
  workLog: (key: string) =>
    queryOptions({
      queryKey: ['workLog', key],
      queryFn: () => api.get<WorkLog>(`/issues/${enc(key)}/worklog`),
    }),
  /** What lies around a thing in the relation graph, nearest first (WP-385). */
  graph: (
    ref: string,
    opts: { hops?: number; guesses?: boolean; open?: boolean; limit?: number } = {},
  ) =>
    queryOptions({
      queryKey: ['graph', ref, opts],
      queryFn: () =>
        api.get<GraphView>(
          `/graph${qs({ ref, hops: opts.hops, limit: opts.limit, open: opts.open ? 1 : undefined, guesses: opts.guesses === false ? 0 : undefined })}`,
        ),
      staleTime: 30_000,
    }),
  /** The nearest route between two things (WP-385). */
  graphPath: (from: string, to: string, guesses = true) =>
    queryOptions({
      queryKey: ['graphPath', from, to, guesses],
      queryFn: () =>
        api.get<GraphPath>(`/graph/path${qs({ from, to, guesses: guesses ? undefined : 0 })}`),
      staleTime: 30_000,
    }),
  search: (
    query: string,
    /** near: the project in view; its issues rank first when words are searched (WP-610) */
    opts: { project?: string; limit?: number; order?: 'rank' | 'updated'; near?: string } = {},
  ) =>
    queryOptions({
      queryKey: ['search', query, opts],
      queryFn: () =>
        api.get<SearchResult>(
          `/issues${qs({ q: query, project: opts.project, limit: opts.limit ?? 200, default_order: opts.order, near: opts.near })}`,
        ),
    }),
  timer: () =>
    queryOptions({
      queryKey: ['timer'],
      queryFn: () => api.get<{ timer: TimeEntry | null }>('/timer').then((r) => r.timer),
    }),
  time: (from: string, to: string) =>
    queryOptions({
      queryKey: ['time', from, to],
      queryFn: () => api.get<TimeEntry[]>(`/time${qs({ from, to })}`),
    }),
  inbox: () =>
    queryOptions({
      queryKey: ['notifications', 'inbox'],
      queryFn: () => api.get<InboxView>('/inbox'),
    }),
  inboxCounts: () =>
    queryOptions({
      queryKey: ['notifications', 'inbox-counts'],
      queryFn: () => api.get<InboxSummary>('/inbox/counts'),
    }),
  tags: () =>
    queryOptions({
      queryKey: ['tags'],
      queryFn: () => api.get<{ name: string; color: string; count: number }[]>('/tags'),
      staleTime: 5 * MIN,
    }),
  views: () =>
    queryOptions({
      queryKey: ['views'],
      queryFn: () => api.get<View[]>('/views'),
      staleTime: 5 * MIN,
    }),
  sprints: (key: string, closed = false) =>
    queryOptions({
      queryKey: ['sprints', key, closed],
      queryFn: () => api.get<Sprint[]>(`/projects/${enc(key)}/sprints${qs({ closed })}`),
    }),
  automations: (key: string) =>
    queryOptions({
      queryKey: ['automations', key],
      queryFn: () => api.get<Automation[]>(`/projects/${enc(key)}/automations`),
    }),
  /** A project's recent bot runs, newest first, narrowed by status and rule; polled while any is still going. */
  botRuns: (
    key: string,
    f: { status?: 'live' | 'failed' | 'done'; automation?: number; limit?: number } = {},
  ) =>
    queryOptions({
      queryKey: ['botRuns', key, f],
      queryFn: () => api.get<BotRun[]>(`/projects/${enc(key)}/bot-runs${qs(f)}`),
      refetchInterval: (query) =>
        query.state.data?.some((r) => r.status === 'queued' || r.status === 'running')
          ? 10_000
          : false,
    }),
  automationTemplates: () =>
    queryOptions({
      queryKey: ['automationTemplates'],
      queryFn: () =>
        api.get<{
          templates: AutomationTemplate[]
          triggers: string[]
          actions: Record<string, string>
        }>('/automation-templates'),
      staleTime: Infinity,
    }),
  webhooks: (key: string) =>
    queryOptions({
      queryKey: ['webhooks', key],
      queryFn: () => api.get<Webhook[]>(`/projects/${enc(key)}/webhooks`),
    }),
  globalWebhooks: () =>
    queryOptions({
      queryKey: ['webhooks', ''],
      queryFn: () => api.get<Webhook[]>(`/admin/webhooks`),
    }),
  /** Merged branches the forge sweep deletes, and stale ones (WP-140). */
  remoteBranches: (project: string) =>
    queryOptions({
      queryKey: ['branches', 'tidy', project],
      queryFn: () =>
        api.get<RemoteBranches[]>(`/branches/tidy${qs({ project: project || undefined })}`),
      staleTime: 60_000,
    }),
  /**
   * Objectives a person can see (WP-927): those in effect and proposals, or
   * with `all` the met and ended ones too; those that need a person first.
   */
  objectives: (opts: { project?: string; all?: boolean } = {}) =>
    queryOptions({
      queryKey: ['objectives', opts.project ?? '', !!opts.all],
      queryFn: () =>
        api.get<Objective[]>(
          `/objectives${qs({ project: opts.project || undefined, all: opts.all ? 1 : undefined })}`,
        ),
      refetchInterval: MIN,
    }),
  repos: (key: string) =>
    queryOptions({
      queryKey: ['repos', key],
      queryFn: () => api.get<ProjectRepo[]>(`/projects/${enc(key)}/repos`),
      refetchInterval: (query) =>
        query.state.data?.some(
          (r) =>
            r.hook_state === 'pending' ||
            (r.hook_state === 'installed' &&
              !r.hook_seen_at &&
              Date.now() - Date.parse(r.hook_checked_at ?? '') < 30_000),
        )
          ? 1500
          : false,
    }),
  forges: () => queryOptions({ queryKey: ['forges'], queryFn: () => api.get<Forge[]>('/forges') }),
  /** The agent kit (WP-ADR2): its plugins, notes, where each goes and its last read. */
  kit: () => queryOptions({ queryKey: ['kit'], queryFn: () => api.get<KitInfo>('/kit') }),
  /** Checkouts' last `wp kit sync` reports: yours, or everyone's for admins. */
  kitCheckouts: () =>
    queryOptions({
      queryKey: ['kit', 'checkouts'],
      queryFn: () => api.get<KitCheckout[]>('/kit/checkouts'),
    }),
  /** Agent setup (WP-ADR44): repos' and machines' agent files, the shared instructions, checks. */
  agentSetup: () =>
    queryOptions({
      queryKey: ['agentSetup'],
      queryFn: () => api.get<AgentSetup>('/agent-setup'),
      staleTime: 30_000,
    }),
  /** One repo's agent file as last read, with its text. */
  agentFile: (forge: number, repo: string, path: string) =>
    queryOptions({
      queryKey: ['agentSetup', 'file', forge, repo, path],
      queryFn: () => api.get<AgentFileContent>('/agent-setup/file' + qs({ forge, repo, path })),
    }),
  /** One machine's reported agent file. */
  machineFile: (user: number, machine: string, path: string) =>
    queryOptions({
      queryKey: ['agentSetup', 'machineFile', user, machine, path],
      queryFn: () =>
        api.get<AgentFileContent>('/agent-setup/machine-file' + qs({ user, machine, path })),
    }),
  /** Admins: which attached repo is the kit, and the ones it could be. */
  kitSource: () =>
    queryOptions({ queryKey: ['kit', 'source'], queryFn: () => api.get<KitSource>('/admin/kit') }),
  /** Admins: the SMTP settings, without the password. */
  emailSettings: () =>
    queryOptions({
      queryKey: ['admin', 'email'],
      queryFn: () => api.get<EmailSettings>('/admin/email'),
    }),
  /** Admins: the template wp ship gives an agent for its last message. */
  shipReply: () =>
    queryOptions({
      queryKey: ['admin', 'ship-reply'],
      queryFn: () => api.get<ShipReply>('/admin/ship-reply'),
    }),
  /** Autopilot's wording for every project (WP-308). */
  autopilotText: () =>
    queryOptions({
      queryKey: ['autopilot', 'text'],
      queryFn: () => api.get<AutopilotText>('/autopilot/text'),
    }),
  /** Who can review a round, and the reviewer's default brief (WP-1320). */
  reviewModels: () =>
    queryOptions({
      queryKey: ['autopilot', 'review-models'],
      queryFn: () => api.get<ReviewModelOptions>('/autopilot/review-models'),
      staleTime: 5 * MIN,
    }),
  /** Autopilot's wording as a project uses it, with what it inherits. */
  projectAutopilotText: (key: string) =>
    queryOptions({
      queryKey: ['autopilot', 'text', key],
      queryFn: () => api.get<AutopilotText>(`/projects/${enc(key)}/autopilot-text`),
    }),
  /** When sessions are told of stalled background jobs, for every project (WP-1045). */
  stallPolicy: () =>
    queryOptions({
      queryKey: ['stalled-jobs', 'policy'],
      queryFn: () => api.get<StallPolicies>('/stalled-jobs/policy'),
    }),
  /** The same as a project uses it, with what it inherits. */
  projectStallPolicy: (key: string) =>
    queryOptions({
      queryKey: ['stalled-jobs', 'policy', key],
      queryFn: () => api.get<StallPolicies>(`/projects/${enc(key)}/stalled-jobs`),
    }),
  /** A project's sandbox levels (WP-418). */
  projectSandbox: (key: string) =>
    queryOptions({
      queryKey: ['sandbox', 'project', key],
      queryFn: () => api.get<SandboxPolicy>(`/projects/${enc(key)}/sandbox`),
    }),
  /** The machines you can see (an admin: all): default level, kill switch. */
  sandboxMachines: () =>
    queryOptions({
      queryKey: ['sandbox', 'machines'],
      queryFn: () => api.get<SandboxMachine[]>('/sandbox/machines'),
      refetchInterval: (query) =>
        query.state.data?.some((m) =>
          m.ops?.some((o) => o.state === 'waiting' || o.state === 'sent'),
        )
          ? 5000
          : false,
    }),
  /** The key machines' sandbox helpers pin (WP-418). */
  sandboxKey: () =>
    queryOptions({
      queryKey: ['sandbox', 'key'],
      queryFn: () => api.get<SandboxKey>('/sandbox/key'),
      staleTime: Infinity,
    }),
  /** Sandbox changes, newest first: of a project, a runner, or all you can see. */
  sandboxLog: (f: { project?: string; runner?: number; limit?: number } = {}) =>
    queryOptions({
      queryKey: ['sandbox', 'log', f],
      queryFn: () => api.get<SandboxChange[]>('/sandbox/log' + qs(f)),
    }),
  orgHooks: (forgeId: number) =>
    queryOptions({
      queryKey: ['orgHooks', forgeId],
      queryFn: () => api.get<OrgHook[]>(`/forges/${forgeId}/org-hooks`),
      refetchInterval: (query) =>
        query.state.data?.some(
          (h) =>
            h.state === 'pending' ||
            (h.state === 'installed' &&
              !h.seen_at &&
              Date.now() - Date.parse(h.checked_at ?? '') < 30_000),
        )
          ? 1500
          : false,
    }),
  /** Organisations the forge's token can see; asks the forge, so only on demand */
  discoverPreview: (forgeId: number) =>
    queryOptions({
      queryKey: ['discoverPreview', forgeId],
      queryFn: () => api.get<DiscoveredRepo[]>(`/forges/${forgeId}/discover`),
      staleTime: 0,
      retry: false,
    }),
  forgeOrgs: (forgeId: number) =>
    queryOptions({
      queryKey: ['forgeOrgs', forgeId],
      queryFn: () => api.get<string[]>(`/forges/${forgeId}/orgs`),
      staleTime: 60_000,
      retry: false,
    }),
  webhookDeliveries: (id: number) =>
    queryOptions({
      queryKey: ['webhookDeliveries', id],
      queryFn: () => api.get<WebhookDelivery[]>(`/webhooks/${id}/deliveries`),
    }),
  velocity: (key: string) =>
    queryOptions({
      queryKey: ['report', key, 'velocity'],
      queryFn: () => api.get<VelocityEntry[]>(`/projects/${enc(key)}/reports/velocity`),
    }),
  dashboard: (key: string) =>
    queryOptions({
      queryKey: ['dashboard', key],
      queryFn: () => api.get<Dashboard>(`/projects/${enc(key)}/dashboard`),
    }),
  dashboards: () =>
    queryOptions({
      queryKey: ['dashboard'],
      queryFn: () => api.get<DashboardAll>('/dashboard'),
    }),
  throughput: (key: string, weeks = 12) =>
    queryOptions({
      queryKey: ['report', key, 'throughput', weeks],
      queryFn: () => api.get<WeekStat[]>(`/projects/${enc(key)}/reports/throughput?weeks=${weeks}`),
    }),
  flow: (key: string, days = 30) =>
    queryOptions({
      queryKey: ['report', key, 'flow', days],
      queryFn: () => api.get<FlowDay[]>(`/projects/${enc(key)}/reports/flow?days=${days}`),
    }),
  forecast: (key: string, scope = '') =>
    queryOptions({
      queryKey: ['report', key, 'forecast', scope],
      queryFn: () => api.get<Forecast>(`/projects/${enc(key)}/reports/forecast${qs({ q: scope })}`),
    }),
  roadmap: (key: string) =>
    queryOptions({
      queryKey: ['report', key, 'roadmap'],
      queryFn: () => api.get<EpicProgress[]>(`/projects/${enc(key)}/reports/roadmap`),
    }),
  burndown: (sprintId: number) =>
    queryOptions({
      queryKey: ['burndown', sprintId],
      queryFn: () => api.get<Burndown>(`/sprints/${sprintId}/burndown`),
    }),
  timeReport: (from: string, to: string, project = '') =>
    queryOptions({
      queryKey: ['timeReport', from, to, project],
      queryFn: () => api.get<TimeReport>(`/reports/time${qs({ from, to, project })}`),
    }),
  tokens: () =>
    queryOptions({ queryKey: ['tokens'], queryFn: () => api.get<APIToken[]>('/me/tokens') }),
  /** the permissions a token can be limited to; static */
  tokenPermissions: () =>
    queryOptions({
      queryKey: ['tokenPermissions'],
      queryFn: () => api.get<TokenPermissionCatalog>('/me/token-permissions'),
      staleTime: 60 * MIN,
    }),
  sessions: () =>
    queryOptions({ queryKey: ['sessions'], queryFn: () => api.get<Session[]>('/me/sessions') }),
  /** an agent session's follow-up: steps for when it finishes (WP-D22) */
  followUp: (session: number) =>
    queryOptions({
      queryKey: ['followUp', session],
      queryFn: () => api.get<FollowUp>(`/sessions/${session}/follow-up`),
    }),
  calendar: () =>
    queryOptions({
      queryKey: ['calendar'],
      queryFn: () => api.get<{ url: string }>('/me/calendar'),
    }),
  /** per project, where your runners can start a task session (WP-71) */
  taskSessionPlaces: () =>
    queryOptions({
      queryKey: ['taskSessionPlaces'],
      queryFn: () => api.get<TaskSessionPlace[]>('/agents/session-places'),
      staleTime: MIN,
    }),
  adminUsers: () =>
    queryOptions({ queryKey: ['adminUsers'], queryFn: () => api.get<User[]>('/admin/users') }),
  invites: () =>
    queryOptions({
      queryKey: ['invites'],
      queryFn: () => api.get<PendingInvite[]>('/admin/invites'),
    }),
}

/** Page queries shared by route loaders (hover preloads) and the pages themselves. */
export const pageQ = {
  homeOpen: () => q.search('assignee:me is:open order:-priority,due', { limit: 300 }),
  homeDone: () => q.search('assignee:me is:done resolved:>=-7d order:-resolved', { limit: 50 }),
  homeStale: () => ({ ...q.search(STALE_QUERY, { limit: 1 }), staleTime: 5 * MIN }),
  list: (key: string, filter = '', showDone = false) =>
    q.search([filter, showDone ? '' : 'is:open'].filter(Boolean).join(' '), {
      project: key,
      limit: 1000,
      order: 'rank',
    }),
  timeline: (key: string, filter = '') =>
    q.search(`is:open ${filter}`.trim(), { project: key, limit: 1000, order: 'rank' }),
  search: (query: string) => q.search(query, { limit: 300, order: 'updated' }),
}

/** Open work nobody has touched in a month (the "coming back after a break" nudge). */
export const STALE_QUERY = 'is:open updated:<-30d order:updated'

type IssueLike = Issue | IssueDetail

/** Apply f to every cached copy of an issue (detail, boards, lists). */
export function patchIssueEverywhere(qc: QueryClient, key: string, f: (i: IssueLike) => IssueLike) {
  qc.setQueryData<IssueDetail>(['issue', key], (d) => (d ? (f(d) as IssueDetail) : d))
  const mapList = (list: Issue[]) => list.map((i) => (i.key === key ? (f(i) as Issue) : i))
  qc.setQueriesData<Board>({ queryKey: ['board'] }, (b) =>
    b ? { ...b, columns: b.columns.map((c) => ({ ...c, issues: mapList(c.issues) })) } : b,
  )
  qc.setQueriesData<Backlog>({ queryKey: ['backlog'] }, (b) =>
    b
      ? {
          ...b,
          backlog: mapList(b.backlog),
          epics: mapList(b.epics),
          sprints: b.sprints.map((s) => ({ ...s, issues: mapList(s.issues) })),
        }
      : b,
  )
  qc.setQueriesData<SearchResult>({ queryKey: ['search'] }, (r) =>
    r ? { ...r, issues: mapList(r.issues) } : r,
  )
  qc.setQueriesData<IssueDetail>({ queryKey: ['issue'] }, (d) =>
    d && d.children ? { ...d, children: mapList(d.children) } : d,
  )
}

/** Refetch everything that may show an issue in a project. */
export function invalidateProject(qc: QueryClient, projectKey?: string) {
  const keys: QueryKey[] = [['search'], ['issue']]
  if (projectKey) {
    keys.push(
      ['board', projectKey],
      ['backlog', projectKey],
      ['project', projectKey],
      ['activity', projectKey],
      ['report', projectKey],
    )
  } else {
    keys.push(['board'], ['backlog'], ['project'])
  }
  keys.push(['projects'])
  for (const k of keys) qc.invalidateQueries({ queryKey: k })
}

export interface IssuePatch {
  title?: string
  description?: string
  /** what it's for (WP-1196); a person sending an agent's text keeps it */
  intent?: string
  type?: string
  status?: string
  priority?: string | number
  parent?: string | null
  sprint?: string | null
  points?: number | null
  start_date?: string | null
  due_date?: string | null
  assignees?: string[]
  add_assignees?: string[]
  remove_assignees?: string[]
  labels?: string[]
  add_labels?: string[]
  remove_labels?: string[]
  after?: string
  before?: string
  /** agent spec: '' anyone, 'none' humans only, 'any', or a family/model */
  agent?: string
  category?: string
  /** workflow name or id; null takes the issue off its workflow */
  workflow?: string | null
  step?: string
  /** saved with the change, in the same transaction */
  comment?: string
}

export interface IssueInput {
  project?: string
  type?: string
  title: string
  description?: string
  status?: string
  priority?: string | number
  parent?: string
  sprint?: string
  assignees?: string[]
  labels?: string[]
  points?: number | null
  start_date?: string
  due_date?: string
  after?: string
  before?: string
  /** an issue template name: fills in what's empty and adds its sub-tasks */
  template?: string
  agent?: string
  category?: string
  workflow?: string
  /** a draft: agents can't see it until a person publishes it (WP-42) */
  draft?: boolean
  /** the title is text typed where a title goes: a model may write a short one (WP-67) */
  title_is_text?: boolean
}

export function useUpdateIssue() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({
      key,
      patch,
    }: {
      key: string
      patch: IssuePatch
      optimistic?: Partial<Issue>
      projectKey?: string
    }) => tracked(api.patch<IssueDetail>(`/issues/${enc(key)}`, patch)),
    onMutate: async ({ key, optimistic }) => {
      if (!optimistic) return
      await qc.cancelQueries({ queryKey: ['issue', key] })
      patchIssueEverywhere(qc, key, (i) => ({ ...i, ...optimistic }))
    },
    onSuccess: (d) => {
      qc.setQueryData(['issue', d.key], d)
      patchIssueEverywhere(qc, d.key, (i) => ({ ...i, ...stripDetail(d) }))
    },
    onSettled: (_d, _e, v) => {
      invalidateProject(qc, v.projectKey)
      qc.invalidateQueries({ queryKey: ['history', v.key] })
    },
  })
}

function stripDetail(d: IssueDetail): Issue {
  const {
    role: _r,
    ancestors: _a,
    children: _c,
    relations: _rel,
    links: _l,
    watchers: _w,
    shares: _s,
    watching: _wa,
    time_spent: _t,
    ...rest
  } = d
  return rest
}

export function useCreateIssue() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (in_: IssueInput) => {
      const { title, description } = splitLongTitle(in_.title)
      const body = description
        ? {
            ...in_,
            title,
            title_is_text: true,
            description: in_.description?.trim()
              ? `${description}\n\n${in_.description}`
              : description,
          }
        : in_
      return tracked(api.post<IssueDetail>('/issues', body))
    },
    onSuccess: (d) => {
      qc.setQueryData(['issue', d.key], d)
      invalidateProject(qc, d.project_key)
    },
  })
}

/** Publishes a draft and the drafts under it, so agents can see them (WP-42). */
export function usePublishIssue() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (key: string) => api.post<IssueDetail>(`/issues/${enc(key)}/publish`),
    onSuccess: (d) => {
      qc.setQueryData(['issue', d.key], d)
      qc.invalidateQueries({ queryKey: ['issue'] })
      invalidateProject(qc, d.project_key)
    },
  })
}

/** Puts an issue on hold, changes its hold, or (null) lifts it (WP-269). */
export function useHoldIssue() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({
      key,
      hold,
    }: {
      key: string
      hold: { until?: string; note?: string; keep_waits?: boolean } | null
    }) =>
      hold
        ? api.put<IssueDetail>(`/issues/${enc(key)}/hold`, hold)
        : api.del<IssueDetail>(`/issues/${enc(key)}/hold`),
    onSuccess: (d) => {
      qc.setQueryData(['issue', d.key], d)
      qc.invalidateQueries({ queryKey: ['agents'] })
      invalidateProject(qc, d.project_key)
    },
  })
}

/**
 * Sets, changes, confirms (an agent's proposal) or clears a task's
 * objective (WP-927).
 */
export function useObjective(key: string) {
  const qc = useQueryClient()
  const path = `/issues/${enc(key)}/objective`
  return useMutation({
    mutationFn: (
      v: { set: ObjectiveInput } | { confirm: string } | { clear: true },
    ): Promise<Objective | void> =>
      'set' in v
        ? api.put<Objective>(path, v.set)
        : 'confirm' in v
          ? api.post<Objective>(path + '/confirm', { rev: v.confirm })
          : api.del<void>(path),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['issue', key] })
      qc.invalidateQueries({ queryKey: ['objectives'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
    },
  })
}

/**
 * A person steering a task's objective in effect (WP-927): read again,
 * move on to the next round, accept a reading whose check files changed, or end it.
 */
export function useObjectiveGo(key: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (action: ObjectiveAction) =>
      api.post<Objective>(`/issues/${enc(key)}/objective/go`, { action }),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['issue', key] })
      qc.invalidateQueries({ queryKey: ['objectives'] })
      qc.invalidateQueries({ queryKey: ['agents'] })
    },
  })
}

/** Approves or discards text from outside (WP-279). */
export function useDecideOutside(issueKey: string, projectKey: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { id: number; approve: boolean }) =>
      api.post(`/outside/${v.id}/${v.approve ? 'approve' : 'discard'}`),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ['outside', issueKey] })
      qc.invalidateQueries({ queryKey: ['comments', issueKey] })
      qc.invalidateQueries({ queryKey: ['issue', issueKey] })
      qc.invalidateQueries({ queryKey: ['waiting'] })
      invalidateProject(qc, projectKey)
    },
  })
}

/** Starts a fresh Claude Code session for a task on your runner, as wp start KEY --bg would (WP-71). */
export function useStartTaskSession() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (key: string) => api.post<TaskSessionStart>(`/agents/start/${enc(key)}`),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['runnerJobs'] }),
  })
}

/** Publishes a draft page and the drafts under it (WP-42). */
export function usePublishPage() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api.post<Page>(`/pages/${id}/publish`),
    onSuccess: (p) => {
      qc.setQueryData(['page', p.id], p)
      qc.invalidateQueries({ queryKey: ['pages'] })
      qc.invalidateQueries({ queryKey: ['page'] })
    },
  })
}

/** Has the story model write a page's user story now (WP-190). */
export function useSuggestStory() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api.post<Page>(`/pages/${id}/story`),
    onSuccess: (p) => qc.setQueryData(['page', p.id], p),
  })
}

export function useDeleteIssue() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ key, cascade }: { key: string; cascade?: boolean; projectKey?: string }) =>
      api.del(`/issues/${enc(key)}${qs({ cascade })}`),
    onSuccess: (_d, v) => {
      qc.removeQueries({ queryKey: ['issue', v.key] })
      invalidateProject(qc, v.projectKey)
    },
  })
}

export function useBulkUpdate() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: {
      issues: string[]
      patch: IssuePatch
      descendants?: boolean
      projectKey?: string
    }) =>
      api.post<{ updated: string[] }>('/issues/bulk', {
        issues: v.issues,
        patch: v.patch,
        descendants: v.descendants ?? false,
      }),
    onSettled: (_d, _e, v) => invalidateProject(qc, v.projectKey),
  })
}

/** Marks agents' closes of these issues reviewed (WP-272). */
export function useReviewCloses() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { issues: string[]; projectKey?: string }) =>
      api.post<{ reviewed: number }>('/review/closes', { issues: v.issues }),
    onSettled: (_d, _e, v) => {
      invalidateProject(qc, v.projectKey)
      qc.invalidateQueries({ queryKey: ['waiting'] })
      qc.invalidateQueries({ queryKey: ['catch-up'] })
    },
  })
}

/**
 * Catch up (WP-245): the closes agents made that no person has reviewed,
 * oldest first, with each one's summary, screenshots and where to try it.
 */
export const catchUpQuery = (project?: string) =>
  queryOptions({
    queryKey: ['catch-up', project ?? ''],
    queryFn: () => api.get<CatchUp>(`/catch-up${qs({ project })}`),
    staleTime: 30_000,
  })

/** Sends a finished task back: it reopens with the person's note (WP-245). */
export function useNotFinished() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { key: string; note: string; projectKey?: string }) =>
      api.post<IssueDetail>(`/issues/${enc(v.key)}/not-finished`, { note: v.note }),
    onSettled: (_d, _e, v) => {
      invalidateProject(qc, v.projectKey)
      qc.invalidateQueries({ queryKey: ['waiting'] })
      qc.invalidateQueries({ queryKey: ['catch-up'] })
    },
  })
}

/** Files a bug about a finished task, related to it (WP-245). */
export function useReportProblem() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { key: string; note: string; projectKey?: string }) =>
      api.post<IssueDetail>(`/issues/${enc(v.key)}/problems`, { note: v.note }),
    onSettled: (_d, _e, v) => invalidateProject(qc, v.projectKey),
  })
}

export function useTimer() {
  const qc = useQueryClient()
  const done = () => {
    qc.invalidateQueries({ queryKey: ['timer'] })
    qc.invalidateQueries({ queryKey: ['time'] })
    qc.invalidateQueries({ queryKey: ['issueTime'] })
    qc.invalidateQueries({ queryKey: ['timeReport'] })
    qc.invalidateQueries({ queryKey: ['issue'] })
  }
  const start = useMutation({
    mutationFn: (v: { issue?: string; note?: string }) =>
      api.post<{ timer: TimeEntry }>('/timer/start', v).then((r) => r.timer),
    onSuccess: (t) => qc.setQueryData(['timer'], t),
    onSettled: done,
  })
  const stop = useMutation({
    mutationFn: () => api.post('/timer/stop'),
    onMutate: () => qc.setQueryData(['timer'], null),
    onSettled: done,
  })
  return { start, stop }
}

// The API replaces prefs wholesale, so saves run one at a time, each merging
// its change into the latest copy: two quick toggles can't undo each other.
let prefsChain: Promise<unknown> = Promise.resolve()

export function useSavePrefs() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (patch: Partial<UserPrefs>) => {
      qc.setQueryData<User>(['me'], (u) => (u ? { ...u, prefs: { ...u.prefs, ...patch } } : u))
      const run = prefsChain.then(() => {
        const cur = qc.getQueryData<User>(['me'])?.prefs
        return api.patch<User>('/me', { prefs: { ...cur, ...patch } })
      })
      prefsChain = run.catch(() => {})
      return run
    },
    onSuccess: (u) =>
      qc.setQueryData<User>(['me'], (old) =>
        old ? { ...u, prefs: { ...u.prefs, ...old.prefs } } : u,
      ),
    onError: () => qc.invalidateQueries({ queryKey: ['me'] }),
  })
}

export type ProjectSort = 'recent' | 'custom'

/**
 * The sidebar's projects in this person's order, with the controls: pick
 * recent or custom, and drop a project where another sits among its siblings.
 * The choice rides on the profile's prefs, so it follows them across devices,
 * and applies at once while it saves.
 */
export function useProjectOrder() {
  const projects = useQuery(q.projects()).data
  const prefs = useQuery(q.me()).data?.prefs
  const save = useSavePrefs()
  const order = prefs?.project_order
  const sort: ProjectSort = prefs?.project_sort || (order?.length ? 'custom' : 'recent')
  const tree = useMemo(() => projectTree(projects ?? [], sort, order), [projects, sort, order])
  return {
    sort,
    tree,
    setSort: (s: ProjectSort) => save.mutate({ project_sort: s }),
    /** Moving a project makes the order custom. */
    move: (id: number, over: number) => {
      const next = moveProject(tree, id, over)
      if (next) save.mutate({ project_sort: 'custom', project_order: next })
    },
  }
}

/**
 * Warms a query once the page has settled (browser idle), so a picker that
 * needs it opens with its options already there.
 */
export function usePrefetch<T, K extends QueryKey>(
  options: FetchQueryOptions<T, Error, T, K>,
  enabled = true,
) {
  const qc = useQueryClient()
  const key = JSON.stringify(options.queryKey)
  useEffect(() => {
    if (!enabled) return
    const run = () => void qc.prefetchQuery(options)
    if ('requestIdleCallback' in window) {
      const id = window.requestIdleCallback(run, { timeout: 1500 })
      return () => window.cancelIdleCallback(id)
    }
    const t = setTimeout(run, 300)
    return () => clearTimeout(t)
  }, [qc, key, enabled]) // eslint-disable-line react-hooks/exhaustive-deps
}

/** The token ledger on Agents → Usage (WP-35). */
export const usageLedgerQuery = (f: UsageFilters) =>
  queryOptions({
    queryKey: [
      'usage',
      'ledger',
      f.agent,
      f.project,
      f.days,
      f.machine ?? '',
      f.session ?? '',
      f.who ?? '',
    ],
    queryFn: () => api.get<UsageLedger>(`/usage/ledger${qs(usageParams(f))}`),
    staleTime: 30_000,
    placeholderData: (prev) => prev,
  })

/**
 * Where recent work stands on its way to production (WP-389): every project
 * you see, or one. The server asks the forge which merges each deployed
 * commit has, and caches the answers.
 */
export const deliveryQuery = (project?: string) =>
  queryOptions({
    queryKey: ['delivery', project ?? ''],
    queryFn: () => api.get<Delivery>(`/delivery${qs({ project })}`),
    staleTime: 30_000,
    refetchInterval: 60_000,
    placeholderData: (prev) => prev,
  })

/** The CI board (WP-225): the server reads the forge at most every 15s. */
export const ciBoardQuery = () =>
  queryOptions({
    queryKey: ['ci'],
    queryFn: () => api.get<CIBoard>('/ci'),
    staleTime: 10_000,
    refetchInterval: 20_000,
    placeholderData: (prev) => prev,
  })

/**
 * CI over time (WP-490): the runs and jobs the server keeps, by day or week,
 * for every repo you can see or one.
 */
export const ciHistoryQuery = (days: number, bucket: CIHistoryBucket, repo?: string) =>
  queryOptions({
    queryKey: ['ci', 'history', days, bucket, repo ?? ''],
    queryFn: () =>
      api.get<CIHistory>(`/ci/history${qs({ days, bucket, repo: repo || undefined })}`),
    staleTime: 5 * 60_000,
    placeholderData: (prev) => prev,
  })

/** Sets the runners of a forge's pool, which the board's ETAs play over. */
export function useSetCIPool() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ forge, runners }: { forge: number; runners: CIPoolRunner[] }) =>
      api.put<void>(`/ci/pools/${forge}`, { runners }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['ci'] }),
  })
}

/** Autopilot (WP-194): your settings, what it is doing in each project, and the meters its limits read. */
export const autopilotQuery = () =>
  queryOptions({
    queryKey: ['autopilot'],
    queryFn: () => api.get<AutopilotOverview>('/autopilot'),
    staleTime: 10_000,
    placeholderData: (prev) => prev,
  })

const SUMMARIES = 15

/** The task summaries agents left (unread ones, or all), newest first, a page at a time. */
export const autopilotSummariesQuery = (f: { project?: string; all?: boolean }) =>
  infiniteQueryOptions({
    queryKey: ['autopilot', 'summaries', f.project ?? '', !!f.all],
    queryFn: ({ pageParam }) =>
      api.get<TaskSummaryPage>(
        `/autopilot/summaries${qs({
          project: f.project,
          all: f.all,
          before: pageParam || undefined,
          limit: SUMMARIES,
        })}`,
      ),
    initialPageParam: 0,
    getNextPageParam: (last) => (last.more ? last.summaries.at(-1)?.id : undefined),
    staleTime: 15_000,
    placeholderData: (prev) => prev,
  })

/** Saves your defaults (no project) or one project's switch and overrides; answers the whole overview. */
export function useSetAutopilot() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ project, ...body }: LimitsInput & { project?: string; enabled?: boolean }) =>
      api.put<AutopilotOverview>(
        project ? `/autopilot/projects/${enc(project)}` : '/autopilot',
        body,
      ),
    onSuccess: (o) => qc.setQueryData(['autopilot'], o),
    onSettled: () => qc.invalidateQueries({ queryKey: ['autopilot'] }),
  })
}

/** Turns autopilot off in every project at once; answers the whole overview. */
export function useTurnOffAutopilot() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => api.post<AutopilotOverview>('/autopilot/off'),
    onSuccess: (o) => qc.setQueryData(['autopilot'], o),
  })
}

/** Stops what autopilot was about to do with a session, or has it do it now. */
export function useAutopilotStep() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, action }: { id: number; action: 'stop' | 'now' }) =>
      api.post<void>(`/autopilot/steps/${id}/${action}`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['autopilot'] }),
  })
}

/** Has autopilot start the next task in one of your idle sessions now (WP-307). */
export function useStartAutopilotSession() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: number) => api.post<void>(`/autopilot/sessions/${id}/start`),
    onSettled: () => qc.invalidateQueries({ queryKey: ['autopilot'] }),
  })
}

/** Marks task summaries read (or unread), some by id or a whole project's. */
export function useReviewSummaries() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (b: { ids?: number[]; all?: boolean; project?: string; unread?: boolean }) =>
      api.post<{ updated: number }>('/autopilot/summaries/reviewed', b),
    onSettled: () => qc.invalidateQueries({ queryKey: ['autopilot'] }),
  })
}

/**
 * Open issues worth linking to this one; nothing is linked (WP-296). A
 * mutation, so the list holds still while relations are added from it.
 */
export function useSuggestRelations(key: string) {
  return useMutation({
    mutationFn: () =>
      api.get<{ suggestions: RelationSuggestion[] }>(`/issues/${enc(key)}/relations/suggested`),
  })
}

/** Admin → Experiments (WP-254): every experiment and its live run. */
export const experimentsQuery = () =>
  queryOptions({
    queryKey: ['experiments'],
    queryFn: () => api.get<Experiments>('/experiments'),
  })

/** One run's results; run 0 is the latest. */
export const experimentResultsQuery = (key: string, run: number, scope: 'done' | 'all') =>
  queryOptions({
    queryKey: ['experiments', key, 'results', run, scope],
    queryFn: () =>
      api.get<ExperimentResults>(
        `/experiments/${enc(key)}/results${qs({ run: run || undefined, scope })}`,
      ),
  })

export function useStartExperiment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { key: string; input: ExperimentStartInput }) =>
      api.post<unknown>(`/experiments/${enc(v.key)}/start`, v.input),
    onSettled: () => qc.invalidateQueries({ queryKey: ['experiments'] }),
  })
}

export function useStopExperiment() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { key: string; reason: string }) =>
      api.post<unknown>(`/experiments/${enc(v.key)}/stop`, { reason: v.reason }),
    onSettled: () => qc.invalidateQueries({ queryKey: ['experiments'] }),
  })
}
