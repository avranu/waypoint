import { useMemo, useState, type FormEvent } from 'react'
import { useNavigate, useSearch } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  BookA,
  Bot,
  CircleDot,
  FileCode,
  FileText,
  GitPullRequest,
  Lightbulb,
  Network,
  Route as RouteIcon,
} from 'lucide-react'
import { q } from '@/lib/queries'
import type { GraphEdge, GraphHit, GraphNode, GraphNodeType, GraphView } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Switch } from '@/components/ui/input'
import { Empty, PageHeader, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { AppLink } from '@/components/AppLink'
import { vizClass, viz } from '@/components/charts'

/** The page's search (validated in the router): sure = only links people and agents made; to = a second thing, for the route to it. */
interface GraphSearch {
  ref?: string
  hops?: number
  sure?: boolean
  open?: boolean
  to?: string
}

// Four groups only: the validated categorical palette has four slots.

type Group = 'work' | 'knowledge' | 'code' | 'session'

const groupOf: Record<GraphNodeType, Group> = {
  issue: 'work',
  page: 'knowledge',
  design: 'knowledge',
  glossary: 'knowledge',
  pull: 'code',
  file: 'code',
  session: 'session',
}

const groupColor: Record<Group, string> = {
  work: viz.blue,
  knowledge: viz.orange,
  code: viz.aqua,
  session: viz.yellow,
}

const groupLabel: Record<Group, string> = {
  work: 'Tasks',
  knowledge: 'Pages, decisions, terms',
  code: 'Pull requests, files',
  session: 'Agent sessions',
}

function NodeIcon({ type, className }: { type: GraphNodeType; className?: string }) {
  const c = clsx('size-3.5 shrink-0 text-subtle', className)
  switch (type) {
    case 'issue':
      return <CircleDot className={c} />
    case 'page':
      return <FileText className={c} />
    case 'design':
      return <Lightbulb className={c} />
    case 'glossary':
      return <BookA className={c} />
    case 'pull':
      return <GitPullRequest className={c} />
    case 'file':
      return <FileCode className={c} />
    case 'session':
      return <Bot className={c} />
  }
}

/** What to ask the graph for to centre on a node: a task's key, else its id. */
const centreRef = (n: GraphNode) => (n.type === 'issue' ? n.ref : n.node)

const dash: Record<GraphEdge['basis'], string | undefined> = {
  person: undefined,
  agent: '6 4',
  inferred: '1.5 4',
}

const basisWord: Record<GraphEdge['basis'], string> = {
  person: 'a person',
  agent: 'an agent',
  inferred: 'Waypoint’s guess',
}

const closed = (n: GraphNode) =>
  n.state === 'done' ||
  n.state === 'canceled' ||
  n.state === 'merged' ||
  n.state === 'closed' ||
  n.state === 'retired'

/** A route read out: "relates to WP-4 → documented by". */
function routeText(path: GraphEdge[] | null, names: Map<string, string>): string {
  const p = path ?? []
  return p
    .map((e, i) => {
      let s = e.label
      if (e.basis === 'inferred') s += ' (guess)'
      else if (e.basis === 'agent') s += ' (agent)'
      if (i < p.length - 1) s += ' ' + (names.get(e.to) ?? '')
      return s
    })
    .join(' → ')
}

const SIZE = 640
const MID = SIZE / 2

interface Placed {
  hit: GraphHit
  x: number
  y: number
  angle: number
}

export function layoutRings(view: GraphView): Map<string, Placed> {
  const nodes = view.nodes ?? []
  const out = new Map<string, Placed>()
  const maxHops = Math.max(1, ...nodes.map((n) => n.hops))
  const step = (MID - 40) / maxHops
  const TAU = 2 * Math.PI
  for (let h = 1; h <= maxHops; h++) {
    const ring = nodes.filter((n) => n.hops === h)
    if (!ring.length) continue
    const r = step * h
    const parentOf = (n: GraphHit) => {
      const p = n.path ?? []
      return p.length ? p[p.length - 1].from : ''
    }
    if (h === 1) {
      ring.sort((a, b) => a.distance - b.distance)
      ring.forEach((n, i) => {
        const angle = -Math.PI / 2 + (TAU * i) / ring.length
        out.set(n.node, {
          hit: n,
          angle,
          x: MID + r * Math.cos(angle),
          y: MID + r * Math.sin(angle),
        })
      })
      continue
    }
    const gap = Math.min(TAU / ring.length, Math.max(56 / r, 0.5))
    const byParent = new Map<string, GraphHit[]>()
    for (const n of ring) {
      const k = parentOf(n)
      byParent.set(k, [...(byParent.get(k) ?? []), n])
    }
    const want: { n: GraphHit; a: number }[] = []
    for (const [k, kids] of byParent) {
      const base = out.get(k)?.angle ?? -Math.PI / 2
      kids.sort((a, b) => a.distance - b.distance)
      kids.forEach((n, j) => want.push({ n, a: base + (j - (kids.length - 1) / 2) * gap }))
    }
    want.sort((x, y) => x.a - y.a)
    const wanted = want.map((w) => w.a)
    for (let i = 1; i < want.length; i++) want[i].a = Math.max(want[i].a, want[i - 1].a + gap)
    const drift = want.reduce((s, w, i) => s + w.a - wanted[i], 0) / want.length
    for (const { n, a } of want) {
      const angle = a - drift
      out.set(n.node, { hit: n, angle, x: MID + r * Math.cos(angle), y: MID + r * Math.sin(angle) })
    }
  }
  return out
}

function GraphMap({
  view,
  selected,
  onSelect,
}: {
  view: GraphView
  selected: string | null
  onSelect: (node: string) => void
}) {
  const placed = useMemo(() => layoutRings(view), [view])
  const [hover, setHover] = useState<string | null>(null)
  const focus = hover ?? selected
  const onRoute = new Set<string>()
  const focusHit = focus ? placed.get(focus)?.hit : undefined
  for (const e of focusHit?.path ?? []) onRoute.add(e.from + '|' + e.to)
  const pos = (id: string) =>
    id === view.center.node ? { x: MID, y: MID } : (placed.get(id) ?? null)
  const maxHops = Math.max(1, ...(view.nodes ?? []).map((n) => n.hops))
  const ringStep = (MID - 40) / maxHops
  return (
    <svg
      viewBox={`0 0 ${SIZE} ${SIZE}`}
      className="mx-auto block h-auto w-full max-w-[640px] touch-manipulation"
      role="img"
      aria-label={`Map of what lies around ${view.center.ref}`}
    >
      {Array.from({ length: maxHops }, (_, i) => (
        <circle
          key={i}
          cx={MID}
          cy={MID}
          r={ringStep * (i + 1)}
          fill="none"
          stroke="var(--wp-line)"
          strokeWidth={1}
        />
      ))}
      {(view.edges ?? []).map((e, i) => {
        const a = pos(e.from)
        const b = pos(e.to)
        if (!a || !b) return null
        const hot = onRoute.has(e.from + '|' + e.to) || onRoute.has(e.to + '|' + e.from)
        return (
          <line
            key={i}
            x1={a.x}
            y1={a.y}
            x2={b.x}
            y2={b.y}
            stroke={hot ? 'var(--wp-fg)' : 'var(--wp-line-strong)'}
            strokeWidth={hot ? 2 : 1}
            strokeDasharray={dash[e.basis]}
            strokeLinecap="round"
          >
            <title>{`${e.label} (by ${basisWord[e.basis]})${e.why ? ': ' + e.why : ''}`}</title>
          </line>
        )
      })}
      {[...placed.values()].map(({ hit, x, y }) => {
        const color = groupColor[groupOf[hit.type]]
        const on = focus === hit.node
        const label = hit.hops === 1 || on || placed.size <= 24
        return (
          <g
            key={hit.node}
            role="button"
            tabIndex={0}
            aria-label={`${hit.ref} ${hit.title}`}
            className="cursor-pointer outline-none"
            onClick={() => onSelect(hit.node)}
            onKeyDown={(ev) => {
              if (ev.key === 'Enter' || ev.key === ' ') {
                ev.preventDefault()
                onSelect(hit.node)
              }
            }}
            onPointerEnter={() => setHover(hit.node)}
            onPointerLeave={() => setHover(null)}
            onFocus={() => setHover(hit.node)}
            onBlur={() => setHover(null)}
          >
            <title>{`${hit.ref} ${hit.title}${hit.state ? ' [' + hit.state + ']' : ''}`}</title>
            {}
            <circle cx={x} cy={y} r={14} fill="transparent" />
            <circle
              cx={x}
              cy={y}
              r={on ? 8 : 6}
              fill={closed(hit) ? 'var(--wp-panel)' : color}
              stroke={closed(hit) ? color : 'var(--wp-panel)'}
              strokeWidth={2}
            />
            {label && (
              <text
                x={x}
                y={y - 11}
                textAnchor="middle"
                className={clsx(
                  'fill-[var(--wp-muted)] text-[11px] max-sm:text-[20px]',
                  hit.hops > 1 && !on && 'max-sm:hidden',
                )}
                style={{ paintOrder: 'stroke', stroke: 'var(--wp-panel)', strokeWidth: 3 }}
              >
                {hit.ref.length > 24 ? hit.ref.slice(0, 23) + '…' : hit.ref}
              </text>
            )}
          </g>
        )
      })}
      <g>
        <circle
          cx={MID}
          cy={MID}
          r={10}
          fill={groupColor[groupOf[view.center.type]]}
          stroke="var(--wp-fg)"
          strokeWidth={2}
        />
        <text
          x={MID}
          y={MID + 26}
          textAnchor="middle"
          className="fill-[var(--wp-fg)] text-[12px] font-semibold max-sm:text-[22px]"
          style={{ paintOrder: 'stroke', stroke: 'var(--wp-panel)', strokeWidth: 3 }}
        >
          {view.center.ref}
        </text>
      </g>
    </svg>
  )
}

function Legend() {
  return (
    <div className="flex flex-wrap items-center gap-x-4 gap-y-1 px-4 pb-3 text-xs text-muted">
      {(Object.keys(groupLabel) as Group[]).map((g) => (
        <span key={g} className="flex items-center gap-1.5">
          <svg width="10" height="10" aria-hidden>
            <circle cx="5" cy="5" r="4" fill={groupColor[g]} />
          </svg>
          {groupLabel[g]}
        </span>
      ))}
      {(['person', 'agent', 'inferred'] as const).map((b) => (
        <span key={b} className="flex items-center gap-1.5">
          <svg width="22" height="6" aria-hidden>
            <line
              x1="1"
              y1="3"
              x2="21"
              y2="3"
              stroke="var(--wp-muted)"
              strokeWidth="1.5"
              strokeDasharray={dash[b]}
              strokeLinecap="round"
            />
          </svg>
          by {basisWord[b]}
        </span>
      ))}
      <span>hollow: finished</span>
    </div>
  )
}

/** A node's name as a link: in the app, out to the forge, or a plain path. */
function NodeLink({ n, className }: { n: GraphNode; className?: string }) {
  const body = (
    <>
      <span className="text-subtle">{n.ref}</span>{' '}
      <span className={clsx(closed(n) && 'line-through text-muted')}>
        {n.type === 'file' ? '' : n.title}
      </span>
    </>
  )
  if (n.url?.startsWith('/'))
    return (
      <AppLink to={n.url} className={clsx('hover:underline', className)}>
        {body}
      </AppLink>
    )
  if (n.url && /^https?:\/\//i.test(n.url))
    return (
      <a
        href={n.url}
        target="_blank"
        rel="noreferrer"
        className={clsx('hover:underline', className)}
      >
        {body}
      </a>
    )
  return <span className={className}>{body}</span>
}

function Nearest({
  view,
  selected,
  onSelect,
  onCentre,
}: {
  view: GraphView
  selected: string | null
  onSelect: (node: string) => void
  onCentre: (n: GraphNode) => void
}) {
  const names = new Map<string, string>([[view.center.node, view.center.ref]])
  for (const n of view.nodes ?? []) names.set(n.node, n.ref)
  return (
    <ul className="divide-y divide-line">
      {(view.nodes ?? []).map((n) => (
        <li
          key={n.node}
          className={clsx(
            'flex items-start gap-2 px-4 py-2 text-[13px]',
            selected === n.node && 'bg-hover',
          )}
          onMouseEnter={() => onSelect(n.node)}
        >
          <span
            className="mt-0.5 w-8 shrink-0 text-right text-xs tabular-nums text-subtle"
            title="Distance: each step's length, added up"
          >
            {n.distance.toFixed(1)}
          </span>
          <NodeIcon type={n.type} className="mt-0.5" />
          <div className="min-w-0 flex-1">
            <NodeLink n={n} className="block truncate" />
            <div className="truncate text-xs text-subtle" title={routeText(n.path, names)}>
              {routeText(n.path, names)}
            </div>
          </div>
          <Button size="xs" variant="ghost" onClick={() => onCentre(n)} title="Centre the map here">
            Centre
          </Button>
        </li>
      ))}
    </ul>
  )
}

function RouteTo({ from, to, guesses }: { from: string; to: string; guesses: boolean }) {
  const r = useQuery(q.graphPath(from, to, guesses))
  if (r.isPending) return <Skeleton className="m-4 h-16" />
  if (r.error) return <div className="px-4 py-3 text-[13px] text-danger">{r.error.message}</div>
  const p = r.data
  if (!p.found)
    return (
      <div className="px-4 py-3 text-[13px] text-muted">
        Nothing links {p.from.ref} and {p.to.ref} within 6 steps.
      </div>
    )
  const nodes = p.nodes ?? []
  return (
    <div className="px-4 py-3 text-[13px]">
      <div className="mb-2 text-xs text-muted">
        {p.hops === 0
          ? 'The same thing'
          : `Distance ${p.distance.toFixed(1)}, ${p.hops} ${p.hops === 1 ? 'step' : 'steps'}`}
      </div>
      <ol className="space-y-1">
        {nodes.map((n, i) => (
          <li key={n.node} className="flex min-w-0 items-center gap-2">
            {i > 0 && (
              <span className="shrink-0 text-xs text-subtle">
                {(p.path ?? [])[i - 1]?.label}
                {(p.path ?? [])[i - 1]?.basis === 'inferred' && ' (guess)'}
              </span>
            )}
            <NodeIcon type={n.type} />
            <NodeLink n={n} className="truncate" />
          </li>
        ))}
      </ol>
    </div>
  )
}

export function GraphPage() {
  const search: GraphSearch = useSearch({ from: '/app/graph' })
  const navigate = useNavigate()
  const set = (patch: Partial<GraphSearch>) =>
    navigate({ to: '/graph', search: { ...search, ...patch } })
  const hops = search.hops ?? 2
  const guesses = !search.sure
  const [draft, setDraft] = useState(search.ref ?? '')
  const [toDraft, setToDraft] = useState(search.to ?? '')
  const [selected, setSelected] = useState<string | null>(null)
  const view = useQuery({
    ...q.graph(search.ref ?? '', { hops, guesses, open: search.open, limit: 80 }),
    enabled: !!search.ref,
    placeholderData: (prev) => prev,
  })
  const go = (e: FormEvent) => {
    e.preventDefault()
    setSelected(null)
    set({ ref: draft.trim() || undefined, to: undefined })
    setToDraft('')
  }
  const centre = (n: GraphNode) => {
    setSelected(null)
    setDraft(centreRef(n))
    set({ ref: centreRef(n), to: undefined })
    setToDraft('')
  }
  const v = view.data
  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <PageHeader
        icon={<Network className="size-5 text-subtle" />}
        title="Connections"
        subtitle="Everything that relates to a thing, nearest first, and how"
      />
      <div className="mx-auto max-w-5xl space-y-4 px-3 sm:px-6">
        <form onSubmit={go} className="flex flex-wrap items-center gap-2">
          <Input
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            placeholder="A task, ADR7, PL3, a term, a file path…"
            aria-label="What to centre on"
            className="min-w-0 flex-1 basis-56"
          />
          <Button type="submit" size="sm">
            Show
          </Button>
        </form>
        <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-[13px] text-muted">
          <SegmentedControl
            label="Steps out"
            value={String(hops) as '1' | '2' | '3' | '4'}
            onChange={(h) => set({ hops: Number(h) })}
            textFrom="always"
            options={(['1', '2', '3', '4'] as const).map((h) => ({
              value: h,
              label: `${h} ${h === '1' ? 'step' : 'steps'}`,
              icon: null,
            }))}
          />
          <label className="flex items-center gap-2">
            <Switch
              checked={guesses}
              onChange={(g) => set({ sure: g ? undefined : true })}
              label="Guesses"
            />
            Guesses
          </label>
          <label className="flex items-center gap-2">
            <Switch
              checked={!!search.open}
              onChange={(o) => set({ open: o || undefined })}
              label="Open tasks only"
            />
            Open tasks only
          </label>
        </div>
        {!search.ref ? (
          <Section>
            <Empty icon={<Network />} title="Pick something to centre on">
              Tasks, pages, decisions, goals, terms, pull requests, files and agent sessions are one
              graph: relations, parents, links, the files a pull request changed. Guesses add tasks
              alike in words.
            </Empty>
          </Section>
        ) : view.isPending ? (
          <Skeleton className="h-96" />
        ) : view.error ? (
          <Section>
            <div className="px-4 py-3 text-[13px] text-danger">{view.error.message}</div>
          </Section>
        ) : v ? (
          <>
            <Section
              className={vizClass}
              title={
                <span className="flex min-w-0 items-center gap-2">
                  <NodeIcon type={v.center.type} />
                  <NodeLink n={v.center} className="truncate" />
                </span>
              }
            >
              {(v.nodes ?? []).length === 0 ? (
                <Empty icon={<Network />} title="Nothing relates to it yet">
                  {guesses ? undefined : 'Turn on guesses to add tasks alike in words.'}
                </Empty>
              ) : (
                <>
                  <div className="px-2 pt-2">
                    <GraphMap view={v} selected={selected} onSelect={setSelected} />
                  </div>
                  <Legend />
                </>
              )}
            </Section>
            <Section
              title={
                <span className="flex items-center gap-2">
                  <RouteIcon className="size-3.5 text-subtle" /> Route to…
                </span>
              }
            >
              <form
                className="flex flex-wrap gap-2 px-4 py-3"
                onSubmit={(e) => {
                  e.preventDefault()
                  set({ to: toDraft.trim() || undefined })
                }}
              >
                <Input
                  value={toDraft}
                  onChange={(e) => setToDraft(e.target.value)}
                  placeholder="Another task, page, decision or file"
                  aria-label="Route to"
                  className="min-w-0 flex-1 basis-56"
                />
                <Button type="submit" size="sm" variant="secondary">
                  Find route
                </Button>
              </form>
              {search.to && search.ref && (
                <RouteTo from={search.ref} to={search.to} guesses={guesses} />
              )}
            </Section>
            {(v.nodes ?? []).length > 0 && (
              <Section
                title={`Nearest ${v.nodes!.length}${v.more ? '+' : ''}`}
                actions={
                  v.size && (
                    <span className="text-xs text-subtle" title="The whole graph">
                      {v.size.nodes.toLocaleString()} things, {v.size.edges.toLocaleString()} links
                    </span>
                  )
                }
              >
                <Nearest view={v} selected={selected} onSelect={setSelected} onCentre={centre} />
              </Section>
            )}
          </>
        ) : null}
      </div>
    </div>
  )
}
