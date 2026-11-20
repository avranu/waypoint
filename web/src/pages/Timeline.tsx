// A Gantt-style timeline: issues with dates as bars you can drag to move or
// resize; epics show the span of their work.

import { useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { useAgentFilter } from '@/lib/agents'
import {
  CalendarRange,
  ChevronDown,
  ChevronLeft,
  ChevronRight,
  Crosshair,
  Spline,
} from 'lucide-react'
import { pageQ } from '@/lib/queries'
import { localISO, shortDate } from '@/lib/format'
import { useLocal, useMediaQuery } from '@/lib/hooks'
import { Button, IconButton, ToggleButton } from '@/components/ui/button'
import { Empty, LoadError } from '@/components/ui/misc'
import { DatePicker } from '@/components/issue/pickers'
import { StatusIcon, TypeIcon } from '@/components/issue/icons'
import { useIssueEdit } from '@/components/issue/edit'
import { FilterBar, useOpenIssue, useProject } from './Project'
import { useSearch } from '@tanstack/react-router'
import { isClosed, type Issue, type Project } from '@/lib/types'

const DAY = 86400000
type Zoom = 'week' | 'month' | 'quarter'
const DAY_PX: Record<Zoom, number> = { week: 44, month: 18, quarter: 7 }

const parse = (d: string) => new Date(d + 'T00:00:00')
// Calendar days, not 24h steps: a DST change must not shift a date by one.
const addDays = (d: Date, n: number) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + n)
const diffDays = (a: Date, b: Date) => Math.round((a.getTime() - b.getTime()) / DAY)

interface Row {
  issue: Issue
  depth: number
  start: string | null
  end: string | null
  derived: boolean // span computed from children
  hasKids: boolean
}

