import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { Link, Outlet, useNavigate, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  DndContext,
  DragOverlay,
  PointerSensor,
  TouchSensor,
  closestCenter,
  useSensor,
  useSensors,
  type CollisionDetection,
  type DragEndEvent,
  type DragOverEvent,
  type DragStartEvent,
} from '@dnd-kit/core'
import { SortableContext, useSortable } from '@dnd-kit/sortable'
import {
  Bell,
  BookA,
  Bot,
  ChevronDown,
  ChevronRight,
  ArrowDown,
  ArrowUp,
  Clock3,
  ListOrdered,
  FileText,
  Clock,
  FolderKanban,
  Home,
  LayoutDashboard,
  Inbox,
  Lightbulb,
  LogOut,
  MoreHorizontal,
  Moon,
  PanelLeftClose,
  PanelLeftOpen,
  Plus,
  Search,
  Rocket,
  Server,
  Workflow,
  Settings,
  Shield,
  Sun,
  SunMoon,
  Bookmark,
  Square,
  Trash2,
  Sparkles,
  CircleCheckBig,
} from 'lucide-react'
import { api } from '@/lib/api'
import { q, useProjectOrder, useSavePrefs, useTimer } from '@/lib/queries'
import { startLive } from '@/lib/live'
import { clock } from '@/lib/format'
import { designReviewQuery } from '@/lib/knowledge'
import { envSummaryQuery } from '@/lib/environments'
import { useHotkey, useLocal, useMediaQuery, useScrollingDown, useTick } from '@/lib/hooks'
import { setTheme, storedTheme, type Theme } from '@/lib/theme'
import { waitingQuery } from '@/lib/requests'
import { assistantsQuery, health, HEALTH_COLOR, type Assistant } from '@/lib/assistant'
import { Avatar, Kbd } from '@/components/ui/misc'
import { IconButton } from '@/components/ui/button'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { ProjectIcon } from '@/components/issue/icons'
import { toastError } from '@/components/ui/toast'
import { CommandPalette } from './CommandPalette'
import { Shortcuts } from './Shortcuts'
import { CreateIssueDialog, openCreateIssue, useCreateIssueDialog } from './CreateIssue'
import { GlobalAuditDialog } from './AuditDialog'
import { UnsavedGuard } from './UnsavedGuard'
import { NewProjectDialog } from './NewProject'
import { DeleteProjectsDialog } from './DeleteProjects'
import type { Project } from '@/lib/types'

export function AppShell() {
  const qc = useQueryClient()
  const me = useQuery(q.me()).data
  const [collapsed, setCollapsed] = useLocal('sidebar-collapsed', false)
  const [palette, setPalette] = useState(false)
  const [newProject, setNewProject] = useState(false)
  // Deleting projects: one (from its menu) or a pick of several.
  const [deleting, setDeleting] = useState<{ only?: Project } | null>(null)
  const onDeleteProjects = (only?: Project) => setDeleting({ only })
  const create = useCreateIssueDialog()
  const desktop = useMediaQuery('(min-width: 768px)')

  useEffect(() => startLive(qc), [qc])
  // On a new device, follow the theme saved to the profile.
  useEffect(() => {
    if (me?.prefs.theme && !localStorage.getItem('wp:theme')) setTheme(me.prefs.theme)
  }, [me?.prefs.theme])
  // Warm the caches the sidebar and pickers need.
  useEffect(() => {
    qc.prefetchQuery(q.users())
    qc.prefetchQuery(q.timer())
    qc.prefetchQuery(q.inboxCounts())
    qc.prefetchQuery(waitingQuery())
    qc.prefetchQuery(q.views())
    qc.prefetchQuery(q.tags())
  }, [qc])

  useHotkey('k', () => setPalette(true), { meta: true })
  useHotkey('/', () => setPalette(true))
  useHotkey('c', () => create.open())
  useHotkey('d', () => openCreateIssue({ draft: true }))
  useHotkey('[', () => setCollapsed(!collapsed))

  return (
    <div className="flex h-full">
      {desktop && (
        <Sidebar
          collapsed={collapsed}
          onToggle={() => setCollapsed(!collapsed)}
          onSearch={() => setPalette(true)}
          onCreate={() => create.open()}
          onNewProject={() => setNewProject(true)}
          onDeleteProjects={onDeleteProjects}
        />
      )}
      <main className="relative flex min-w-0 flex-1 flex-col overflow-hidden max-md:pb-[60px]">
        <Outlet />
      </main>
      {!desktop && (
        <MobileNav
          onSearch={() => setPalette(true)}
          onCreate={() => create.open()}
          onDeleteProjects={() => onDeleteProjects()}
        />
      )}
      <CommandPalette
        open={palette}
        onClose={() => setPalette(false)}
        onCreate={() => create.open()}
        onNewProject={() => setNewProject(true)}
        onDeleteProjects={() => onDeleteProjects()}
      />
      <CreateIssueDialog />
      <GlobalAuditDialog />
      <UnsavedGuard />
      <NewProjectDialog open={newProject} onClose={() => setNewProject(false)} />
      {deleting && <DeleteProjectsDialog only={deleting.only} onClose={() => setDeleting(null)} />}
      <Shortcuts />
    </div>
  )
}

