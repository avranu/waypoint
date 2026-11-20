import {
  createRootRouteWithContext,
  createRoute,
  createRouter,
  lazyRouteComponent,
  Outlet,
  redirect,
} from '@tanstack/react-router'
import type { QueryClient } from '@tanstack/react-query'
import { catchUpQuery, ciBoardQuery, deliveryQuery, pageQ, q } from '@/lib/queries'
import { agentFilter } from '@/lib/agents'
import { pageQuery, pagesQuery } from '@/lib/pages'
import { runnerJobQuery } from '@/lib/runners'
import { codeAuditQuery } from '@/lib/code-audits'
import {
  designNoteQuery,
  designQuery,
  goalsQuery,
  designReviewQuery,
  glossaryEntryQuery,
  glossaryQuery,
  hasFilters,
  linksQuery,
} from '@/lib/knowledge'
import { envQuery, envsQuery } from '@/lib/environments'
import { last30Days } from '@/lib/format'
import { readLocal } from '@/lib/hooks'
import { weekRange } from '@/pages/time/util'
import { Toaster } from '@/components/ui/toast'
import { ErrorPage } from '@/pages/Error'
import { AppShell } from '@/components/AppShell'
import { InvitePage, LoginPage, ResetPage } from '@/pages/Auth'
import { HomePage } from '@/pages/Home'
import { ProjectLayout } from '@/pages/Project'
import { BoardPage } from '@/pages/Board'
import { BacklogPage } from '@/pages/Backlog'
import { ListPage } from '@/pages/List'
import { IssuePage } from '@/pages/IssuePage'
import { InboxPage } from '@/pages/Inbox'
import { requestQuery, waitingQuery } from '@/lib/requests'
import { SearchPage, ViewPage } from '@/pages/Search'
import { sessionQuery } from '@/lib/sessions'
import { validateAgentsSearch } from '@/pages/agents/search'
import { validateAssistantSearch } from '@/pages/assistant/search'
import { assistantQuery } from '@/lib/assistant'

export interface RouterContext {
  qc: QueryClient
}

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: () => (
    <>
      <Outlet />
      <Toaster />
    </>
  ),
})

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/login',
  validateSearch: (s: Record<string, unknown>): { next?: string } => ({
    next: typeof s.next === 'string' ? s.next : undefined,
  }),
  component: LoginPage,
})
const inviteRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/invite/$token',
  component: InvitePage,
})
const resetRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: '/reset/$token',
  component: ResetPage,
})

const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: 'app',
  beforeLoad: async ({ context, location }) => {
    try {
      await context.qc.ensureQueryData(q.me())
    } catch {
      throw redirect({
        to: '/login',
        search: { next: location.href === '/' ? undefined : location.href },
      })
    }
  },
  loader: ({ context }) => {
    context.qc.prefetchQuery(q.projects())
  },
  component: AppShell,
})

const homeRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/',
  loader: ({ context }) => {
    context.qc.prefetchQuery(pageQ.homeDone())
    context.qc.prefetchQuery(pageQ.homeStale())
    return context.qc.ensureQueryData(pageQ.homeOpen())
  },
  component: HomePage,
})

const inboxRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/inbox',
  loader: ({ context }) => {
    context.qc.prefetchQuery(waitingQuery())
    context.qc.prefetchQuery(q.inbox())
  },
  component: InboxPage,
})

const objectivesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/objectives',
  loader: ({ context }) => {
    context.qc.prefetchQuery(q.objectives())
  },
  component: lazyRouteComponent(() => import('@/pages/Objectives'), 'ObjectivesPage'),
})

const inboxRequestRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/requests/$code',
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(waitingQuery())
    context.qc.prefetchQuery(requestQuery(params.code))
  },
  component: lazyRouteComponent(() => import('@/pages/Request'), 'RequestPage'),
})

// Connections (WP-385): the relation graph around a thing.
const graphRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/graph',
  validateSearch: (
    s: Record<string, unknown>,
  ): { ref?: string; hops?: number; sure?: boolean; open?: boolean; to?: string } => {
    const hops = Number(s.hops)
    const on = (v: unknown) => (v === true || v === '1' || v === 1 ? true : undefined)
    return {
      ref: typeof s.ref === 'string' && s.ref ? s.ref : undefined,
      hops: hops >= 1 && hops <= 4 ? hops : undefined,
      sure: on(s.sure),
      open: on(s.open),
      to: typeof s.to === 'string' && s.to ? s.to : undefined,
    }
  },
  component: lazyRouteComponent(() => import('@/pages/Graph'), 'GraphPage'),
})

const searchRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/search',
  validateSearch: (s: Record<string, unknown>): { q?: string; mode?: 'all' | 'issues' } => ({
    q: typeof s.q === 'string' ? s.q : undefined,
    mode: s.mode === 'all' || s.mode === 'issues' ? s.mode : undefined,
  }),
  loaderDeps: ({ search }) => ({ q: search.q ?? '', mode: search.mode }),
  // Issues load before the page shows; everything else fills in after.
  loader: ({ context, deps }) => {
    const text = deps.q.trim()
    if (!text) return
    if ((deps.mode ?? (hasFilters(text) ? 'issues' : 'all')) === 'issues')
      return context.qc.ensureQueryData(pageQ.search(deps.q))
  },
  component: SearchPage,
})

const viewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/views/$id',
  loader: ({ context }) => context.qc.ensureQueryData(q.views()),
  component: ViewPage,
})

const timeRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/time',
  loader: async ({ context }) => {
    const me = await context.qc.ensureQueryData(q.me())
    const { fromISO, toISO } = weekRange(me.prefs.week_start ?? 1, 0)
    context.qc.prefetchQuery(q.timer())
    context.qc.prefetchQuery(q.timeReport(fromISO, toISO))
    return context.qc.ensureQueryData(q.time(fromISO, toISO))
  },
  component: lazyRouteComponent(() => import('@/pages/Time'), 'TimePage'),
})

const settingsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/settings',
  validateSearch: (s: Record<string, unknown>): { tab?: string } => ({
    tab: typeof s.tab === 'string' ? s.tab : undefined,
  }),
  component: lazyRouteComponent(() => import('@/pages/Settings'), 'SettingsPage'),
})

const runnerJobRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/runner-jobs/$id',
  loader: ({ context, params }) =>
    context.qc.ensureQueryData(runnerJobQuery(Number(params.id))).catch(() => null),
  component: lazyRouteComponent(() => import('@/pages/RunnerJob'), 'RunnerJobPage'),
})

const codeAuditRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/audits/$id',
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(codeAuditQuery(Number(params.id)))
  },
  component: lazyRouteComponent(() => import('@/pages/CodeAudit'), 'CodeAuditPage'),
})

const agentsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/agents',
  validateSearch: validateAgentsSearch,
  component: lazyRouteComponent(() => import('@/pages/Agents'), 'AgentsPage'),
})

const agentSessionRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/agents/sessions/$id',
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(sessionQuery(Number(params.id)))
  },
  component: lazyRouteComponent(() => import('@/pages/agents/SessionPage'), 'SessionPage'),
})

const assistantsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/assistant',
  component: lazyRouteComponent(() => import('@/pages/assistant/Assistant'), 'AssistantsIndexPage'),
})

const assistantRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/assistant/$id',
  validateSearch: validateAssistantSearch,
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(assistantQuery(Number(params.id)))
  },
  component: lazyRouteComponent(() => import('@/pages/assistant/Assistant'), 'AssistantPage'),
})

const environmentsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/environments',
  loader: ({ context }) => context.qc.prefetchQuery(envsQuery(undefined, false)),
  component: lazyRouteComponent(() => import('@/pages/Environments'), 'EnvironmentsPage'),
})

const liveRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/live',
  loader: ({ context }) => context.qc.prefetchQuery(deliveryQuery()),
  component: lazyRouteComponent(() => import('@/pages/Live'), 'LivePage'),
})

// Every project at a glance (WP-780).
const overviewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/overview',
  loader: ({ context }) => {
    context.qc.prefetchQuery(q.dashboards())
  },
  component: lazyRouteComponent(() => import('@/pages/Dashboard'), 'OverviewPage'),
})

const catchUpRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/catch-up',
  validateSearch: (s: Record<string, unknown>): { project?: string } => ({
    project: typeof s.project === 'string' ? s.project : undefined,
  }),
  loaderDeps: ({ search }) => ({ project: search.project }),
  loader: ({ context, deps }) => context.qc.prefetchQuery(catchUpQuery(deps.project)),
  component: lazyRouteComponent(() => import('@/pages/CatchUp'), 'CatchUpPage'),
})

const ciRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/ci',
  loader: ({ context }) => context.qc.prefetchQuery(ciBoardQuery()),
  component: lazyRouteComponent(() => import('@/pages/CI'), 'CIPage'),
})

const ciHistoryRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/ci/history',
  validateSearch: (
    s: Record<string, unknown>,
  ): { days?: number; bucket?: 'day' | 'week'; repo?: string } => ({
    days: typeof s.days === 'number' && [30, 90, 365].includes(s.days) ? s.days : undefined,
    bucket: s.bucket === 'day' || s.bucket === 'week' ? s.bucket : undefined,
    repo: typeof s.repo === 'string' && s.repo ? s.repo : undefined,
  }),
  component: lazyRouteComponent(() => import('@/pages/CIHistory'), 'CIHistoryPage'),
})

const adminRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/admin',
  validateSearch: (s: Record<string, unknown>): { tab?: string } => ({
    tab: typeof s.tab === 'string' ? s.tab : undefined,
  }),
  component: lazyRouteComponent(() => import('@/pages/Admin'), 'AdminPage'),
})

const issueRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/issue/$key',
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(q.comments(params.key))
    return context.qc.ensureQueryData(q.issue(params.key)).catch(() => null)
  },
  component: IssuePage,
})

export interface ProjectSearch {
  issue?: string
  q?: string
}
const projectSearch = (s: Record<string, unknown>): ProjectSearch => ({
  issue: typeof s.issue === 'string' ? s.issue : undefined,
  q: typeof s.q === 'string' && s.q ? s.q : undefined,
})

const projectRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/p/$key',
  loader: ({ context, params }) => context.qc.ensureQueryData(q.project(params.key)),
  component: ProjectLayout,
})

const projectIndexRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/',
  beforeLoad: ({ params }) => {
    throw redirect({ to: '/p/$key/overview', params })
  },
})

// A project at a glance (WP-780): goals, flow, pace, what may be wrong.
const projectOverviewRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/overview',
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(q.dashboard(params.key))
  },
  component: lazyRouteComponent(() => import('@/pages/Dashboard'), 'ProjectOverviewPage'),
})

const boardRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/board',
  validateSearch: projectSearch,
  loaderDeps: ({ search }) => ({ q: search.q ?? '' }),
  loader: ({ context, params, deps }) =>
    context.qc.ensureQueryData(q.board(params.key, agentFilter(params.key, deps.q))),
  component: BoardPage,
})

const backlogRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/backlog',
  validateSearch: projectSearch,
  loaderDeps: ({ search }) => ({ q: search.q ?? '' }),
  loader: ({ context, params, deps }) =>
    context.qc.ensureQueryData(q.backlog(params.key, agentFilter(params.key, deps.q))),
  component: BacklogPage,
})

const listRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/list',
  validateSearch: (
    s: Record<string, unknown>,
  ): ProjectSearch & { group?: string; done?: boolean } => ({
    ...projectSearch(s),
    group: typeof s.group === 'string' ? s.group : undefined,
    done: s.done === true || s.done === 'true' ? true : undefined,
  }),
  loaderDeps: ({ search }) => ({ q: search.q ?? '', done: !!search.done }),
  loader: ({ context, params, deps }) =>
    context.qc.ensureQueryData(pageQ.list(params.key, agentFilter(params.key, deps.q), deps.done)),
  component: ListPage,
})

const timelineRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/timeline',
  validateSearch: projectSearch,
  loaderDeps: ({ search }) => ({ q: search.q ?? '' }),
  loader: ({ context, params, deps }) =>
    context.qc.ensureQueryData(pageQ.timeline(params.key, agentFilter(params.key, deps.q))),
  component: lazyRouteComponent(() => import('@/pages/Timeline'), 'TimelinePage'),
})

const reportsRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/reports',
  // Charts load in parallel; start them all on hover without blocking the page.
  loader: ({ context, params }) => {
    const { qc } = context
    const k = params.key
    for (const o of [
      q.velocity(k),
      q.roadmap(k),
      q.forecast(k),
      q.throughput(k, readLocal('reports-weeks', 12)),
      q.flow(k, readLocal('reports-flow-days', 30)),
      q.timeReport(...last30Days(), k),
    ])
      qc.prefetchQuery(o as Parameters<typeof qc.prefetchQuery>[0])
    const active = qc
      .getQueryData<{ sprints?: { id: number; state: string }[] }>(['project', k])
      ?.sprints?.find((s) => s.state === 'active')
    if (active) qc.prefetchQuery(q.burndown(active.id))
  },
  component: lazyRouteComponent(() => import('@/pages/Reports'), 'ReportsPage'),
})

const projectSettingsRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/settings',
  validateSearch: (s: Record<string, unknown>): { tab?: string; automation?: number } => ({
    tab: typeof s.tab === 'string' ? s.tab : undefined,
    // An automation to open in its editor (from a bot run).
    automation:
      Number.isInteger(Number(s.automation)) && Number(s.automation) > 0
        ? Number(s.automation)
        : undefined,
  }),
  component: lazyRouteComponent(() => import('@/pages/ProjectSettings'), 'ProjectSettingsPage'),
})

const projectEnvironmentsRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/environments',
  loader: ({ context, params }) => context.qc.prefetchQuery(envsQuery(params.key, true)),
  component: lazyRouteComponent(() => import('@/pages/Environments'), 'ProjectEnvironmentsPage'),
})

const projectAgentsRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/agents',
  component: lazyRouteComponent(() => import('@/pages/ProjectAgents'), 'ProjectAgentsPage'),
})

const environmentRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/environments/$name',
  loader: ({ context, params }) => context.qc.prefetchQuery(envQuery(params.key, params.name)),
  component: lazyRouteComponent(() => import('@/pages/Environment'), 'EnvironmentPage'),
})

export interface PagesSearch {
  q?: string
  category?: string
  project?: string
}
const pagesSearch = (s: Record<string, unknown>): PagesSearch => ({
  q: typeof s.q === 'string' && s.q ? s.q : undefined,
  category: typeof s.category === 'string' && s.category ? s.category : undefined,
  project: typeof s.project === 'string' && s.project ? s.project : undefined,
})

const pagesRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/pages',
  validateSearch: pagesSearch,
  loader: ({ context }) => context.qc.ensureQueryData(pagesQuery({})),
  component: lazyRouteComponent(() => import('@/pages/Pages'), 'PagesPage'),
})

const pageRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/pages/$id',
  validateSearch: pagesSearch,
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(pagesQuery({}))
    return context.qc.ensureQueryData(pageQuery(Number(params.id))).catch(() => null)
  },
  component: lazyRouteComponent(() => import('@/pages/Pages'), 'PagesPage'),
})

const deviceRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/device',
  validateSearch: (s: Record<string, unknown>): { code?: string } => ({
    code: typeof s.code === 'string' ? s.code : undefined,
  }),
  component: lazyRouteComponent(() => import('@/pages/Device'), 'DevicePage'),
})

const projectPagesRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/pages',
  loader: ({ context, params }) => context.qc.ensureQueryData(pagesQuery({ project: params.key })),
  component: lazyRouteComponent(() => import('@/pages/Pages'), 'ProjectPagesPage'),
})

// ---- knowledge: glossary, design notes, wikilinks (WP-D10) ----

const projectFilter = (s: Record<string, unknown>): { project?: string } => ({
  project: typeof s.project === 'string' && s.project ? s.project : undefined,
})

const glossaryRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/glossary',
  validateSearch: projectFilter,
  loaderDeps: ({ search }) => ({ project: search.project }),
  loader: ({ context, deps }) =>
    context.qc.ensureQueryData(
      glossaryQuery({
        project: deps.project,
        own: !!deps.project && deps.project !== 'global',
      }),
    ),
  component: lazyRouteComponent(() => import('@/pages/Glossary'), 'GlossaryPage'),
})

const glossaryEntryRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/glossary/$id',
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(linksQuery({ type: 'glossary', id: Number(params.id) }))
    return context.qc.ensureQueryData(glossaryEntryQuery(Number(params.id))).catch(() => null)
  },
  component: lazyRouteComponent(() => import('@/pages/Glossary'), 'GlossaryEntryPage'),
})

const designRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/design',
  validateSearch: projectFilter,
  loaderDeps: ({ search }) => ({ project: search.project }),
  loader: ({ context, deps }) =>
    context.qc.ensureQueryData(
      designQuery({
        project: deps.project,
        own: !!deps.project && deps.project !== 'global',
        status: readLocal('design-status', '') || undefined,
      }),
    ),
  component: lazyRouteComponent(() => import('@/pages/Design'), 'DesignPage'),
})

const designReviewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/design/review',
  validateSearch: projectFilter,
  loaderDeps: ({ search }) => ({ project: search.project ?? '' }),
  loader: ({ context, deps }) => context.qc.ensureQueryData(designReviewQuery(deps.project)),
  component: lazyRouteComponent(() => import('@/pages/Design'), 'DesignReviewPage'),
})

const designNoteRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/design/$id',
  validateSearch: (s: Record<string, unknown>): { tab?: string } => ({
    tab: typeof s.tab === 'string' ? s.tab : undefined,
  }),
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(linksQuery({ type: 'design', id: Number(params.id) }))
    return context.qc.ensureQueryData(designNoteQuery(Number(params.id))).catch(() => null)
  },
  component: lazyRouteComponent(() => import('@/pages/Design'), 'DesignNotePage'),
})

const goRoute = createRoute({
  getParentRoute: () => appRoute,
  path: '/go',
  validateSearch: (s: Record<string, unknown>): { ref?: string; project?: string } => ({
    ref: typeof s.ref === 'string' ? s.ref : undefined,
    project: typeof s.project === 'string' && s.project ? s.project : undefined,
  }),
  component: lazyRouteComponent(() => import('@/pages/Go'), 'GoPage'),
})

const projectGlossaryRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/glossary',
  loader: ({ context, params }) =>
    context.qc.ensureQueryData(
      glossaryQuery({ project: params.key, own: readLocal(`glossary-own-${params.key}`, false) }),
    ),
  component: lazyRouteComponent(() => import('@/pages/Glossary'), 'ProjectGlossaryPage'),
})

const projectGoalsRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/goals',
  loader: ({ context, params }) => context.qc.ensureQueryData(goalsQuery(params.key)),
  component: lazyRouteComponent(() => import('@/pages/Goals'), 'ProjectGoalsPage'),
})

const projectDesignRoute = createRoute({
  getParentRoute: () => projectRoute,
  path: '/design',
  loader: ({ context, params }) => {
    context.qc.prefetchQuery(designReviewQuery(params.key))
    return context.qc.ensureQueryData(
      designQuery({
        project: params.key,
        own: readLocal(`design-own-${params.key}`, false),
        status: readLocal('design-status', '') || undefined,
      }),
    )
  },
  component: lazyRouteComponent(() => import('@/pages/Design'), 'ProjectDesignPage'),
})

const routeTree = rootRoute.addChildren([
  loginRoute,
  inviteRoute,
  resetRoute,
  appRoute.addChildren([
    homeRoute,
    inboxRoute,
    objectivesRoute,
    inboxRequestRoute,
    searchRoute,
    graphRoute,
    viewRoute,
    timeRoute,
    settingsRoute,
    adminRoute,
    agentsRoute,
    agentSessionRoute,
    assistantsRoute,
    assistantRoute,
    environmentsRoute,
    liveRoute,
    overviewRoute,
    catchUpRoute,
    ciRoute,
    ciHistoryRoute,
    runnerJobRoute,
    codeAuditRoute,
    issueRoute,
    pagesRoute,
    pageRoute,
    glossaryRoute,
    glossaryEntryRoute,
    designRoute,
    designReviewRoute,
    designNoteRoute,
    goRoute,
    deviceRoute,
    projectRoute.addChildren([
      projectIndexRoute,
      projectOverviewRoute,
      boardRoute,
      backlogRoute,
      listRoute,
      timelineRoute,
      reportsRoute,
      projectEnvironmentsRoute,
      projectAgentsRoute,
      environmentRoute,
      projectPagesRoute,
      projectGlossaryRoute,
      projectGoalsRoute,
      projectDesignRoute,
      projectSettingsRoute,
    ]),
  ]),
])

export function makeRouter(qc: QueryClient) {
  return createRouter({
    routeTree,
    context: { qc },
    // Hovering or touching a link loads the next page's data.
    defaultPreload: 'intent',
    defaultPreloadDelay: 40,
    // Freshness is the query cache's job.
    defaultPreloadStaleTime: 0,
    defaultPendingMs: 400,
    defaultErrorComponent: ErrorPage,
    scrollRestoration: true,
  })
}

declare module '@tanstack/react-router' {
  interface Register {
    router: ReturnType<typeof makeRouter>
  }
}
