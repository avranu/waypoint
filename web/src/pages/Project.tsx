import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type CSSProperties,
} from 'react'
import { createPortal } from 'react-dom'
import {
  Link,
  Outlet,
  useNavigate,
  useParams,
  useRouterState,
  useSearch,
} from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { setShowAgentMade, useShowAgentMade } from '@/lib/agents'
import {
  BarChart3,
  BookA,
  Bot,
  Ellipsis,
  CalendarRange,
  Columns3,
  LayoutDashboard,
  FileText,
  Lightbulb,
  Target,
  ListFilter,
  ListTodo,
  Plus,
  Rows3,
  Search,
  Server,
  Settings2,
  X,
} from 'lucide-react'
import { q } from '@/lib/queries'
import { backgroundCSS, contrastText, useProjectLook } from '@/lib/theme'
import { useDebounced, useHotkey } from '@/lib/hooks'
import { useEscapeLayer } from '@/components/ui/layers'
import { Button, IconButton, ToggleButton } from '@/components/ui/button'
import { Avatar } from '@/components/ui/misc'
import { MenuList, Popover, usePopover } from '@/components/ui/popover'
import { ProjectIcon } from '@/components/issue/icons'
import { IssueDetailView } from '@/components/issue/IssueDetail'
import { openCreateIssue } from '@/components/CreateIssue'
import type { Project } from '@/lib/types'

/** Accent color and background for a project, as CSS variables. */
export function projectStyle(p: Pick<Project, 'color'>): CSSProperties {
  return { '--wp-accent': p.color, '--wp-accent-fg': contrastText(p.color) } as CSSProperties
}

export function useProject(): Project {
  const { key } = useParams({ strict: false }) as { key: string }
  return useQuery(q.project(key)).data!
}

const tabs = [
  { to: '/p/$key/overview', label: 'Overview', icon: LayoutDashboard },
  { to: '/p/$key/board', label: 'Board', icon: Columns3 },
  { to: '/p/$key/backlog', label: 'Backlog', icon: ListTodo },
  { to: '/p/$key/list', label: 'List', icon: Rows3 },
  { to: '/p/$key/timeline', label: 'Timeline', icon: CalendarRange },
  { to: '/p/$key/agents', label: 'Agents', icon: Bot },
  { to: '/p/$key/reports', label: 'Reports', icon: BarChart3 },
  { to: '/p/$key/environments', label: 'Environments', icon: Server },
  { to: '/p/$key/pages', label: 'Pages', icon: FileText },
  { to: '/p/$key/goals', label: 'Goals', icon: Target },
  { to: '/p/$key/design', label: 'Design', icon: Lightbulb },
  { to: '/p/$key/glossary', label: 'Glossary', icon: BookA },
  { to: '/p/$key/settings', label: 'Settings', icon: Settings2 },
] as const

const tabClass =
  'flex h-9 shrink-0 items-center gap-1.5 whitespace-nowrap border-b-2 border-transparent px-2 text-[13px] text-muted hover:text-fg data-[status=active]:border-accent data-[status=active]:font-medium data-[status=active]:text-fg'

/**
 * The tab strip: every tab that fits, then a "More" menu with the rest, so
 * none is clipped (PL4). A hidden copy of the strip is measured; the tab you
 * are on stays visible.
 */
