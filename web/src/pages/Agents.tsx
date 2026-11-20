// The Agents page (WP-D15, WP-D16): the overview of who is working on what
// and what waits on you, what agents will take next and in what order, their
// sessions, what they did, the pull requests that came of it, how they used
// the glossary, pages and design notes, and what people decided. One filter
// bar (agent, project, period) carries across the tabs, so switching tabs
// keeps the question being asked.

import { useEffect, useMemo, useRef, type ReactNode } from 'react'
import { Link, useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  Activity,
  BookOpenCheck,
  Bot,
  ChartLine,
  SquareTerminal,
  ChevronDown,
  FileCog,
  Gauge,
  GitPullRequest,
  LayoutDashboard,
  ListOrdered,
  Package,
  Plane,
  ScrollText,
  ShieldCheck,
  Terminal,
  X,
} from 'lucide-react'
import { autopilotQuery, q } from '@/lib/queries'
import { agentLabel, agentMatches, knownAgentsQuery, projectTree } from '@/lib/agents'
import { relativeTime } from '@/lib/format'
import { PageHeader } from '@/components/ui/misc'
import { Select } from '@/components/ui/input'
import { Picker, Popover, usePopover, type PickItem } from '@/components/ui/popover'
import { OverviewTab } from './agents/Overview'
import { QueueTab } from './agents/Queue'
import { SessionsTab } from './agents/Sessions'
import { ActivityTab } from './agents/Activity'
import { PullsTab } from './agents/Pulls'
import { KnowledgeTab } from './agents/Knowledge'
import { AutopilotTab } from './agents/Autopilot'
import { UsageTab } from './agents/Usage'
import { TrendsTab } from './agents/Trends'
import { CliUseTab } from './agents/CliUse'
import { AuditTab } from './agents/Audit'
import { AuditsTab } from './agents/Audits'
import { KitTab } from './agents/Kit'
import { SetupTab } from './agents/Setup'
import {
  DEFAULT_DAYS,
  PERIODS,
  type AgentsSearch,
  type AgentsTab,
  type HubFilters,
  type HubTabProps,
} from './agents/search'

const TABS: {
  id: AgentsTab
  label: string
  icon: ReactNode
  Component: (p: HubTabProps) => ReactNode
}[] = [
  { id: 'overview', label: 'Overview', icon: <LayoutDashboard />, Component: OverviewTab },
  { id: 'queue', label: 'Queue', icon: <ListOrdered />, Component: QueueTab },
  { id: 'sessions', label: 'Sessions', icon: <Terminal />, Component: SessionsTab },
  { id: 'autopilot', label: 'Autopilot', icon: <Plane />, Component: AutopilotTab },
  { id: 'activity', label: 'Activity', icon: <Activity />, Component: ActivityTab },
  { id: 'pulls', label: 'Pull requests', icon: <GitPullRequest />, Component: PullsTab },
  { id: 'knowledge', label: 'Knowledge', icon: <BookOpenCheck />, Component: KnowledgeTab },
  { id: 'usage', label: 'Usage', icon: <Gauge />, Component: UsageTab },
  { id: 'trends', label: 'Trends', icon: <ChartLine />, Component: TrendsTab },
  { id: 'cli', label: 'wp CLI', icon: <SquareTerminal />, Component: CliUseTab },
  { id: 'kit', label: 'Kit', icon: <Package />, Component: KitTab },
  { id: 'setup', label: 'Setup', icon: <FileCog />, Component: SetupTab },
  { id: 'audits', label: 'Audits', icon: <ShieldCheck />, Component: AuditsTab },
  { id: 'audit', label: 'Audit log', icon: <ScrollText />, Component: AuditTab },
]