export function TimelinePage() {
  const project = useProject()
  const search = useSearch({ strict: false }) as { q?: string; issue?: string }
  const filter = search.q ?? ''
  const agentQ = useAgentFilter(project.key, filter)
  const tl = useQuery({
    ...pageQ.timeline(project.key, agentQ),
    placeholderData: (p) => p,
  })
  const res = tl.data
  const [zoom, setZoom] = useLocal<Zoom>('timeline-zoom', 'month')
  const [collapsed, setCollapsed] = useState<Set<number>>(new Set())
  const scroller = useRef<HTMLDivElement>(null)
  const open = useOpenIssue()
  const editable = project.role === 'owner' || project.role === 'editor'
  const px = DAY_PX[zoom]
  const label = useMediaQuery('(max-width: 639px)') ? 150 : 260

  const issues = res?.issues ?? []
  const { rows, unscheduled } = useMemo(() => buildRows(issues, collapsed), [issues, collapsed])

  // Visible range: from a little before the earliest date to a little after the latest.
  const today = parse(localISO(new Date()))
  const range = useMemo(() => {
    const dates = rows
      .flatMap((r) => [r.start, r.end])
      .filter(Boolean)
      .map((d) => parse(d!))
    for (const s of project.sprints ?? []) {
      if (s.start_date) dates.push(parse(s.start_date))
      if (s.end_date) dates.push(parse(s.end_date))
    }
    dates.push(today)
    const min = new Date(Math.min(...dates.map((d) => d.getTime())))
    const max = new Date(Math.max(...dates.map((d) => d.getTime())))
    const pad = zoom === 'week' ? 7 : zoom === 'month' ? 21 : 45
    const start = addDays(min, -pad)
    start.setDate(start.getDate() - ((start.getDay() + 6) % 7)) // Monday
    return { start, days: diffDays(max, start) + pad * 2 }
  }, [rows, project.sprints, zoom]) // eslint-disable-line react-hooks/exhaustive-deps

  const scrollToToday = () => {
    const el = scroller.current
    if (el) el.scrollLeft = diffDays(today, range.start) * px - el.clientWidth / 3
  }
  // On first show and zoom changes only, so a drag's refetch doesn't yank the view.
  const hasRows = rows.length > 0
  useEffect(scrollToToday, [zoom, hasRows]) // eslint-disable-line react-hooks/exhaustive-deps

  const width = range.days * px
  const ticks = useMemo(() => buildTicks(range.start, range.days, zoom), [range, zoom])
  const [showDeps, setShowDeps] = useLocal('timeline-deps', true)
  const hasDeps = rows.some((r) => blocksOf(r.issue).length > 0)
  const live = useMemo(liveStore, [])

  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <FilterBar
        project={project}
        placeholder="Filter the timeline…"
        extra={
          <div className="flex items-center gap-1">
            {hasDeps && (
              <ToggleButton
                label="Dependencies"
                hint="Lines between issues that block each other"
                pressed={showDeps}
                onPressedChange={setShowDeps}
                icon={<Spline className="size-4" />}
              />
            )}
            <div className="flex rounded-lg border border-line bg-panel p-0.5">
              {(['week', 'month', 'quarter'] as Zoom[]).map((z) => (
                <button
                  key={z}
                  aria-label={z}
                  onClick={() => setZoom(z)}
                  className={clsx(
                    'h-7 rounded-md px-2.5 text-xs capitalize',
                    zoom === z ? 'bg-hover font-medium' : 'text-muted',
                  )}
                >
                  <span className="sm:hidden">{z[0]}</span>
                  <span className="hidden sm:inline">{z}</span>
                </button>
              ))}
            </div>
            <IconButton
              label="Scroll left"
              className="hidden sm:inline-flex"
              onClick={() => scroller.current?.scrollBy({ left: -300, behavior: 'smooth' })}
              icon={<ChevronLeft className="size-4" />}
            />
            <Button
              size="sm"
              variant="ghost"
              icon={<Crosshair className="size-3.5" />}
              onClick={scrollToToday}
            >
              Today
            </Button>
            <IconButton
              label="Scroll right"
              className="hidden sm:inline-flex"
              onClick={() => scroller.current?.scrollBy({ left: 300, behavior: 'smooth' })}
              icon={<ChevronRight className="size-4" />}
            />
          </div>
        }
      />
      {res && res.total > issues.length && (
        <p className="px-5 py-1.5 text-xs text-subtle">
          Showing the first {issues.length} of {res.total} open issues.
        </p>
      )}
      {tl.isError && !res ? (
        <LoadError what="The timeline" error={tl.error} />
      ) : rows.length === 0 && unscheduled.length === 0 ? (
        <Empty icon={<CalendarRange />} title="Nothing to plan yet">
          Give issues a start or due date and they appear here as bars you can drag.
        </Empty>
      ) : (
        <div className="min-h-0 flex-1 overflow-y-auto px-3 pb-6 sm:px-5">
          {rows.length > 0 && (
            <div
              className="max-h-[calc(100dvh-230px)] overflow-auto rounded-xl border border-line bg-panel"
              ref={scroller}
            >
              <div
                className="relative isolate"
                style={{ width: width + label, '--label': `${label}px` } as React.CSSProperties}
              >
                {/* Header: sticky labels column + date ticks */}
                <div className="sticky top-0 z-[3] flex border-b border-line bg-panel/95 backdrop-blur">
                  <div className="sticky left-0 z-[2] w-[var(--label)] shrink-0 border-r border-line bg-panel px-3 py-2 text-xs font-semibold text-subtle">
                    Issue
                  </div>
                  <div className="relative h-12 shrink-0" style={{ width }}>
                    {ticks.major.map((t) => (
                      <div
                        key={t.x}
                        className="absolute top-1 text-[11px] font-medium text-muted"
                        style={{ left: t.x * px + 4 }}
                      >
                        {t.label}
                      </div>
                    ))}
                    {ticks.minor.map((t) => (
                      <div
                        key={t.x}
                        className="absolute bottom-1 text-[10px] text-subtle"
                        style={{ left: t.x * px + 2 }}
                      >
                        {t.label}
                      </div>
                    ))}
                    {(project.sprints ?? []).map(
                      (s) =>
                        s.start_date &&
                        s.end_date && (
                          <div
                            key={s.id}
                            title={s.name}
                            role="img"
                            aria-label={`Sprint ${s.name}`}
                            className={clsx(
                              'absolute top-[22px] h-1.5 rounded-full',
                              s.state === 'active' ? 'bg-accent' : 'bg-line-strong',
                            )}
                            style={{
                              left: diffDays(parse(s.start_date), range.start) * px,
                              width: (diffDays(parse(s.end_date), parse(s.start_date)) + 1) * px,
                            }}
                          />
                        ),
                    )}
                  </div>
                </div>
                {/* Grid lines and today */}
                <div
                  className="pointer-events-none absolute bottom-0 left-[var(--label)] top-12"
                  style={{ width }}
                >
                  {ticks.minor.map((t) => (
                    <div
                      key={t.x}
                      className="absolute inset-y-0 w-px bg-line/60"
                      style={{ left: t.x * px }}
                    />
                  ))}
                  <div
                    className="absolute inset-y-0 w-0.5 bg-danger/70"
                    style={{ left: diffDays(today, range.start) * px + px / 2 }}
                  />
                </div>
                {showDeps && hasDeps && (
                  <DependencyLines
                    rows={rows}
                    origin={range.start}
                    px={px}
                    width={width}
                    live={live}
                    selected={search.issue}
                  />
                )}
                {rows.map((r) => (
                  <TimelineRow
                    key={r.issue.id}
                    row={r}
                    project={project}
                    origin={range.start}
                    px={px}
                    width={width}
                    editable={editable && !r.derived}
                    live={showDeps && hasDeps ? live : undefined}
                    onOpen={open}
                    collapsed={collapsed.has(r.issue.id)}
                    onToggle={() => {
                      const s = new Set(collapsed)
                      if (s.has(r.issue.id)) s.delete(r.issue.id)
                      else s.add(r.issue.id)
                      setCollapsed(s)
                    }}
                  />
                ))}
              </div>
            </div>
          )}
          {unscheduled.length > 0 && (
            <Unscheduled issues={unscheduled} project={project} editable={editable} onOpen={open} />
          )}
        </div>
      )}
    </div>
  )
}