function NavLink({
  to,
  icon,
  children,
  badge,
  badgeDanger,
  collapsed,
  exact,
  secondary,
}: {
  to: string
  icon: ReactNode
  children: ReactNode
  badge?: number
  badgeDanger?: boolean
  collapsed?: boolean
  exact?: boolean
  /** Left out of the collapsed rail, which keeps core navigation only. */
  secondary?: boolean
}) {
  if (collapsed && secondary) return null
  const label = String(children)
  return (
    <Link
      to={to}
      activeOptions={{ exact }}
      className="group relative flex h-8 items-center gap-2.5 rounded-lg px-2 text-[13px] text-muted hover:bg-hover hover:text-fg data-[status=active]:bg-hover data-[status=active]:font-medium data-[status=active]:text-fg"
      title={collapsed ? (badge ? `${label} (${badge})` : label) : undefined}
      aria-label={collapsed ? (badge ? `${label}, ${badge}` : label) : undefined}
    >
      <span className="flex size-4 shrink-0 items-center justify-center [&>svg]:size-4">
        {icon}
      </span>
      {!collapsed && <span className="flex-1 truncate">{children}</span>}
      {!!badge &&
        (collapsed ? (
          <span
            className={clsx(
              'absolute right-1.5 top-1.5 size-2 rounded-full ring-2 ring-panel',
              badgeDanger ? 'bg-danger' : 'bg-accent',
            )}
          />
        ) : (
          <span
            className={clsx(
              'rounded-full px-1.5 text-[10px] font-semibold leading-4',
              badgeDanger ? 'bg-danger text-white' : 'bg-accent text-accent-fg',
            )}
          >
            {badge}
          </span>
        ))}
    </Link>
  )
}