export function AgentsPage() {
  const search = useSearch({ from: '/app/agents' })
  const navigate = useNavigate({ from: '/agents' })
  const tab = TABS.find((t) => t.id === search.tab) ?? TABS[0]
  const unread = useQuery(autopilotQuery()).data?.unreviewed ?? 0
  const filters: HubFilters = {
    agent: search.agent ?? '',
    project: search.project ?? '',
    days: search.days ?? DEFAULT_DAYS,
    machine: search.machine,
    session: search.session,
    who: search.who,
    repo: search.repo,
    branch: search.branch,
    state: search.state,
    view: search.view,
  }
  const setFilters = (f: Partial<AgentsSearch>) =>
    navigate({ to: '/agents', search: (s: AgentsSearch) => ({ ...s, ...f }), replace: true })
  const go = (t: AgentsTab) =>
    navigate({
      to: '/agents',
      search: (s: AgentsSearch) => ({
        ...s,
        tab: t === 'overview' ? undefined : t,
        pr: undefined,
        machine: undefined,
        session: undefined,
        who: undefined,
        repo: undefined,
        branch: undefined,
        state: undefined,
        view: undefined,
      }),
    })
  // On a phone the tab strip scrolls: bring the open tab (a link from a notification) into view.
  const strip = useRef<HTMLElement>(null)
  useEffect(() => {
    strip.current?.querySelector('[aria-selected="true"]')?.scrollIntoView({
      inline: 'center',
      block: 'nearest',
    })
  }, [tab.id])
  useEffect(() => {
    document.title = `Agents · ${tab.label} · Waypoint`
  }, [tab.label])
  const Tab = tab.Component
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <header className="shrink-0 border-b border-line">
        <div className="mx-auto w-full max-w-[1600px]">
          <PageHeader icon={<Bot className="size-5 text-accent" />} title="Agents" />
          <nav
            ref={strip}
            role="tablist"
            aria-label="Agents sections"
            className="no-scrollbar -mb-px flex gap-1 overflow-x-auto px-2 sm:px-4"
          >
            {TABS.map((t) => (
              <button
                key={t.id}
                role="tab"
                aria-selected={t.id === tab.id}
                onClick={() => go(t.id)}
                className={clsx(
                  'flex h-9 shrink-0 items-center gap-1.5 border-b-2 px-2.5 text-[13px] [&_svg]:size-3.5',
                  t.id === tab.id
                    ? 'border-accent font-medium text-fg'
                    : 'border-transparent text-muted hover:text-fg',
                )}
              >
                {t.icon}
                {t.label}
                {t.id === 'autopilot' && unread > 0 && (
                  <span
                    role="img"
                    aria-label={`${unread} unread task ${unread === 1 ? 'summary' : 'summaries'}`}
                    title={`${unread} unread task ${unread === 1 ? 'summary' : 'summaries'}`}
                    className="inline-flex h-4 min-w-4 items-center justify-center rounded-full bg-accent px-1 text-[10px] font-semibold leading-none text-accent-fg"
                  >
                    {unread > 99 ? '99+' : unread}
                  </span>
                )}
              </button>
            ))}
          </nav>
        </div>
      </header>
      <div className="min-h-0 flex-1 overflow-y-auto pb-16">
        <div className="mx-auto w-full max-w-[1600px] px-4 pt-3 sm:px-6">
          {tab.id !== 'kit' && tab.id !== 'setup' && (
            <FilterBar
              filters={filters}
              setFilters={setFilters}
              period={!['queue', 'sessions', 'autopilot', 'audits'].includes(tab.id)}
              periodOnly={tab.id === 'cli'}
            />
          )}
          <div role="tabpanel" className={tab.id === 'kit' || tab.id === 'setup' ? 'mt-1' : 'mt-4'}>
            <Tab key={tab.id} filters={filters} setFilters={setFilters} />
          </div>
        </div>
      </div>
    </div>
  )
}