// Issues this one blocks, when the API includes them.
const blocksOf = (i: Issue) => i.blocks ?? []

const HEADER_H = 49 // the date header, border included
const ROW_H = 40

/** A bar's days (from the timeline's origin) while it's being dragged. */
interface Span {
  s: number
  e: number
}

/**
 * What the dependency lines follow between renders: bars mid-drag and the
 * hovered issue. Kept outside React state so a drag re-renders the lines,
 * not every row.
 */
function liveStore() {
  let preview = new Map<number, Span>()
  let hover: string | null = null
  let version = 0
  const subs = new Set<() => void>()
  const bump = () => {
    version++
    subs.forEach((f) => f())
  }
  return {
    subscribe: (f: () => void) => {
      subs.add(f)
      return () => void subs.delete(f)
    },
    version: () => version,
    preview: (id: number) => preview.get(id),
    hover: () => hover,
    setPreview(id: number, span: Span | null) {
      const cur = preview.get(id)
      if (span ? cur?.s === span.s && cur?.e === span.e : !cur) return
      preview = new Map(preview)
      if (span) preview.set(id, span)
      else preview.delete(id)
      bump()
    },
    setHover(key: string | null) {
      if (hover === key) return
      hover = key
      bump()
    },
  }
}
type Live = ReturnType<typeof liveStore>

/** Whether a row is drawn as a due-date diamond rather than a bar. */
const isMilestone = (r: Row) => !r.issue.start_date && !r.derived

/** A row's left and right edges in px, matching how TimelineRow draws it. */
function edges(r: Row, span: Span, px: number): [number, number] {
  if (isMilestone(r)) {
    const c = span.e * px + px / 2
    return [c - 8, c + 8]
  }
  const left = span.s * px + 1
  return [left, left + Math.max(px, (span.e - span.s + 1) * px - 2)]
}

/**
 * Arrows from each blocker's end to the start of what it blocks, under
 * everything else in the timeline (the sticky labels and row borders too). Muted until either end is hovered or open; red and dashed when the
 * blocked issue starts before its blocker ends. Only rows on screen get
 * lines (a blocker in a collapsed group or filtered out has none).
 */