function Sidebar({
  collapsed,
  onToggle,
  onSearch,
  onCreate,
  onNewProject,
  onDeleteProjects,
}: {
  collapsed: boolean
  onToggle: () => void
  onSearch: () => void
  onCreate: () => void
  onNewProject: () => void
  onDeleteProjects: (only?: Project) => void
}) {
  const me = useQuery(q.me()).data
  const projects = useQuery(q.projects()).data ?? []
  const [folded, setFolded] = useLocal<number[]>('projects-folded', [])
  const views = (useQuery(q.views()).data ?? []).filter((v) => v.pinned)
  // The Inbox badge counts what waits on you (WP-D16) with the unread notifications
  // that are still relevant (WP-1141).
  const waiting = useQuery(waitingQuery()).data
  const unread = (useQuery(q.inboxCounts()).data?.live_unread ?? 0) + (waiting?.total ?? 0)
  // Finished work no one has looked at yet (WP-245).
  const closes = waiting?.closes ?? 0
  const envsDown = useQuery(envSummaryQuery()).data?.down ?? 0
  const assistants = useMyAssistants()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const owns = projects.some((p) => p.role === 'owner')
  const menu = usePopover()
  const order = useProjectOrder()
  const visible = order.tree.filter((n) => !n.ancestors.some((a) => folded.includes(a)))
  const sensors = useSensors(
    useSensor(PointerSensor, { activationConstraint: { distance: 6 } }),
    useSensor(TouchSensor, { activationConstraint: { delay: 220, tolerance: 6 } }),
  )
  const [dragging, setDragging] = useState<number | null>(null)
  const [over, setOver] = useState<number | null>(null)
  const parentOf = useMemo(
    () => new Map(order.tree.map((n) => [n.project.id, n.ancestors.at(-1) ?? null])),
    [order.tree],
  )
  // A project only moves among its siblings, so only they are drop targets.
  const siblingsOnly: CollisionDetection = (args) =>
    closestCenter({
      ...args,
      droppableContainers: args.droppableContainers.filter(
        (c) => parentOf.get(Number(c.id)) === parentOf.get(Number(args.active.id)),
      ),
    })
  const siblings = (id: number) =>
    order.tree
      .filter((n) => parentOf.get(n.project.id) === parentOf.get(id))
      .map((n) => n.project.id)
  const reset = () => (setDragging(null), setOver(null))
  const onDragEnd = (e: DragEndEvent) => {
    reset()
    if (e.over) order.move(Number(e.active.id), Number(e.over.id))
  }
  return (
    <aside
      className={clsx(
        'flex shrink-0 flex-col border-r border-line bg-side-scroll transition-[width]',
        collapsed ? 'w-14' : 'w-60',
      )}
    >
      <div className="bg-side-fixed shadow-[0_1px_0_var(--wp-line)]">
        <div className="flex h-12 items-center gap-2 px-3">
          {!collapsed && <UserMenu />}
          <IconButton
            label={collapsed ? 'Expand sidebar' : 'Collapse sidebar'}
            onClick={onToggle}
            icon={
              collapsed ? (
                <PanelLeftOpen className="size-4" />
              ) : (
                <PanelLeftClose className="size-4" />
              )
            }
            className={clsx('shrink-0', collapsed && 'mx-auto')}
          />
        </div>
        <div className="space-y-0.5 px-2">
          <button
            onClick={onSearch}
            className="flex h-8 w-full items-center gap-2.5 rounded-lg px-2 text-[13px] text-muted hover:bg-hover hover:text-fg"
            title="Search (⌘K)"
          >
            <Search className="size-4 shrink-0" />
            {!collapsed && (
              <>
                <span className="flex-1 text-left">Search</span>
                <Kbd>⌘K</Kbd>
              </>
            )}
          </button>
          <button
            onClick={onCreate}
            className="flex h-8 w-full items-center gap-2.5 rounded-lg px-2 text-[13px] text-muted hover:bg-hover hover:text-fg"
            title="New issue (C)"
          >
            <Plus className="size-4 shrink-0" />
            {!collapsed && (
              <>
                <span className="flex-1 text-left">New issue</span>
                <Kbd>C</Kbd>
              </>
            )}
          </button>
          <TimerSidebar collapsed={collapsed} />
        </div>
        <div className="h-2" />
      </div>
      <nav className="flex-1 pt-2 space-y-0.5 overflow-y-auto px-2 pb-3">
        <NavLink to="/" exact icon={<Home />} collapsed={collapsed}>
          My work
        </NavLink>
        <NavLink to="/overview" icon={<LayoutDashboard />} collapsed={collapsed}>
          Overview
        </NavLink>
        <NavLink to="/inbox" icon={<Inbox />} badge={unread} collapsed={collapsed}>
          Inbox
        </NavLink>
        <NavLink to="/time" icon={<Clock />} collapsed={collapsed}>
          Time
        </NavLink>
        <NavLink to="/agents" icon={<Bot />} collapsed={collapsed}>
          Agents
        </NavLink>
        {assistants.map((a) => (
          <NavLink
            key={a.id}
            to={`/assistant/${a.id}`}
            icon={<AssistantGlyph a={a} />}
            badge={a.state.pending.length}
            secondary={!a.state.pending.length}
            collapsed={collapsed}
          >
            {a.name}
          </NavLink>
        ))}
        <NavLink
          to="/environments"
          icon={<Server />}
          badge={envsDown}
          badgeDanger
          secondary={!envsDown}
          collapsed={collapsed}
        >
          Environments
        </NavLink>
        <NavLink
          to="/catch-up"
          secondary={!closes}
          icon={<CircleCheckBig />}
          badge={closes}
          collapsed={collapsed}
        >
          Catch up
        </NavLink>
        <NavLink to="/live" secondary icon={<Rocket />} collapsed={collapsed}>
          What’s live
        </NavLink>
        <NavLink to="/ci" secondary icon={<Workflow />} collapsed={collapsed}>
          CI
        </NavLink>
        <NavLink to="/pages" secondary icon={<FileText />} collapsed={collapsed}>
          Pages
        </NavLink>
        <NavLink to="/design" secondary icon={<Lightbulb />} collapsed={collapsed}>
          Design
        </NavLink>
        <NavLink to="/glossary" secondary icon={<BookA />} collapsed={collapsed}>
          Glossary
        </NavLink>
        {views.length > 0 && !collapsed && (
          <>
            {!collapsed && (
              <div className="px-2 pb-1 pt-4 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                Views
              </div>
            )}
            {views.map((v) => (
              <NavLink
                key={v.id}
                to={`/views/${v.id}`}
                icon={<Bookmark />}
                secondary
                collapsed={collapsed}
              >
                {v.name}
              </NavLink>
            ))}
          </>
        )}
        {!collapsed && (
          <div className="flex items-center justify-between px-2 pb-1 pt-4">
            <span className="text-[11px] font-semibold uppercase tracking-wide text-subtle">
              Projects
            </span>
            <div className="flex items-center gap-0.5">
              <IconButton
                ref={menu.anchor}
                label="Project actions"
                size="xs"
                onClick={menu.toggle}
                icon={<MoreHorizontal className="size-3.5" />}
              />
              <Popover open={menu.open} onClose={menu.close} anchor={menu.anchor} align="end">
                <MenuList
                  onClose={menu.close}
                  items={[
                    {
                      label: 'Custom order',
                      icon: <ListOrdered />,
                      checked: order.sort === 'custom',
                      onSelect: () => order.setSort('custom'),
                    },
                    {
                      label: 'Recently used',
                      icon: <Clock3 />,
                      checked: order.sort === 'recent',
                      onSelect: () => order.setSort('recent'),
                    },
                    ...(owns
                      ? [
                          { separator: true, label: '' },
                          { label: 'New project', icon: <Plus />, onSelect: onNewProject },
                          {
                            label: 'Delete projects…',
                            icon: <Trash2 />,
                            danger: true,
                            onSelect: () => onDeleteProjects(),
                          },
                        ]
                      : []),
                  ]}
                />
              </Popover>
              <IconButton
                label="New project"
                size="xs"
                onClick={onNewProject}
                icon={<Plus className="size-3.5" />}
              />
            </div>
          </div>
        )}
        <DndContext
          sensors={sensors}
          collisionDetection={siblingsOnly}
          onDragStart={(e: DragStartEvent) => setDragging(Number(e.active.id))}
          onDragOver={(e: DragOverEvent) => setOver(e.over ? Number(e.over.id) : null)}
          onDragEnd={onDragEnd}
          onDragCancel={reset}
        >
          <SortableContext items={visible.map((n) => n.project.id)} strategy={() => null}>
            {visible.map(({ project: p, depth, hasChildren }) => {
              const group = siblings(p.id)
              const at = group.indexOf(p.id)
              return (
                <ProjectRow
                  key={p.id}
                  project={p}
                  depth={depth}
                  hasChildren={hasChildren}
                  collapsed={collapsed}
                  active={pathname.startsWith(`/p/${p.key}/`) || pathname === `/p/${p.key}`}
                  isFolded={folded.includes(p.id)}
                  onFold={() =>
                    setFolded(
                      folded.includes(p.id) ? folded.filter((x) => x !== p.id) : [...folded, p.id],
                    )
                  }
                  drop={
                    dragging !== null && over === p.id && dragging !== p.id
                      ? order.tree.findIndex((n) => n.project.id === dragging) <
                        order.tree.findIndex((n) => n.project.id === p.id)
                        ? 'below'
                        : 'above'
                      : undefined
                  }
                  onMove={(by) => order.move(p.id, group[at + by])}
                  canMove={{ up: at > 0, down: at < group.length - 1 }}
                  onDelete={() => onDeleteProjects(p)}
                  onDeleteSeveral={() => onDeleteProjects()}
                />
              )
            })}
          </SortableContext>
          <DragOverlay>
            {dragging !== null && (
              <div className="flex h-8 items-center gap-2.5 rounded-lg border border-line bg-raised px-2 text-[13px] shadow-pop">
                {(() => {
                  const p = projects.find((x) => x.id === dragging)
                  return p ? (
                    <>
                      <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={18} />
                      <span className="truncate">{p.name}</span>
                    </>
                  ) : null
                })()}
              </div>
            )}
          </DragOverlay>
        </DndContext>
        {collapsed && (
          <IconButton
            label="New project"
            onClick={onNewProject}
            icon={<Plus className="size-4" />}
            className="w-full"
          />
        )}
        {projects.length === 0 && !collapsed && (
          <button
            onClick={onNewProject}
            className="mx-2 mt-1 text-[13px] text-accent hover:underline"
          >
            Create your first project
          </button>
        )}
      </nav>
      <div className="space-y-0.5 border-t border-line bg-side-fixed p-2">
        {me?.is_admin && (
          <NavLink to="/admin" icon={<Shield />} collapsed={collapsed}>
            Admin
          </NavLink>
        )}
        <NavLink to="/settings" icon={<Settings />} collapsed={collapsed}>
          Settings
        </NavLink>
        {collapsed && (
          <div className="flex justify-center pt-1">
            <UserMenu compact />
          </div>
        )}
      </div>
    </aside>
  )
}