function ProjectTabs({
  project,
  tabs: all,
}: {
  project: string
  tabs: readonly (typeof tabs)[number][]
}) {
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const box = useRef<HTMLDivElement>(null)
  const ruler = useRef<HTMLDivElement>(null)
  const [fit, setFit] = useState(all.length)
  const more = usePopover()
  const measure = useCallback(() => {
    const width = box.current?.clientWidth ?? 0
    const sizes = Array.from(ruler.current?.children ?? []).map(
      (c) => (c as HTMLElement).offsetWidth + 4,
    )
    if (!width || sizes.length === 0) return
    const moreW = 76
    let used = 0
    let n = 0
    for (const w of sizes.slice(0, all.length)) {
      if (used + w > width) break
      used += w
      n++
    }
    if (n < all.length) {
      n = 0
      used = 0
      for (const w of sizes.slice(0, all.length)) {
        if (used + w + moreW > width) break
        used += w
        n++
      }
    }
    setFit(n)
  }, [all.length])
  useLayoutEffect(measure, [measure])
  useEffect(() => {
    const el = box.current
    if (!el) return
    const ro = new ResizeObserver(measure)
    ro.observe(el)
    for (const c of Array.from(ruler.current?.children ?? [])) ro.observe(c)
    void document.fonts?.ready.then(measure)
    return () => ro.disconnect()
  }, [measure])
  const href = (to: string) => to.replace('$key', project)
  const active = (to: string) => pathname === href(to) || pathname.startsWith(href(to) + '/')
  let shown = all.slice(0, fit)
  const here = all.findIndex((t) => active(t.to))
  if (here >= fit && fit > 0) shown = [...all.slice(0, fit - 1), all[here]]
  const rest = all.filter((t) => !shown.includes(t))
  const nav = useNavigate()
  return (
    <div ref={box} className="relative -mb-px">
      <div
        ref={ruler}
        aria-hidden
        className="pointer-events-none invisible absolute left-0 top-0 flex h-0 gap-1 overflow-hidden"
      >
        {all.map((t) => (
          <span key={t.to} className={tabClass}>
            <t.icon className="size-3.5" />
            {t.label}
          </span>
        ))}
      </div>
      <nav className="flex gap-1" aria-label="Project sections">
        {shown.map((t) => (
          <Link key={t.to} to={t.to} params={{ key: project }} className={tabClass}>
            <t.icon className="size-3.5" />
            {t.label}
          </Link>
        ))}
        {rest.length > 0 && (
          <>
            <button
              ref={more.anchor}
              onClick={more.toggle}
              aria-haspopup="menu"
              aria-expanded={more.open}
              className={tabClass}
            >
              <Ellipsis className="size-3.5" />
              More
            </button>
            <Popover open={more.open} onClose={more.close} anchor={more.anchor} align="end">
              <MenuList
                onClose={more.close}
                items={rest.map((t) => ({
                  label: t.label,
                  icon: <t.icon />,
                  onSelect: () => nav({ to: t.to, params: { key: project } } as never),
                }))}
              />
            </Popover>
          </>
        )}
      </nav>
    </div>
  )
}

export function ProjectLayout() {
  const { key } = useParams({ from: '/app/p/$key' })
  const project = useQuery(q.project(key)).data
  const search = useSearch({ strict: false }) as { issue?: string }
  const navigate = useNavigate()
  const closeIssue = () =>
    navigate({
      to: '.',
      search: (s: Record<string, unknown>) => ({ ...s, issue: undefined }),
    } as never)
  useEffect(() => {
    if (project) document.title = `${project.name} · Waypoint`
  }, [project])
  const look = useProjectLook(key, {
    color: project?.color ?? '',
    background: project?.background ?? '',
  })
  if (!project) return null
  const canCreate = project.role !== 'viewer' && project.role !== 'guest'
  return (
    <div
      className="relative flex min-h-0 flex-1 flex-col"
      style={projectStyle(look)}
      data-bg={look.background ? '' : undefined}
    >
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0"
        style={{ background: backgroundCSS(look.background) }}
      />
      <header className="wp-surface-blur relative z-[1] flex shrink-0 flex-col gap-2 border-b border-line bg-panel/80 px-3 pt-2.5 backdrop-blur sm:px-5">
        <div className="flex items-center gap-2.5">
          <ProjectIcon icon={project.icon} color={project.color} name={project.name} size={26} />
          <div className="min-w-0 flex-1">
            <h1 className="truncate text-[15px] font-semibold leading-tight">{project.name}</h1>
            <div className="truncate text-xs text-subtle">
              {project.key} · {project.open_issues} open
              {project.role === 'guest' ? ' · shared with you' : ''}
            </div>
          </div>
          {canCreate && (
            <Button
              variant="primary"
              size="sm"
              icon={<Plus className="size-3.5" />}
              onClick={() => openCreateIssue({ project: project.key })}
              className="hidden sm:inline-flex"
            >
              New issue
            </Button>
          )}
        </div>
        <ProjectTabs
          project={project.key}
          tabs={tabs.filter(
            (t) => project.role !== 'guest' || t.label === 'Board' || t.label === 'List',
          )}
        />
      </header>
      <div className="relative z-[1] flex min-h-0 flex-1 flex-col">
        <Outlet />
      </div>
      {search.issue && <IssueSheet issueKey={search.issue} onClose={closeIssue} />}
    </div>
  )
}

/** The issue detail as a side sheet over the current page. */
export function IssueSheet({ issueKey, onClose }: { issueKey: string; onClose: () => void }) {
  const leaving = useRef<HTMLElement | null>(null)
  useEscapeLayer(true, () => {
    const t = document.activeElement as HTMLElement | null
    if (!t || !(t.isContentEditable || ['INPUT', 'TEXTAREA', 'SELECT'].includes(t.tagName)))
      return onClose()
    if (leaving.current === t) return onClose()
    leaving.current = t
    setTimeout(() => {
      if (document.activeElement === t) t.blur()
      leaving.current = null
    })
    return false
  })
  return createPortal(
    <div className="fixed inset-0 z-40 flex justify-end">
      <div className="absolute inset-0 bg-black/20 md:bg-black/10" onClick={onClose} />
      <div
        role="dialog"
        aria-label={issueKey}
        className="animate-sheet relative h-full w-full overflow-y-auto border-l border-line bg-panel shadow-pop md:w-[min(820px,calc(100vw-80px))]"
      >
        <IssueDetailView key={issueKey} issueKey={issueKey} mode="sheet" onClose={onClose} />
      </div>
    </div>,
    document.body,
  )
}