function DependencyLines({
  rows,
  origin,
  px,
  width,
  live,
  selected,
}: {
  rows: Row[]
  origin: Date
  px: number
  width: number
  live: Live
  selected?: string
}) {
  useSyncExternalStore(live.subscribe, live.version)
  const focus = live.hover() ?? selected ?? null
  const index = new Map(rows.map((r, n) => [r.issue.key, n]))
  const span = (r: Row): Span =>
    live.preview(r.issue.id) ?? {
      s: diffDays(parse(r.start!), origin),
      e: diffDays(parse(r.end!), origin),
    }
  const lines: { d: string; late: boolean; hot: boolean; key: string }[] = []
  rows.forEach((r, n) => {
    for (const k of blocksOf(r.issue)) {
      const m = index.get(k)
      if (m === undefined || m === n) continue
      const to = rows[m]
      const from = span(r)
      const into = span(to)
      const x1 = edges(r, from, px)[1]
      const x2 = edges(to, into, px)[0]
      const y1 = n * ROW_H + ROW_H / 2
      const y2 = m * ROW_H + ROW_H / 2
      // Forward links curve gently; backward ones loop out and back.
      const bend = Math.max(14, Math.min(48, Math.abs(x2 - x1) / 2))
      lines.push({
        key: `${r.issue.key}>${k}`,
        late: into.s <= from.e,
        hot: focus === r.issue.key || focus === k,
        d: `M${x1},${y1} C${x1 + bend},${y1} ${x2 - bend},${y2} ${x2 - 1},${y2}`,
      })
    }
  })
  if (!lines.length) return null
  // Hot lines last, so they draw over the rest.
  lines.sort((a, b) => Number(a.hot) - Number(b.hot))
  const dim = focus !== null && lines.some((l) => l.hot)
  const color = (l: (typeof lines)[number]) =>
    l.late ? 'var(--wp-danger)' : l.hot ? 'var(--wp-accent)' : 'var(--wp-muted)'
  return (
    <svg
      aria-hidden
      data-testid="dependency-lines"
      className="pointer-events-none absolute left-[var(--label)] -z-10 overflow-visible"
      style={{ top: HEADER_H }}
      width={width}
      height={rows.length * ROW_H}
    >
      <defs>
        {(['muted', 'accent', 'danger'] as const).map((c) => (
          <marker
            key={c}
            id={`wp-dep-${c}`}
            viewBox="0 0 8 8"
            refX="7"
            refY="4"
            markerWidth="6"
            markerHeight="6"
            orient="auto"
          >
            <path d="M0,0 L8,4 L0,8 z" fill={`var(--wp-${c})`} />
          </marker>
        ))}
      </defs>
      {lines.map((l) => (
        <path
          key={l.key}
          data-dep={l.key}
          data-hot={l.hot || undefined}
          data-late={l.late || undefined}
          d={l.d}
          fill="none"
          stroke={color(l)}
          strokeOpacity={l.hot ? 1 : dim ? 0.2 : 0.55}
          strokeWidth={l.hot ? 2 : 1.25}
          strokeDasharray={l.late ? '4 3' : undefined}
          markerEnd={`url(#wp-dep-${l.late ? 'danger' : l.hot ? 'accent' : 'muted'})`}
          className="transition-[stroke-opacity] duration-150"
        />
      ))}
    </svg>
  )
}

function buildRows(issues: Issue[], collapsed: Set<number>): { rows: Row[]; unscheduled: Issue[] } {
  const ids = new Set(issues.map((i) => i.id))
  const kids = new Map<number, Issue[]>()
  const roots: Issue[] = []
  for (const i of issues) {
    if (i.parent_id && ids.has(i.parent_id)) {
      if (!kids.has(i.parent_id)) kids.set(i.parent_id, [])
      kids.get(i.parent_id)!.push(i)
    } else roots.push(i)
  }
  const span = (i: Issue): [string | null, string | null] => {
    let s = i.start_date ?? i.due_date
    let e = i.due_date ?? i.start_date
    for (const k of kids.get(i.id) ?? []) {
      const [ks, ke] = span(k)
      if (ks && (!s || ks < s)) s = ks
      if (ke && (!e || ke > e)) e = ke
    }
    return [s, e]
  }
  const rows: Row[] = []
  const unscheduled: Issue[] = []
  const walk = (i: Issue, depth: number) => {
    const [s, e] = span(i)
    const own = !!(i.start_date || i.due_date)
    if (!s) {
      if (depth === 0) unscheduled.push(i)
      return
    }
    const children = kids.get(i.id) ?? []
    rows.push({ issue: i, depth, start: s, end: e, derived: !own, hasKids: children.length > 0 })
    if (!collapsed.has(i.id)) for (const k of children) walk(k, depth + 1)
  }
  for (const r of roots) walk(r, 0)
  return { rows, unscheduled }
}