/** The assistants you own (WP-ADR1), for the navigation. */
function useMyAssistants() {
  const all = useQuery({ ...assistantsQuery(), refetchInterval: 60_000 }).data
  return useMemo(() => (all ?? []).filter((a) => a.mine), [all])
}

/** An assistant's icon, with a dot when it needs you or isn't running. */
function AssistantGlyph({ a }: { a: Assistant }) {
  const h = health(a)
  return (
    <span className="relative flex">
      <Sparkles />
      {h !== 'ok' && h !== 'unknown' && (
        <span
          className="absolute -right-0.5 -top-0.5 size-1.5 rounded-full"
          style={{ background: HEALTH_COLOR[h] }}
        />
      )}
    </span>
  )
}

function ThemeIcon({ t }: { t: Theme }) {
  return t === 'dark' ? <Moon /> : t === 'light' ? <Sun /> : <SunMoon />
}

function UserMenu({ compact }: { compact?: boolean }) {
  const me = useQuery(q.me()).data
  const qc = useQueryClient()
  const navigate = useNavigate()
  const m = usePopover()
  const savePrefs = useSavePrefs()
  const [theme, setT] = useState<Theme>(storedTheme())
  const cycle = () => {
    const next: Theme = theme === '' ? 'dark' : theme === 'dark' ? 'light' : ''
    setTheme(next)
    setT(next)
    savePrefs.mutate({ theme: next }) // the local choice already applies; syncing is best-effort
  }
  const logout = () =>
    api.post('/auth/logout').then(() => {
      qc.clear()
      navigate({ to: '/login' })
    }, toastError)
  return (
    <>
      <button
        ref={m.anchor}
        onClick={m.toggle}
        className={clsx(
          'flex min-w-0 items-center gap-2 rounded-lg px-1.5 py-1 hover:bg-hover',
          !compact && 'flex-1',
        )}
      >
        <Avatar user={me} size={24} />
        {!compact && <span className="truncate text-[13px] font-semibold">{me?.display_name}</span>}
      </button>
      <Popover open={m.open} onClose={m.close} anchor={m.anchor}>
        <div className="px-2.5 py-2 text-xs text-muted">
          Signed in as <span className="font-medium text-fg">@{me?.username}</span>
        </div>
        <MenuList
          onClose={m.close}
          items={[
            {
              label: `Theme: ${theme === '' ? 'system' : theme}`,
              icon: <ThemeIcon t={theme} />,
              onSelect: cycle,
            },
            { separator: true, label: '' },
            { label: 'Sign out', icon: <LogOut />, onSelect: logout },
          ]}
        />
      </Popover>
    </>
  )
}

