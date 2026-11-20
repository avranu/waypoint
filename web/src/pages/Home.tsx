import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import { useQueries, useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { BrushCleaning as Broom, ChevronDown, PartyPopper } from 'lucide-react'
import { pageQ, q, STALE_QUERY } from '@/lib/queries'
import { LongTitleHint, SaveError, useQuickAdd } from '@/components/issue/quickAdd'
import {
  QuickAddDetailsButton,
  isDetailsKey,
  openDetails,
} from '@/components/issue/QuickAddDetails'
import { dueState } from '@/lib/format'
import { useLocal } from '@/lib/hooks'
import { Empty, PageHeader } from '@/components/ui/misc'
import { Select } from '@/components/ui/input'
import { toast } from '@/components/ui/toast'
import { IssueRow } from '@/components/issue/IssueRow'
import { ProjectIcon } from '@/components/issue/icons'
import { IssueSheet } from './Project'
import { AssistantDesk } from '@/components/AssistantDesk'
import type { Issue, Project } from '@/lib/types'

function greeting() {
  const h = new Date().getHours()
  return h < 5 ? 'Up late' : h < 12 ? 'Good morning' : h < 18 ? 'Good afternoon' : 'Good evening'
}

/** Projects (with statuses) for a set of issues, for inline editing. */
export function useProjectsFor(issues: Issue[]): Record<string, Project> {
  const keys = useMemo(() => [...new Set(issues.map((i) => i.project_key))], [issues])
  const results = useQueries({ queries: keys.map((k) => q.project(k)) })
  return Object.fromEntries(results.filter((r) => r.data).map((r) => [r.data!.key, r.data!]))
}

export function HomePage() {
  const me = useQuery(q.me()).data
  const open = useQuery(pageQ.homeOpen()).data
  const done = useQuery(pageQ.homeDone()).data
  const issues = open?.issues ?? []
  const projects = useProjectsFor([...issues, ...(done?.issues ?? [])])
  const [sheet, setSheet] = useState<string | null>(null)
  useEffect(() => {
    document.title = 'My work · Waypoint'
  }, [])

  const sections = useMemo(() => {
    const s: { id: string; title: string; items: Issue[]; tone?: string }[] = [
      { id: 'overdue', title: 'Overdue', items: [], tone: 'text-danger' },
      { id: 'soon', title: 'Due soon', items: [], tone: 'text-warn' },
      { id: 'doing', title: 'In progress', items: [] },
      { id: 'next', title: 'Up next', items: [] },
      { id: 'later', title: 'Backlog', items: [] },
    ]
    for (const i of issues) {
      const d = dueState(i.due_date)
      if (d === 'overdue') s[0].items.push(i)
      else if (d === 'today' || d === 'soon') s[1].items.push(i)
      else if (i.status.category === 'in_progress') s[2].items.push(i)
      else if (i.status.category === 'todo') s[3].items.push(i)
      else s[4].items.push(i)
    }
    return s.filter((x) => x.items.length)
  }, [issues])

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto max-w-5xl pb-16">
        <PageHeader
          title={`${greeting()}, ${me?.display_name.split(' ')[0] ?? ''}`}
          subtitle={
            open
              ? issues.length
                ? `${issues.length} open ${issues.length === 1 ? 'issue' : 'issues'} assigned to you${sections[0]?.id === 'overdue' ? ` · ${sections[0].items.length} overdue` : ''}`
                : 'Nothing assigned to you.'
              : '…'
          }
        />
        <div className="space-y-3 px-4 sm:px-6">
          <QuickCapture />
          <AssistantDesk />
          <StaleWork />
        </div>
        <div className="mt-4 space-y-4 px-4 sm:px-6">
          {open && issues.length === 0 && (
            <Empty icon={<PartyPopper />} title="You're all caught up">
              Issues assigned to you show up here, sorted by what's most urgent.
            </Empty>
          )}
          {sections.map((s) => (
            <Group key={s.id} id={s.id} title={s.title} tone={s.tone} count={s.items.length}>
              {s.items.map((i) => (
                <IssueRow
                  key={i.key}
                  issue={i}
                  project={projects[i.project_key]}
                  onOpen={setSheet}
                  showProject
                />
              ))}
            </Group>
          ))}
          {done && done.issues.length > 0 && (
            <Group
              id="done"
              title="Completed this week"
              count={done.issues.length}
              defaultCollapsed
            >
              {done.issues.map((i) => (
                <IssueRow
                  key={i.key}
                  issue={i}
                  project={projects[i.project_key]}
                  onOpen={setSheet}
                  showProject
                  showUpdated
                />
              ))}
            </Group>
          )}
        </div>
      </div>
      {sheet && <IssueSheet issueKey={sheet} onClose={() => setSheet(null)} />}
    </div>
  )
}