function buildTicks(start: Date, days: number, zoom: Zoom) {
  const major: { x: number; label: string }[] = []
  const minor: { x: number; label: string }[] = []
  for (let n = 0; n < days; n++) {
    const d = addDays(start, n)
    if (d.getDate() === 1 || n === 0)
      major.push({
        x: n,
        label: d.toLocaleDateString(undefined, {
          month: 'long',
          year: zoom === 'quarter' || d.getMonth() === 0 ? 'numeric' : undefined,
        }),
      })
    if (zoom === 'week')
      minor.push({
        x: n,
        label: d.toLocaleDateString(undefined, { weekday: 'narrow' }) + d.getDate(),
      })
    else if (zoom === 'month' && d.getDay() === 1) minor.push({ x: n, label: String(d.getDate()) })
    else if (zoom === 'quarter' && d.getDay() === 1 && d.getDate() <= 7)
      minor.push({ x: n, label: '' })
  }
  // The leading partial month gets a label too; drop it if the next month's
  // label starts too close to read both.
  if (major.length > 1 && (major[1].x - major[0].x) * DAY_PX[zoom] < 110) major.shift()
  return { major, minor }
}

function TimelineRow({
  row,
  project,
  origin,
  px,
  width,
  editable,
  live,
  onOpen,
  collapsed,
  onToggle,
}: {
  row: Row
  project: Project
  origin: Date
  px: number
  width: number
  editable: boolean
  /** Set when dependency lines are shown, so they follow drags and hover. */
  live?: Live
  onOpen: (k: string) => void
  collapsed: boolean
  onToggle: () => void
}) {
  const { issue } = row
  const edit = useIssueEdit(issue, project)
  const [drag, setDrag] = useState<{
    mode: 'move' | 'start' | 'end'
    x0: number
    dx: number
  } | null>(null)
  const startD = parse(row.start!)
  const endD = parse(row.end!)
  const shift = drag ? Math.round(drag.dx / px) : 0
  let s = diffDays(startD, origin)
  let e = diffDays(endD, origin)
  if (drag?.mode === 'move') ((s += shift), (e += shift))
  if (drag?.mode === 'start') s = Math.min(s + shift, e)
  if (drag?.mode === 'end') e = Math.max(e + shift, s)
  const milestone = isMilestone(row)
  // Before paint, so the lines move in the same frame as the bar.
  const dragged = drag ? `${s}:${e}` : ''
  useLayoutEffect(() => {
    live?.setPreview(issue.id, dragged ? { s, e } : null)
  }, [live, issue.id, dragged]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => () => live?.setPreview(issue.id, null), [live, issue.id])
  const closed = isClosed(issue.status.category)
  const color = issue.type === 'epic' ? '#a855f7' : 'var(--wp-accent)'

  const begin = (mode: 'move' | 'start' | 'end') => (ev: React.PointerEvent) => {
    if (!editable) return
    ev.stopPropagation()
    ;(ev.target as HTMLElement).setPointerCapture(ev.pointerId)
    setDrag({ mode, x0: ev.clientX, dx: 0 })
  }
  const move = (ev: React.PointerEvent) => drag && setDrag({ ...drag, dx: ev.clientX - drag.x0 })
  const end = () => {
    if (!drag) return
    const n = Math.round(drag.dx / px)
    const mode = drag.mode
    setDrag(null)
    if (n === 0) {
      if (mode === 'move') onOpen(issue.key)
      return
    }
    const ns = localISO(addDays(origin, s))
    const ne = localISO(addDays(origin, e))
    if (milestone) edit.due(ne)
    else if (mode === 'start') edit.start(ns)
    else if (mode === 'end') edit.due(ne)
    else if (!issue.due_date)
      edit.start(ns) // a start-only issue stays start-only
    else edit.patch({ start_date: ns, due_date: ne }, { start_date: ns, due_date: ne })
  }

  return (
    <div
      className="group flex h-10 border-b border-line last:border-b-0"
      onPointerEnter={live && (() => live.setHover(issue.key))}
      onPointerLeave={live && (() => live.setHover(null))}
      onFocus={live && (() => live.setHover(issue.key))}
      onBlur={live && (() => live.setHover(null))}
    >
      <div
        className="sticky left-0 z-[2] flex w-[var(--label)] shrink-0 items-center gap-1.5 border-r border-line bg-panel pr-2 text-[13px]"
        style={{ paddingLeft: 8 + row.depth * 16 }}
      >
        <button
          onClick={onToggle}
          className={clsx(
            'flex size-5 shrink-0 items-center justify-center rounded text-subtle hover:bg-hover',
            !row.hasKids && 'invisible',
          )}
        >
          <ChevronDown
            className={clsx('size-3.5 transition-transform', collapsed && '-rotate-90')}
          />
        </button>
        <StatusIcon category={issue.status.category} color={issue.status.color} />
        <TypeIcon type={issue.type} />
        <button
          onClick={() => onOpen(issue.key)}
          className={clsx(
            'min-w-0 flex-1 truncate text-left hover:underline',
            closed && 'text-muted line-through',
          )}
          title={`${issue.key} ${issue.title}`}
        >
          {issue.title}
        </button>
      </div>
      <div
        className="relative shrink-0"
        style={{ width }}
        onPointerMove={move}
        onPointerUp={end}
        onPointerCancel={() => setDrag(null)}
      >
        {milestone ? (
          <div
            onPointerDown={begin('move')}
            title={`${issue.key} due ${shortDate(row.end)}`}
            className={clsx(
              'absolute top-1/2 size-3.5 -translate-y-1/2 rotate-45 rounded-[3px] shadow-card',
              editable && 'cursor-grab active:cursor-grabbing',
            )}
            style={{ left: e * px + px / 2 - 7, background: color }}
          />
        ) : (
          <div
            onPointerDown={begin('move')}
            title={`${issue.key}: ${shortDate(row.start)} – ${shortDate(row.end)}${row.derived ? ' (from sub-issues)' : ''}`}
            className={clsx(
              'absolute top-1/2 flex h-6 -translate-y-1/2 items-center overflow-hidden rounded-md text-[11px] font-medium text-white shadow-card',
              editable && 'cursor-grab active:cursor-grabbing',
              row.derived && 'opacity-60',
              drag && 'ring-2 ring-fg/30',
            )}
            style={{
              left: s * px + 1,
              width: Math.max(px, (e - s + 1) * px - 2),
              background: row.derived
                ? `repeating-linear-gradient(135deg, ${color}, ${color} 6px, color-mix(in oklab, ${color} 75%, transparent) 6px, color-mix(in oklab, ${color} 75%, transparent) 12px)`
                : color,
            }}
          >
            {editable && (
              <span
                onPointerDown={begin('start')}
                className="absolute inset-y-0 left-0 w-2 cursor-ew-resize hover:bg-white/30"
              />
            )}
            <span className="truncate px-2.5">{(e - s + 1) * px > 70 ? issue.title : ''}</span>
            {editable && (
              <span
                onPointerDown={begin('end')}
                className="absolute inset-y-0 right-0 w-2 cursor-ew-resize hover:bg-white/30"
              />
            )}
          </div>
        )}
        {drag && (
          <div
            className="pointer-events-none absolute -top-0.5 z-[1] rounded bg-fg px-1.5 py-0.5 text-[10px] text-bg"
            style={{ left: s * px }}
          >
            {shortDate(localISO(addDays(origin, s)))} – {shortDate(localISO(addDays(origin, e)))}
          </div>
        )}
      </div>
    </div>
  )
}