/** Agent, project and period: the same three questions on every tab. */
function FilterBar({
  filters,
  setFilters,
  period,
  periodOnly,
}: HubTabProps & { period: boolean; periodOnly?: boolean }) {
  const projects = useQuery(q.projects()).data ?? []
  return (
    <div className="flex flex-wrap items-center gap-2">
      {!periodOnly && (
        <>
          <AgentFilter
            value={filters.agent}
            onChange={(agent) => setFilters({ agent: agent || undefined })}
          />
          <Select
            aria-label="Project"
            value={filters.project}
            onChange={(e) => setFilters({ project: e.target.value || undefined })}
            className="!w-auto min-w-36 max-w-56"
          >
            <option value="">All projects</option>
            {projectTree(projects).map(({ project: p, depth }) => (
              <option key={p.id} value={p.key}>
                {' '.repeat(depth * 2) + p.name}
              </option>
            ))}
          </Select>
        </>
      )}
      {filters.project && !periodOnly && (
        <Link
          to="/p/$key/agents"
          params={{ key: filters.project }}
          className="inline-flex h-8 items-center rounded-lg px-2 text-[13px] text-muted hover:bg-hover hover:text-fg"
        >
          {filters.project} Agents tab
        </Link>
      )}
      {period && (
        <div
          role="radiogroup"
          aria-label="Period"
          className="ml-auto flex h-8 items-center rounded-lg border border-line p-0.5"
        >
          {PERIODS.map((d) => (
            <button
              key={d}
              role="radio"
              aria-checked={filters.days === d}
              onClick={() => setFilters({ days: d === DEFAULT_DAYS ? undefined : d })}
              className={clsx(
                'h-full rounded-md px-2.5 text-xs',
                filters.days === d
                  ? 'bg-accent-soft font-medium text-fg'
                  : 'text-muted hover:text-fg',
              )}
            >
              {d} days
            </button>
          ))}
        </div>
      )}
    </div>
  )
}

/**
 * Picks an agent, a family, or any spec typed in, and says which known
 * agents it covers, since that's what delegating with it would reach.
 */
export function AgentFilter({
  value,
  onChange,
  allLabel = 'All agents',
}: {
  value: string
  onChange: (spec: string) => void
  allLabel?: string
}) {
  const pop = usePopover()
  const known = useQuery(knownAgentsQuery()).data ?? []
  const items = useMemo(() => {
    const out: PickItem<string>[] = [
      { value: '', label: allLabel, icon: <Bot className="size-3.5" /> },
    ]
    const families = new Map<string, number>()
    for (const a of known) families.set(a.family, (families.get(a.family) ?? 0) + 1)
    for (const [family, n] of families)
      if (n > 1) out.push({ value: family, label: family, hint: `${n} agents`, keywords: 'family' })
    for (const a of known)
      out.push({
        value: a.identity,
        label: a.identity,
        hint: a.last_seen ? relativeTime(a.last_seen) : a.token ? 'token' : undefined,
        keywords: [a.family, ...a.models].join(' '),
      })
    return out
  }, [known, allLabel])
  const covered = value ? known.filter((a) => agentMatches(value, a.identity)) : []
  return (
    <>
      <button
        ref={pop.anchor}
        onClick={() => pop.setOpen(true)}
        aria-label="Agent"
        className={clsx(
          'inline-flex h-8 max-w-64 items-center gap-1.5 rounded-lg border px-2.5 text-[13px]',
          value ? 'border-accent/50 bg-accent-soft' : 'border-line hover:bg-hover',
        )}
        title={
          value
            ? covered.length
              ? `Covers ${covered.map((a) => a.identity).join(', ')}`
              : 'No agent seen so far matches this'
            : undefined
        }
      >
        <Bot className="size-3.5 shrink-0 text-muted" />
        <span className="truncate">{value ? agentLabel(value) : allLabel}</span>
        {value && known.length > 0 && (
          <span className="shrink-0 text-xs text-subtle">
            {covered.length === 1 ? '' : `${covered.length} agents`}
          </span>
        )}
        {value ? (
          <span
            role="button"
            tabIndex={-1}
            aria-label="Clear agent"
            onClick={(e) => {
              e.stopPropagation()
              onChange('')
            }}
            className="-mr-1 rounded p-0.5 text-subtle hover:bg-hover hover:text-fg"
          >
            <X className="size-3" />
          </span>
        ) : (
          <ChevronDown className="size-3.5 text-subtle" />
        )}
      </button>
      <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor}>
        <Picker
          items={items}
          selected={[value]}
          onPick={(v) => onChange(v)}
          onClose={() => pop.close()}
          placeholder="Agent, family or model…"
          onCreate={(text) => onChange(text)}
          createLabel={(text) => `Agents matching “${text}”`}
        />
      </Popover>
    </>
  )
}
