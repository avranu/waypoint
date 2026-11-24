import { useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { createPortal } from 'react-dom'
import { useNavigate, useRouterState } from '@tanstack/react-router'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  BookA,
  Bookmark,
  ClipboardCheck,
  Clock,
  FileText,
  FolderPlus,
  Home,
  Inbox,
  Target,
  Keyboard,
  Lightbulb,
  Moon,
  Play,
  PenLine,
  Plus,
  Search,
  Settings,
  Shield,
  ShieldAlert,
  Square,
  Trash2,
  Sparkles,
} from 'lucide-react'
import { assistantsQuery } from '@/lib/assistant'
import { q, useSavePrefs, useTimer } from '@/lib/queries'
import { useDebounced } from '@/lib/hooks'
import { pagesQuery } from '@/lib/pages'
import { searchAllQuery } from '@/lib/knowledge'
import { KindIcon } from '@/components/knowledge'
import type { Issue, SearchHit } from '@/lib/types'
import { setTheme } from '@/lib/theme'
import { ProjectIcon, StatusIcon, TypeIcon } from '@/components/issue/icons'
import { Kbd } from '@/components/ui/misc'
import { openShortcuts } from './Shortcuts'
import { openCreateIssue } from './CreateIssue'
import { openAuditDialog } from './AuditDialog'
import { useEscapeLayer } from '@/components/ui/layers'
import { useHoverCard } from '@/components/ui/hovercard'
import { IssuePeek } from '@/components/issue/IssuePeek'

interface Item {
  id: string
  group: string
  /** the issue it opens, for its hover card */
  issue?: Issue
  label: ReactNode
  text: string
  icon: ReactNode
  hint?: ReactNode
  run: () => void
}

export function CommandPalette({
  open,
  onClose,
  onCreate,
  onNewProject,
  onDeleteProjects,
}: {
  open: boolean
  onClose: () => void
  onCreate: () => void
  onNewProject: () => void
  onDeleteProjects: () => void
}) {
  if (!open) return null
  return (
    <Palette
      onClose={onClose}
      onCreate={onCreate}
      onNewProject={onNewProject}
      onDeleteProjects={onDeleteProjects}
    />
  )
}