function TimerSidebar({ collapsed }: { collapsed: boolean }) {
  const timer = useQuery(q.timer()).data
  const { stop } = useTimer()
  useTick(1000, !!timer)
  if (!timer) return null
  const secs = (Date.now() - new Date(timer.started_at).getTime()) / 1000
  return (
    <div
      className={clsx(
        'mt-1 flex items-center gap-2 rounded-lg border border-line bg-raised px-2 py-1.5',
        collapsed && 'flex-col px-1',
      )}
    >
      <button
        onClick={() => stop.mutate()}
        title="Stop timer"
        aria-label="Stop timer"
        className="flex size-6 shrink-0 items-center justify-center rounded-md bg-danger/10 text-danger hover:bg-danger/20"
      >
        <Square className="size-3 fill-current" />
      </button>
      {!collapsed ? (
        <div className="min-w-0 flex-1">
          <div className="text-[13px] font-semibold tabular-nums">{clock(secs)}</div>
          {timer.issue ? (
            <Link
              to="/issue/$key"
              params={{ key: timer.issue.key }}
              className="block truncate text-[11px] text-muted hover:text-fg"
            >
              {timer.issue.key} {timer.issue.title}
            </Link>
          ) : (
            <div className="truncate text-[11px] text-muted">{timer.note || 'No issue'}</div>
          )}
        </div>
      ) : (
        <div className="text-[10px] font-semibold tabular-nums">
          {secs < 60
            ? `${Math.floor(secs)}s`
            : secs < 3600
              ? `${Math.floor(secs / 60)}m`
              : clock(secs).replace(/:\d\d$/, '')}
        </div>
      )}
    </div>
  )
}