/** Open an issue in the sheet by setting ?issue=. */
export function useOpenIssue() {
  const navigate = useNavigate()
  return useCallback(
    (key: string) =>
      navigate({
        to: '.',
        search: (s: Record<string, unknown>) => ({ ...s, issue: key }),
      } as never),
    [navigate],
  )
}

/** Filter input + member toggles bound to the ?q= search param. */
export function FilterBar({
  project,
  placeholder = 'Filter…',
  extra,
}: {
  project: Project
  placeholder?: string
  extra?: React.ReactNode
}) {
  const navigate = useNavigate()
  const current = (useSearch({ strict: false }) as { q?: string }).q ?? ''
  const [text, setText] = useState(current)
  const debounced = useDebounced(text, 250)
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  useEffect(() => setText(current), [current, pathname])
  useEffect(() => {
    if (debounced !== current)
      navigate({
        to: '.',
        search: (s: Record<string, unknown>) => ({ ...s, q: debounced || undefined }),
        replace: true,
      } as never)
  }, [debounced]) // eslint-disable-line react-hooks/exhaustive-deps
  const assignees = useMemo(
    () => new Set([...text.matchAll(/assignee:([\w.,-]+)/g)].flatMap((m) => m[1].split(','))),
    [text],
  )
  const [open, setOpen] = useState(!!current)
  const input = useRef<HTMLInputElement>(null)
  const expanded = open || !!text
  const reveal = () => {
    setOpen(true)
    requestAnimationFrame(() => input.current?.focus())
  }
  useHotkey('f', reveal)
  const toggleAssignee = (u: string) => {
    const set = new Set(assignees)
    if (set.has(u)) set.delete(u)
    else set.add(u)
    const rest = text.replace(/\s*assignee:[\w.,-]+/g, '').trim()
    setText([rest, set.size ? `assignee:${[...set].join(',')}` : ''].filter(Boolean).join(' '))
  }
  return (
    <div className="wp-surface flex items-center gap-2 px-3 py-2 sm:px-5">
      {expanded ? (
        <div className="relative min-w-0 flex-1 sm:max-w-md">
          <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-subtle" />
          <input
            ref={input}
            value={text}
            onChange={(e) => setText(e.target.value)}
            onBlur={() => !text.trim() && setOpen(false)}
            onKeyDown={(e) => {
              if (e.key === 'Escape' && !text) e.currentTarget.blur()
            }}
            placeholder={placeholder}
            aria-label="Filter issues"
            className="h-8 w-full rounded-lg border border-line bg-panel pl-8 pr-7 text-[13px] outline-none placeholder:text-subtle focus:border-accent"
          />
          {text && (
            <IconButton
              label="Clear filter"
              size="xs"
              onMouseDown={(e) => e.preventDefault()}
              onClick={() => {
                setText('')
                input.current?.focus()
              }}
              className="absolute right-1 top-1/2 -translate-y-1/2"
              icon={<X className="size-3.5" />}
            />
          )}
        </div>
      ) : (
        <IconButton label="Filter (F)" onClick={reveal} icon={<ListFilter className="size-4" />} />
      )}
      <div className="flex items-center -space-x-1">
        {project.members?.map((m) => (
          <button
            key={m.user.id}
            onClick={() => toggleAssignee(m.user.username)}
            title={`Filter: ${m.user.display_name}`}
            className={clsx(
              'rounded-full transition-[opacity,transform]',
              assignees.size && !assignees.has(m.user.username) ? 'opacity-40' : '',
              assignees.has(m.user.username) && 'z-[1] scale-110',
            )}
          >
            <Avatar user={m.user} size={26} ring />
          </button>
        ))}
      </div>
      <AgentMadeToggle projectKey={project.key} />
      {extra && <div className="ml-auto flex items-center">{extra}</div>}
    </div>
  )
}

/** Shows or hides issues agents created for their own tracking. */
function AgentMadeToggle({ projectKey }: { projectKey: string }) {
  const shown = useShowAgentMade(projectKey)
  return (
    <ToggleButton
      label="Show issues agents made"
      hint="Issues agents create for their own tracking"
      pressed={shown}
      onPressedChange={(v) => setShowAgentMade(projectKey, v)}
      icon={<Bot className="size-3.5" />}
      text="Agents"
      textFrom="md"
    />
  )
}