function Palette({
  onClose,
  onCreate,
  onNewProject,
  onDeleteProjects,
}: {
  onClose: () => void
  onCreate: () => void
  onNewProject: () => void
  onDeleteProjects: () => void
}) {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const near = pathname.match(/^\/p\/([^/]+)/)?.[1] ?? pathname.match(/^\/issue\/(.+)-\d+$/)?.[1]
  useEscapeLayer(true, onClose)
  const [text, setText] = useState('')
  const [active, setActive] = useState(0)
  const listRef = useRef<HTMLDivElement>(null)
  const debounced = useDebounced(text.trim(), 120)
  const projects = useQuery(q.projects()).data ?? []
  const views = useQuery(q.views()).data ?? []
  const me = useQuery(q.me()).data
  const timer = useQuery(q.timer()).data
  const { start, stop } = useTimer()
  const savePrefs = useSavePrefs()
  const toggleTheme = () => {
    const next = document.documentElement.classList.contains('dark') ? 'light' : 'dark'
    setTheme(next)
    savePrefs.mutate({ theme: next })
  }
  const assistants = (useQuery(assistantsQuery()).data ?? []).filter((a) => a.mine)
  const search = useQuery({
    ...q.search(debounced || 'is:open order:-updated', {
      limit: debounced ? 30 : 8,
      order: 'updated',
      near,
    }),
    placeholderData: (p) => p,
  })
  const results = search.data

  const pageText = debounced.includes(':') ? '' : debounced
  const pages = useQuery({ ...pagesQuery({ q: pageText }), enabled: !!pageText }).data
  const knowledgeQ = useQuery({
    ...searchAllQuery({ q: pageText, kinds: 'requests,pages,design,glossary', near, limit: 6 }),
    enabled: pageText.length > 1,
    placeholderData: (p) => p,
  })
  const knowledge = knowledgeQ.data
  const settled =
    debounced === text.trim() &&
    !search.isPlaceholderData &&
    !(pageText.length > 1 && knowledgeQ.isPlaceholderData)
  const enterLater = useRef(false)

  const go = (to: string, params?: Record<string, string>) => () =>
    navigate({ to, params } as never)
  const items = useMemo<Item[]>(() => {
    const t = text.trim().toLowerCase()
    const match = (s: string) => !t || s.toLowerCase().includes(t)
    const out: Item[] = []
    const named = new Set<string>()
    for (const h of (pageText && knowledge?.exact) || []) {
      const key = `${h.kind}:${h.id}`
      if (named.has(key)) continue
      named.add(key)
      out.push(matchItem(h, go))
    }
    for (const i of results?.issues ?? []) {
      if (named.has('issue:' + i.id)) continue
      out.push({
        id: 'issue:' + i.key,
        group: debounced ? 'Issues' : 'Recent issues',
        issue: i,
        label: (
          <span className="flex min-w-0 items-center gap-2">
            <span className="w-16 shrink-0 text-xs text-subtle">{i.key}</span>
            <span className="truncate">{i.title}</span>
          </span>
        ),
        text: i.key + ' ' + i.title,
        icon: <StatusIcon category={i.status.category} color={i.status.color} />,
        hint: <TypeIcon type={i.type} />,
        run: go('/issue/$key', { key: i.key }),
      })
    }
    const actions: Omit<Item, 'group'>[] = [
      {
        id: 'new',
        label: 'New issue',
        text: 'new issue create',
        icon: <Plus />,
        hint: <Kbd>C</Kbd>,
        run: onCreate,
      },
      {
        id: 'newd',
        label: 'New draft',
        text: "new draft idea thought capture agents can't see",
        icon: <PenLine />,
        hint: <Kbd>D</Kbd>,
        run: () => openCreateIssue({ draft: true }),
      },
      {
        id: 'audit-week',
        label: 'Audit what landed this week…',
        text: 'audit review grok adversarial last week recent changes merged main security',
        icon: <ShieldAlert />,
        run: () => openAuditDialog({ target: 'recent', since: '7d' }),
      },
      {
        id: 'audit',
        label: 'New audit…',
        text: 'new audit review grok adversarial branch commit range pull request repository',
        icon: <ShieldAlert />,
        run: () => openAuditDialog(),
      },
      {
        id: 'newp',
        label: 'New project',
        text: 'new project create',
        icon: <FolderPlus />,
        run: onNewProject,
      },
      {
        id: 'delp',
        label: 'Delete projects…',
        text: 'delete remove projects clean up',
        icon: <Trash2 />,
        run: onDeleteProjects,
      },
      timer
        ? {
            id: 'stop',
            label: 'Stop timer',
            text: 'stop timer',
            icon: <Square />,
            run: () => stop.mutate(),
          }
        : {
            id: 'start',
            label: 'Start a timer (no issue)',
            text: 'start timer',
            icon: <Play />,
            run: () => start.mutate({}),
          },
      {
        id: 'keys',
        label: 'Keyboard shortcuts',
        text: 'keyboard shortcuts keys help',
        icon: <Keyboard />,
        hint: <Kbd>?</Kbd>,
        run: openShortcuts,
      },
      {
        id: 'theme',
        label: 'Toggle dark mode',
        text: 'theme dark light mode',
        icon: <Moon />,
        run: () => toggleTheme(),
      },
      { id: 'home', label: 'Go to My work', text: 'home my work', icon: <Home />, run: go('/') },
      {
        id: 'inbox',
        label: 'Go to Inbox',
        text: 'inbox notifications',
        icon: <Inbox />,
        run: go('/inbox'),
      },
      {
        id: 'objectives',
        label: 'Go to Objectives',
        text: 'objectives rounds check judge goal loop',
        icon: <Target />,
        run: go('/objectives'),
      },
      {
        id: 'pages',
        label: 'Go to Pages',
        text: 'pages docs notes plans',
        icon: <FileText />,
        run: go('/pages'),
      },
      {
        id: 'design',
        label: 'Go to Design',
        text: 'design notes goals features screens decisions spec',
        icon: <Lightbulb />,
        run: go('/design'),
      },
      {
        id: 'review',
        label: 'Review agents’ design changes',
        text: 'design review approve pending agent',
        icon: <ClipboardCheck />,
        run: go('/design/review'),
      },
      {
        id: 'glossary',
        label: 'Go to Glossary',
        text: 'glossary terms words definitions',
        icon: <BookA />,
        run: go('/glossary'),
      },
      {
        id: 'time',
        label: 'Go to Time tracking',
        text: 'time tracking timesheet',
        icon: <Clock />,
        run: go('/time'),
      },
      {
        id: 'search',
        label: 'Advanced search',
        text: 'search query filter',
        icon: <Search />,
        run: () => navigate({ to: '/search', search: { q: text } }),
      },
      {
        id: 'settings',
        label: 'Settings',
        text: 'settings profile tokens',
        icon: <Settings />,
        run: go('/settings'),
      },
      ...(me?.is_admin
        ? [
            {
              id: 'admin',
              label: 'Admin',
              text: 'admin users invite',
              icon: <Shield />,
              run: go('/admin'),
            },
          ]
        : []),
    ]
    for (const p of projects) {
      if (match(p.name + ' ' + p.key))
        out.push({
          id: 'p:' + p.key,
          group: 'Projects',
          label: p.name,
          text: p.name,
          icon: <ProjectIcon icon={p.icon} color={p.color} name={p.name} size={16} />,
          hint: <span className="text-xs text-subtle">{p.key}</span>,
          run: go('/p/$key/board', { key: p.key }),
        })
    }
    for (const pg of (pageText && pages) || []) {
      if (named.has('page:' + pg.id)) continue
      if (out.filter((o) => o.group === 'Pages').length >= 8) break
      out.push({
        id: 'pg:' + pg.id,
        group: 'Pages',
        label: pg.title,
        text: pg.title,
        icon: <FileText />,
        hint: <span className="text-xs text-subtle">{pg.project_key ?? 'personal'}</span>,
        run: go('/pages/$id', { id: String(pg.id) }),
      })
    }
    for (const g of (pageText && knowledge?.groups) || [])
      for (const h of g.hits) {
        if (named.has(`${g.kind}:${h.id}`)) continue
        out.push({
          id: `${g.kind}:${h.id}`,
          group: g.kind === 'design' ? 'Design notes' : 'Glossary',
          label: h.title,
          text: h.title,
          icon: g.kind === 'design' ? <KindIcon kind={h.sub ?? 'note'} /> : <BookA />,
          hint: <span className="text-xs text-subtle">{h.project_key ?? 'global'}</span>,
          run: go(g.kind === 'design' ? '/design/$id' : '/glossary/$id', { id: String(h.id) }),
        })
      }
    for (const v of views)
      if (match(v.name))
        out.push({
          id: 'v:' + v.id,
          group: 'Views',
          label: v.name,
          text: v.name,
          icon: <Bookmark />,
          run: go('/views/$id', { id: String(v.id) }),
        })
    for (const a of actions)
      if (match(a.text + ' ' + (typeof a.label === 'string' ? a.label : '')))
        out.push({ ...a, group: 'Actions' })
    for (const a of assistants) {
      const ask = text.trim()
      if (ask && !ask.includes(':'))
        out.push({
          id: 'ask:' + a.id,
          group: 'Assistant',
          label: (
            <span className="truncate">
              Ask {a.name}: <span className="text-muted">“{ask}”</span>
            </span>
          ),
          text: ask,
          icon: <Sparkles />,
          run: () =>
            navigate({
              to: '/assistant/$id',
              params: { id: String(a.id) },
              search: { ask, send: '1' },
            }),
        })
      else if (match(`chat ${a.name} assistant`))
        out.push({
          id: 'chat:' + a.id,
          group: 'Assistant',
          label: `Chat with ${a.name}`,
          text: `chat ${a.name} assistant`,
          icon: <Sparkles />,
          run: go('/assistant/$id', { id: String(a.id) }),
        })
    }
    if (!debounced)
      out.sort(
        (a, b) => (a.group === 'Recent issues' ? 1 : 0) - (b.group === 'Recent issues' ? 1 : 0),
      )
    return out
  }, [results, projects, views, pages, knowledge, pageText, text, debounced, timer, me, assistants])

  useEffect(() => setActive(0), [debounced])
  useEffect(() => {
    if (!settled || !enterLater.current) return
    enterLater.current = false
    if (items[active]) run(items[active])
  })
  useEffect(() => {
    listRef.current?.querySelector(`[data-idx="${active}"]`)?.scrollIntoView({ block: 'nearest' })
  }, [active])
  useEffect(() => {
    for (const i of items.slice(0, 5))
      if (i.id.startsWith('issue:'))
        qc.prefetchQuery({ ...q.issue(i.id.slice(6)), staleTime: 15_000 })
  }, [items, qc])

  const hover = useHoverCard<Issue>()
  const run = (i: Item) => {
    onClose()
    i.run()
  }
  let lastGroup = ''
  return createPortal(
    <div className="fixed inset-0 z-50 flex items-start justify-center p-0 sm:p-4 sm:pt-[12vh]">
      <div className="absolute inset-0 bg-black/40" onClick={onClose} />
      <div className="animate-pop relative flex h-full w-full flex-col overflow-hidden border-line bg-raised shadow-pop sm:h-auto sm:max-h-[70vh] sm:max-w-xl sm:rounded-2xl 2xl:max-w-2xl sm:border">
        <div className="flex items-center gap-3 border-b border-line px-4">
          <Search className="size-4 text-subtle" />
          <input
            autoFocus
            role="combobox"
            aria-expanded
            aria-controls="palette-list"
            aria-activedescendant={items[active] ? `palette-${active}` : undefined}
            aria-autocomplete="list"
            value={text}
            onChange={(e) => {
              enterLater.current = false
              setText(e.target.value)
            }}
            placeholder="Search issues, notes, terms, actions…  (try is:open assignee:me)"
            className="h-14 flex-1 bg-transparent text-[15px] outline-none placeholder:text-subtle"
            onKeyDown={(e) => {
              if (e.key === 'ArrowDown') {
                e.preventDefault()
                setActive((a) => Math.min(a + 1, items.length - 1))
              } else if (e.key === 'ArrowUp') {
                e.preventDefault()
                setActive((a) => Math.max(a - 1, 0))
              } else if (e.key === 'Enter') {
                e.preventDefault()
                if (!settled) enterLater.current = true
                else if (items[active]) run(items[active])
              }
            }}
          />
          <button onClick={onClose} className="text-xs text-subtle sm:hidden">
            Cancel
          </button>
          <span className="hidden sm:inline">
            <Kbd>esc</Kbd>
          </span>
        </div>
        <div ref={listRef} id="palette-list" role="listbox" className="flex-1 overflow-y-auto p-2">
          {hover.render((i) => (
            <IssuePeek issue={i} />
          ))}
          {items.map((it, idx) => {
            const header = it.group !== lastGroup ? it.group : null
            lastGroup = it.group
            return (
              <div key={it.id}>
                {header && (
                  <div className="px-2.5 pb-1 pt-2 text-[11px] font-semibold uppercase tracking-wide text-subtle">
                    {header}
                  </div>
                )}
                <button
                  id={`palette-${idx}`}
                  role="option"
                  aria-selected={idx === active}
                  data-idx={idx}
                  onMouseMove={() => setActive(idx)}
                  {...(it.issue ? hover.trigger(it.issue) : {})}
                  onClick={() => run(it)}
                  className={clsx(
                    'flex h-10 w-full items-center gap-3 rounded-lg px-2.5 text-left text-[13px]',
                    idx === active && 'bg-hover',
                  )}
                >
                  <span className="flex size-4 shrink-0 items-center justify-center text-muted [&>svg]:size-4">
                    {it.icon}
                  </span>
                  <span className="min-w-0 flex-1 truncate">{it.label}</span>
                  {it.hint}
                </button>
              </div>
            )
          })}
          {items.length === 0 && (
            <div className="px-3 py-8 text-center text-[13px] text-subtle">
              No matches. Press ↵ on “Advanced search” to use the full query language.
            </div>
          )}
        </div>
      </div>
    </div>,
    document.body,
  )
}

/** A thing a code in the search names, as a palette row. */
function matchItem(
  h: SearchHit,
  go: (to: string, params?: Record<string, string>) => () => void,
): Item {
  const id = String(h.id)
  const [icon, run] =
    h.kind === 'design'
      ? [<KindIcon key="i" kind={h.sub ?? 'note'} />, go('/design/$id', { id })]
      : h.kind === 'page'
        ? [<FileText key="i" />, go('/pages/$id', { id })]
        : h.kind === 'request'
          ? [<Inbox key="i" />, go('/requests/$code', { code: h.key ?? id })]
          : h.kind === 'glossary'
            ? [<BookA key="i" />, go('/glossary/$id', { id })]
            : [<Search key="i" />, go('/issue/$key', { key: h.key ?? id })]
  return {
    id: `${h.kind}:${h.id}`,
    group: 'Matches',
    label: (
      <span className="flex min-w-0 items-center gap-2">
        {h.key && <span className="shrink-0 text-xs tabular-nums text-subtle">{h.key}</span>}
        <span className="truncate">{h.title}</span>
      </span>
    ),
    text: h.title,
    icon,
    hint: <span className="text-xs text-subtle">{h.project_key ?? 'global'}</span>,
    run,
  }
}