/**
 * One sidebar project. The whole row drags by pointer or touch (not while the
 * sidebar is collapsed); the keyboard moves it from the row's menu.
 */
function ProjectRow({
  project: p,
  depth,
  hasChildren,
  collapsed,
  active,
  isFolded,
  onFold,
  drop,
  onMove,
  canMove,
  onDelete,
  onDeleteSeveral,
}: {
  project: Project
  depth: number
  hasChildren: boolean
  collapsed: boolean
  active: boolean
  isFolded: boolean
  onFold: () => void
  /** where a dragged project would land relative to this row */
  drop?: 'above' | 'below'
  onMove: (by: -1 | 1) => void
  canMove: { up: boolean; down: boolean }
  onDelete: () => void
  onDeleteSeveral: () => void
}) {
  const { setNodeRef, listeners, isDragging } = useSortable({ id: p.id, disabled: collapsed })
  return (
    <div
      ref={setNodeRef}
      className={clsx(
        'group relative rounded-lg hover:bg-hover',
        active && 'bg-hover',
        isDragging && 'opacity-30',
      )}
      data-project={p.key}
      {...(collapsed ? {} : listeners)}
    >
      {drop && (
        <span
          aria-hidden
          data-drop={drop}
          className={clsx(
            'pointer-events-none absolute inset-x-1 z-10 h-0.5 rounded bg-accent',
            drop === 'above' ? '-top-px' : '-bottom-px',
          )}
        />
      )}
      <Link
        to="/p/$key/overview"
        params={{ key: p.key }}
        draggable={false}
        className={clsx(
          // The fold and "…" buttons sit to the right of the link, not over it.
          'flex h-8 items-center gap-2.5 rounded-lg px-2 text-[13px]',
          !collapsed && (hasChildren ? 'mr-12' : 'mr-6'),
          active ? 'font-medium text-fg' : 'text-muted hover:text-fg',
        )}
        style={collapsed ? undefined : { paddingLeft: 8 + depth * 16 }}
        title={collapsed ? p.name : undefined}
      >
        <ProjectIcon
          icon={p.icon}
          color={p.color}
          name={p.name}
          size={depth && !collapsed ? 16 : 18}
        />
        {!collapsed && (
          <>
            <span className="flex-1 truncate">{p.name}</span>
            <span className="text-[11px] tabular-nums text-subtle opacity-0 group-hover:opacity-100">
              {p.open_issues}
            </span>
          </>
        )}
      </Link>
      {!collapsed && (
        <ProjectRowMenu
          project={p}
          className={hasChildren ? 'right-6' : 'right-1'}
          onMove={onMove}
          canMove={canMove}
          onDelete={onDelete}
          onDeleteSeveral={onDeleteSeveral}
        />
      )}
      {hasChildren && !collapsed && (
        <button
          onClick={onFold}
          aria-label={isFolded ? `Show ${p.name}'s sub-projects` : `Hide ${p.name}'s sub-projects`}
          aria-expanded={!isFolded}
          className="absolute right-1 top-1.5 flex size-5 items-center justify-center rounded text-subtle hover:bg-line hover:text-fg"
        >
          {isFolded ? <ChevronRight className="size-3.5" /> : <ChevronDown className="size-3.5" />}
        </button>
      )}
    </div>
  )
}