function Group({
  id,
  title,
  count,
  tone,
  children,
  defaultCollapsed,
}: {
  id: string
  title: string
  count: number
  tone?: string
  children: React.ReactNode
  defaultCollapsed?: boolean
}) {
  const [collapsed, setCollapsed] = useLocal(`home-${id}`, !!defaultCollapsed)
  return (
    <section className="overflow-hidden rounded-xl border border-line bg-panel">
      <button
        onClick={() => setCollapsed(!collapsed)}
        className="flex h-10 w-full items-center gap-2 px-4 text-[13px] font-semibold"
      >
        <ChevronDown
          className={clsx('size-4 text-subtle transition-transform', collapsed && '-rotate-90')}
        />
        <span className={tone}>{title}</span>
        <span className="text-xs font-normal text-subtle">{count}</span>
      </button>
      {!collapsed && <div className="border-t border-line">{children}</div>}
    </section>
  )
}

/** Coming back after a break: surface work that has gone quiet. */
function StaleWork() {
  const stale = useQuery(pageQ.homeStale()).data
  if (!stale || stale.total === 0) return null
  return (
    <Link
      to="/search"
      search={{ q: STALE_QUERY }}
      className="flex items-center gap-3 rounded-xl border border-dashed border-line-strong px-4 py-2.5 text-[13px] text-muted hover:bg-hover hover:text-fg"
    >
      <Broom className="size-4 shrink-0" />
      <span className="flex-1">
        {stale.total} open {stale.total === 1 ? 'issue hasn’t' : 'issues haven’t'} changed in a
        month. Close, re-plan or park them in the backlog.
      </span>
      <span className="font-medium text-accent">Review</span>
    </Link>
  )
}

/** Capture an issue in one line, assigned to me. */
function QuickCapture() {
  const navigate = useNavigate()
  const projects = (useQuery(q.projects()).data ?? []).filter(
    (p) => p.role !== 'viewer' && p.role !== 'guest' && !p.archived,
  )
  const [projectKey, setProjectKey] = useLocal('last-project', '')
  const add = useQuickAdd('home')
  const key = projects.some((p) => p.key === projectKey) ? projectKey : projects[0]?.key
  const project = projects.find((p) => p.key === key)
  if (!projects.length) return null
  const submit = () =>
    add
      .submit((p) =>
        key
          ? { project: key, ...p.input, title: p.title, assignees: p.input.assignees ?? ['me'] }
          : null,
      )
      .then(
        (d) =>
          d &&
          toast(`Created ${d.key}`, {
            action: {
              label: 'Open',
              run: () => navigate({ to: '/issue/$key', params: { key: d.key } }),
            },
          }),
      )
  const details = () => openDetails(add, { project: key, assignees: ['me'] })
  return (
    <div className="space-y-1">
      <form
        className="flex items-center gap-2 rounded-xl border border-line bg-panel p-1.5 shadow-card focus-within:border-accent"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <span className="pl-1.5">
          {project && (
            <ProjectIcon icon={project.icon} color={project.color} name={project.name} size={20} />
          )}
        </span>
        <input
          value={add.text}
          readOnly={add.pending}
          onChange={(e) => add.setText(e.target.value)}
          placeholder="Capture something…"
          title="Capture an issue: !high #tag ~2 due:fri set details"
          aria-label="Capture an issue"
          onKeyDown={(e) => {
            if (isDetailsKey(e)) {
              e.preventDefault()
              details()
            }
          }}
          className="h-8 min-w-0 flex-1 bg-transparent text-sm outline-none placeholder:text-subtle"
        />
        {!add.pending && <QuickAddDetailsButton onOpen={details} />}
        <Select
          value={key}
          onChange={(e) => setProjectKey(e.target.value)}
          aria-label="Project"
          className="!h-8 w-28 border-none bg-sunken text-xs sm:w-40"
        >
          {projects.map((p) => (
            <option key={p.key} value={p.key}>
              {p.name}
            </option>
          ))}
        </Select>
      </form>
      <LongTitleHint show={add.long} className="px-2 text-[11px] text-muted" />
      <SaveError error={add.error} className="px-2 text-[11px] text-danger" />
    </div>
  )
}