function Unscheduled({
  issues,
  project,
  editable,
  onOpen,
}: {
  issues: Issue[]
  project: Project
  editable: boolean
  onOpen: (k: string) => void
}) {
  const [open, setOpen] = useLocal('timeline-unscheduled', true)
  return (
    <section className="mt-4 overflow-hidden rounded-xl border border-line bg-panel">
      <button
        onClick={() => setOpen(!open)}
        className="flex h-10 w-full items-center gap-2 px-4 text-[13px] font-semibold"
      >
        <ChevronDown
          className={clsx('size-4 text-subtle transition-transform', !open && '-rotate-90')}
        />
        Unscheduled <span className="text-xs font-normal text-subtle">{issues.length}</span>
      </button>
      {open &&
        issues.map((i) => (
          <UnscheduledRow
            key={i.id}
            issue={i}
            project={project}
            editable={editable}
            onOpen={onOpen}
          />
        ))}
    </section>
  )
}

function UnscheduledRow({
  issue,
  project,
  editable,
  onOpen,
}: {
  issue: Issue
  project: Project
  editable: boolean
  onOpen: (k: string) => void
}) {
  const edit = useIssueEdit(issue, project)
  return (
    <div className="flex h-10 items-center gap-2 border-t border-line px-4 text-[13px]">
      <TypeIcon type={issue.type} />
      <button
        onClick={() => onOpen(issue.key)}
        className="min-w-0 flex-1 truncate text-left hover:underline"
      >
        <span className="text-subtle">{issue.key}</span> {issue.title}
      </button>
      <DatePicker value={null} onChange={edit.start} placeholder="Start" disabled={!editable} />
      <DatePicker value={null} onChange={edit.due} placeholder="Due" disabled={!editable} />
    </div>
  )
}