/** A sidebar project's "…" menu: shown on hover or focus, always on touch screens. */
function ProjectRowMenu({
  project: p,
  className,
  onMove,
  canMove,
  onDelete,
  onDeleteSeveral,
}: {
  project: Project
  className: string
  onMove: (by: -1 | 1) => void
  canMove: { up: boolean; down: boolean }
  onDelete: () => void
  onDeleteSeveral: () => void
}) {
  const navigate = useNavigate()
  const m = usePopover()
  const owner = p.role === 'owner'
  return (
    <>
      <button
        ref={m.anchor}
        onClick={m.toggle}
        aria-label={`Actions for ${p.name}`}
        aria-haspopup="menu"
        aria-expanded={m.open}
        className={clsx(
          'absolute top-1.5 flex size-5 items-center justify-center rounded text-subtle hover:bg-line hover:text-fg focus-visible:opacity-100 group-hover:opacity-100 [@media(hover:none)]:opacity-100',
          m.open ? 'opacity-100' : 'opacity-0',
          className,
        )}
      >
        <MoreHorizontal className="size-3.5" />
      </button>
      <Popover open={m.open} onClose={m.close} anchor={m.anchor} align="end">
        <MenuList
          onClose={m.close}
          items={[
            {
              label: 'Settings',
              icon: <Settings />,
              onSelect: () => navigate({ to: '/p/$key/settings', params: { key: p.key } }),
            },
            { separator: true, label: '' },
            {
              label: 'Move up',
              icon: <ArrowUp />,
              disabled: !canMove.up,
              onSelect: () => onMove(-1),
            },
            {
              label: 'Move down',
              icon: <ArrowDown />,
              disabled: !canMove.down,
              onSelect: () => onMove(1),
            },
            ...(owner
              ? [
                  { separator: true, label: '' },
                  { label: 'Delete…', icon: <Trash2 />, danger: true, onSelect: onDelete },
                  { label: 'Delete several…', icon: <Trash2 />, onSelect: onDeleteSeveral },
                ]
              : []),
          ]}
        />
      </Popover>
    </>
  )
}

function MobileNav({
  onSearch,
  onCreate,
  onDeleteProjects,
}: {
  onSearch: () => void
  onCreate: () => void
  onDeleteProjects: () => void
}) {
  const unread =
    (useQuery(q.inboxCounts()).data?.live_unread ?? 0) + (useQuery(waitingQuery()).data?.total ?? 0)
  const timer = useQuery(q.timer()).data
  const { stop } = useTimer()
  useTick(1000, !!timer)
  const [projectsOpen, setProjectsOpen] = useState(false)
  const { tree } = useProjectOrder()
  const reviews = useQuery({ ...designReviewQuery(), enabled: projectsOpen }).data?.length ?? 0
  const envsDown = useQuery({ ...envSummaryQuery(), enabled: projectsOpen }).data?.down ?? 0
  const assistants = useMyAssistants()
  const anchor = usePopover()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const showFab = !/^\/(issue|settings|admin|device|assistant)/.test(pathname)
  // The + button floats over the page; it steps aside while the person
  // scrolls down so it never sits on what they're reading (charts, the last
  // row of a list), and comes back when they scroll up.
  const scrollingDown = useScrollingDown(pathname)
  const item =
    'flex flex-1 flex-col items-center justify-center gap-0.5 text-[10px] text-muted data-[status=active]:text-accent [&_svg]:size-5'
  const close = useCallback(() => setProjectsOpen(false), [])
  return (
    <>
      {timer && (
        <div className="fixed inset-x-3 bottom-[68px] z-30 flex items-center gap-2 rounded-xl border border-line bg-raised px-3 py-2 shadow-pop">
          <button
            onClick={() => stop.mutate()}
            aria-label="Stop timer"
            className="flex size-7 items-center justify-center rounded-lg bg-danger/10 text-danger"
          >
            <Square className="size-3 fill-current" />
          </button>
          <span className="text-sm font-semibold tabular-nums">
            {clock((Date.now() - new Date(timer.started_at).getTime()) / 1000)}
          </span>
          {timer.issue && (
            <Link
              to="/issue/$key"
              params={{ key: timer.issue.key }}
              className="min-w-0 flex-1 truncate text-xs text-muted"
            >
              {timer.issue.key} {timer.issue.title}
            </Link>
          )}
        </div>
      )}
      {showFab && (
        <button
          onClick={onCreate}
          aria-label="New issue"
          aria-hidden={scrollingDown || undefined}
          tabIndex={scrollingDown ? -1 : undefined}
          className={clsx(
            'fixed right-4 z-30 flex size-12 items-center justify-center rounded-full bg-accent text-accent-fg shadow-pop transition-[translate,opacity] duration-200 motion-reduce:transition-none',
            timer ? 'bottom-[124px]' : 'bottom-[76px]',
            scrollingDown && 'pointer-events-none translate-y-4 opacity-0',
          )}
        >
          <Plus className="size-6" />
        </button>
      )}
      <nav className="pb-safe fixed inset-x-0 bottom-0 z-30 flex h-[60px] border-t border-line bg-panel/95 backdrop-blur">
        <Link to="/" activeOptions={{ exact: true }} className={item}>
          <Home />
          My work
        </Link>
        <button ref={anchor.anchor} onClick={() => setProjectsOpen(true)} className={item}>
          <FolderKanban />
          Projects
        </button>
        <button onClick={onSearch} className={item}>
          <Search />
          Search
        </button>
        <Link to="/inbox" className={clsx(item, 'relative')}>
          <Bell />
          {unread > 0 && (
            <span className="absolute left-1/2 top-1.5 ml-1 rounded-full bg-accent px-1 text-[9px] font-semibold text-accent-fg">
              {unread}
            </span>
          )}
          Inbox
        </Link>
        <Link to="/settings" className={item}>
          <Settings />
          Me
        </Link>
      </nav>
      <Popover open={projectsOpen} onClose={close} anchor={anchor.anchor}>
        <div className="px-2 pb-1 text-xs font-semibold uppercase tracking-wide text-subtle">
          Projects
        </div>
        {tree.map(({ project: p, depth }) => (
          <Link
            key={p.id}
            to="/p/$key/overview"
            params={{ key: p.key }}
            onClick={close}
            className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
            style={{ paddingLeft: 8 + depth * 20 }}
          >
            <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={24} />
            <span className="flex-1 truncate">{p.name}</span>
            <span className="text-xs text-subtle">{p.open_issues}</span>
          </Link>
        ))}
        <Link
          to="/time"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <Clock className="size-5 text-muted" /> Time tracking
        </Link>
        <Link
          to="/agents"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <Bot className="size-5 text-muted" /> Agents
        </Link>
        {assistants.map((a) => (
          <Link
            key={a.id}
            to="/assistant/$id"
            params={{ id: String(a.id) }}
            onClick={close}
            className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
          >
            <span className="flex size-5 items-center justify-center [&>svg]:size-5">
              <AssistantGlyph a={a} />
            </span>
            <span className="flex-1">{a.name}</span>
            {a.state.pending.length > 0 && (
              <span className="rounded-full bg-warn/15 px-1.5 text-xs font-medium text-warn">
                {a.state.pending.length} waiting
              </span>
            )}
          </Link>
        ))}
        <Link
          to="/environments"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <Server className="size-5 text-muted" />
          <span className="flex-1">Environments</span>
          {envsDown > 0 && (
            <span className="rounded-full bg-danger/15 px-1.5 text-xs font-medium text-danger">
              {envsDown} down
            </span>
          )}
        </Link>
        <Link
          to="/catch-up"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <CircleCheckBig className="size-5 text-muted" />
          <span className="flex-1">Catch up</span>
        </Link>
        <Link
          to="/live"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <Rocket className="size-5 text-muted" />
          <span className="flex-1">What’s live</span>
        </Link>
        <Link
          to="/ci"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <Workflow className="size-5 text-muted" /> CI
        </Link>
        <Link
          to="/pages"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <FileText className="size-5 text-muted" /> Pages
        </Link>
        <Link
          to="/design"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <Lightbulb className="size-5 text-muted" />
          <span className="flex-1">Design</span>
          {reviews > 0 && (
            <span className="rounded-full bg-warn/15 px-1.5 text-xs font-medium text-warn">
              {reviews} to review
            </span>
          )}
        </Link>
        <Link
          to="/glossary"
          onClick={close}
          className="flex h-11 items-center gap-3 rounded-lg px-2 text-sm hover:bg-hover"
        >
          <BookA className="size-5 text-muted" /> Glossary
        </Link>
        {tree.some((n) => n.project.role === 'owner') && (
          <button
            onClick={() => {
              close()
              onDeleteProjects()
            }}
            className="flex h-11 w-full items-center gap-3 rounded-lg px-2 text-sm text-danger hover:bg-hover"
          >
            <Trash2 className="size-5" /> Delete projects…
          </button>
        )}
      </Popover>
    </>
  )
}
