import { useMemo, useState, type ReactNode } from 'react'
import { Link, Navigate } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  ArrowDown,
  ArrowDownAZ,
  ArrowUp,
  Bot,
  ChevronDown,
  ChevronUp,
  CircleAlert,
  CircleCheck,
  Eye,
  EyeOff,
  Gauge,
  Info,
  SlidersHorizontal,
  Sparkles,
  TriangleAlert,
} from 'lucide-react'
import { q, invalidateProject } from '@/lib/queries'
import { useLocal } from '@/lib/hooks'
import { plural, shortDate } from '@/lib/format'
import { goalApi, invalidateKnowledge } from '@/lib/knowledge'
import {
  dashboardApi,
  onGoalShare,
  perWeek,
  type DashGoal,
  type Dashboard,
  type DashSignal,
  type DashStage,
  type DashTone,
} from '@/lib/dashboard'
import { Button } from '@/components/ui/button'
import { Empty, PageHeader, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { toast, toastError } from '@/components/ui/toast'
import { BarChart, DataTable, fmtNum, Legend, Sparkbars, viz, vizClass } from '@/components/charts'
import { GroundingBar } from '@/components/goals'
import { ProjectIcon } from '@/components/issue/icons'
import { useProject } from './Project'

type CardID = 'attention' | 'goals' | 'flow' | 'pace'
const CARDS: { id: CardID; label: string }[] = [
  { id: 'attention', label: 'Needs attention' },
  { id: 'goals', label: 'Goals' },
  { id: 'flow', label: 'How work moves' },
  { id: 'pace', label: 'Pace' },
]

interface Layout {
  order: CardID[]
  hidden: CardID[]
}

/** The card order and what's hidden, kept per browser; cards added later show at the end. */
function useLayout() {
  const [saved, setSaved] = useLocal<Layout>('dash-cards', { order: [], hidden: [] })
  // Stored by hand or by an older build, it may be any shape.
  const known = (v: unknown) =>
    Array.isArray(v) ? v.filter((id): id is CardID => CARDS.some((c) => c.id === id)) : []
  const kept = known(saved?.order)
  const order = [...kept, ...CARDS.map((c) => c.id).filter((id) => !kept.includes(id))]
  return { order, hidden: known(saved?.hidden), set: (l: Layout) => setSaved(l) }
}

export function ProjectOverviewPage() {
  const project = useProject()
  const dash = useQuery({ ...q.dashboard(project.key), enabled: project.role !== 'guest' })
  const [details, setDetails] = useLocal('dash-details', false)
  const [customizing, setCustomizing] = useState(false)
  const layout = useLayout()
  if (project.role === 'guest') return <Navigate to="/p/$key/board" params={{ key: project.key }} />
  const d = dash.data

  const cards: Record<CardID, ReactNode> = d
    ? {
        attention: <AttentionCard d={d} details={details} />,
        goals: <GoalsCard d={d} />,
        flow: <FlowCard d={d} details={details} />,
        pace: <PaceCard d={d} details={details} />,
      }
    : ({} as Record<CardID, ReactNode>)

  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <div className="mx-auto max-w-6xl space-y-4 px-3 pt-3 sm:px-5 sm:pt-4">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0 flex-1">
            {d ? (
              <p className="text-[15px] leading-relaxed" data-testid="understanding">
                {understanding(d)}
              </p>
            ) : (
              <Skeleton className="h-6 w-2/3" />
            )}
            {d?.description && (
              <p className="mt-1 line-clamp-2 text-[13px] text-muted">{d.description}</p>
            )}
          </div>
          <ViewControls
            details={details}
            setDetails={setDetails}
            customizing={customizing}
            setCustomizing={setCustomizing}
          />
        </div>
        {customizing && <CustomizePanel layout={layout} />}
        {dash.isError ? (
          <LoadError message={dash.error.message} retry={() => void dash.refetch()} />
        ) : !d ? (
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            {[0, 1, 2, 3].map((i) => (
              <Skeleton key={i} className="h-24" />
            ))}
          </div>
        ) : (
          <>
            <Tiles d={d} />
            <div className="grid grid-cols-1 gap-3 sm:gap-4 lg:grid-cols-2">
              {layout.order
                .filter((id) => !layout.hidden.includes(id))
                .map((id) => (
                  <div key={id} className={clsx('min-w-0', id === 'attention' && 'lg:col-span-2')}>
                    {cards[id]}
                  </div>
                ))}
            </div>
          </>
        )}
      </div>
    </div>
  )
}

/** One or two sentences of what Waypoint sees, from the counts alone. */
function understanding(d: Dashboard): string {
  const parts = [`${d.name} has ${plural(d.open, 'open task')}.`]
  const wk = perWeek(d.pace)
  parts.push(
    d.pace.done_4w > 0
      ? `About ${fmtNum(wk)} ${wk === 1 ? 'gets' : 'get'} done a week.`
      : 'Nothing was finished in the last four weeks.',
  )
  const share = onGoalShare(d)
  if (!d.scoped) parts.push('It has no goals yet.')
  else if (share !== null) parts.push(`${pct(share)} of open work serves a goal people approved.`)
  const neck = d.stages.find((s) => s.bottleneck)
  if (neck) parts.push(`Work waits longest in ${neck.name}.`)
  return parts.join(' ')
}

const pct = (x: number) => `${Math.round(x * 100)}%`

function ViewControls({
  details,
  setDetails,
  customizing,
  setCustomizing,
}: {
  details: boolean
  setDetails: (v: boolean) => void
  customizing: boolean
  setCustomizing: (v: boolean) => void
}) {
  return (
    <div className="flex shrink-0 items-center gap-1.5">
      <Button
        size="sm"
        variant="ghost"
        icon={details ? <EyeOff className="size-3.5" /> : <Eye className="size-3.5" />}
        aria-pressed={details}
        onClick={() => setDetails(!details)}
        title="Show the queries, exact figures and tables behind each card"
      >
        {details ? 'Hide details' : 'Details'}
      </Button>
      <Button
        size="sm"
        variant={customizing ? 'subtle' : 'ghost'}
        icon={<SlidersHorizontal className="size-3.5" />}
        aria-pressed={customizing}
        onClick={() => setCustomizing(!customizing)}
      >
        Customize
      </Button>
    </div>
  )
}

function CustomizePanel({ layout }: { layout: ReturnType<typeof useLayout> }) {
  const move = (i: number, by: number) => {
    const order = [...layout.order]
    const [c] = order.splice(i, 1)
    order.splice(i + by, 0, c)
    layout.set({ order, hidden: layout.hidden })
  }
  const toggle = (id: CardID) =>
    layout.set({
      order: layout.order,
      hidden: layout.hidden.includes(id)
        ? layout.hidden.filter((h) => h !== id)
        : [...layout.hidden, id],
    })
  return (
    <Section title="Cards on this page" className="max-w-md">
      <ul className="divide-y divide-line">
        {layout.order.map((id, i) => {
          const c = CARDS.find((c) => c.id === id)!
          const shown = !layout.hidden.includes(id)
          return (
            <li key={id} className="flex items-center gap-2 px-4 py-1.5 text-[13px]">
              <label className="flex flex-1 items-center gap-2">
                <input type="checkbox" checked={shown} onChange={() => toggle(id)} />
                <span className={clsx(!shown && 'text-subtle')}>{c.label}</span>
              </label>
              <Button
                size="xs"
                variant="ghost"
                aria-label={`Move ${c.label} up`}
                disabled={i === 0}
                onClick={() => move(i, -1)}
                icon={<ChevronUp className="size-3.5" />}
              />
              <Button
                size="xs"
                variant="ghost"
                aria-label={`Move ${c.label} down`}
                disabled={i === layout.order.length - 1}
                onClick={() => move(i, 1)}
                icon={<ChevronDown className="size-3.5" />}
              />
            </li>
          )
        })}
      </ul>
    </Section>
  )
}

function LoadError({ message, retry }: { message: string; retry: () => void }) {
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center gap-2 rounded-xl border border-line bg-panel p-4 text-[13px]"
    >
      <span className="text-danger">Couldn’t load the overview: {message}</span>
      <Button size="xs" variant="subtle" onClick={retry}>
        Try again
      </Button>
    </div>
  )
}

function Tile({
  label,
  value,
  sub,
  children,
  tone,
}: {
  label: string
  value: ReactNode
  sub?: ReactNode
  children?: ReactNode
  tone?: DashTone | 'ok'
}) {
  return (
    <div className="min-w-0 rounded-xl border border-line bg-panel px-4 py-3">
      <div className="text-xs text-muted">{label}</div>
      <div
        className={clsx(
          'mt-0.5 text-2xl font-semibold tabular-nums tracking-tight',
          tone === 'bad' && 'text-danger',
        )}
      >
        {value}
      </div>
      {sub && <div className="mt-0.5 text-xs text-subtle">{sub}</div>}
      {children && <div className="mt-2">{children}</div>}
    </div>
  )
}

function Trend({ now, before, unit }: { now: number; before: number; unit: string }) {
  if (before === 0 && now === 0) return <>no change</>
  const up = now > before
  const Icon = up ? ArrowUp : ArrowDown
  if (now === before) return <>same as the four weeks before</>
  return (
    <span className="inline-flex items-center gap-0.5">
      <Icon className="size-3" aria-hidden />
      {up ? 'up' : 'down'} from {fmtNum(before)} {unit}
    </span>
  )
}

function Tiles({ d }: { d: Dashboard }) {
  const wk = perWeek(d.pace)
  const share = onGoalShare(d)
  const urgent = d.signals.filter((s) => s.tone !== 'info').length
  const drift = d.goals?.drift
  return (
    <div className={clsx('grid grid-cols-2 gap-3 lg:grid-cols-4', vizClass)}>
      <Tile
        label="Done a week"
        value={fmtNum(wk)}
        sub={<Trend now={wk} before={d.pace.done_prev_4w / 4} unit="a week" />}
      >
        <Sparkbars
          name="Done"
          ariaLabel="Tasks done per week, last twelve weeks"
          values={d.pace.weeks.map((w) => w.done)}
          labels={d.pace.weeks.map((w) => 'Week of ' + shortDate(w.week))}
          height={28}
        />
      </Tile>
      <Tile
        label="Filed to done"
        value={d.pace.lead_days === null ? '–' : days(d.pace.lead_days)}
        sub="typically, last four weeks"
      />
      <Tile
        label="On a goal"
        value={!d.scoped ? 'No goals' : share === null ? '–' : pct(share)}
        sub={
          drift && drift.previous_grounded > 0
            ? `of open work · finished work ${pct(drift.grounded)} this week, ${pct(drift.previous_grounded)} last`
            : 'of open work'
        }
      >
        {d.goals && <GroundingBar counts={d.goals.totals} />}
      </Tile>
      <Tile
        label="Needs attention"
        value={
          urgent === 0 ? (
            <span className="inline-flex items-center gap-1.5 text-ok">
              <CircleCheck className="size-5" aria-hidden />0
            </span>
          ) : (
            urgent
          )
        }
        sub={urgent === 0 ? 'nothing looks wrong' : plural(urgent, 'problem')}
        tone={urgent > 0 ? 'bad' : 'ok'}
      />
    </div>
  )
}

const days = (n: number) => (n < 1 ? 'under a day' : `${fmtNum(n, 0)} ${n < 1.5 ? 'day' : 'days'}`)

const TONE: Record<DashTone, { icon: typeof Info; className: string; label: string }> = {
  bad: { icon: CircleAlert, className: 'text-danger', label: 'Problem' },
  warn: { icon: TriangleAlert, className: 'text-warn', label: 'Worth a look' },
  info: { icon: Info, className: 'text-info', label: 'For you' },
}

function AttentionCard({ d, details }: { d: Dashboard; details: boolean }) {
  return (
    <Section title="Needs attention">
      {d.signals.length === 0 ? (
        <Empty icon={<CircleCheck />} title="Nothing looks wrong">
          Work is moving, nothing is late or blocked, and nobody is waiting for an answer.
        </Empty>
      ) : (
        <ul className="divide-y divide-line" aria-label="Needs attention">
          {d.signals.map((s) => (
            <SignalRow key={s.id} s={s} d={d} details={details} />
          ))}
        </ul>
      )}
    </Section>
  )
}

function SignalRow({
  s,
  d,
  details,
  showProject,
}: {
  s: DashSignal
  d: Dashboard
  details: boolean
  showProject?: boolean
}) {
  const tone = TONE[s.tone]
  const Icon = tone.icon
  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-1.5 px-4 py-2.5" data-signal={s.id}>
      <Icon className={clsx('size-4 shrink-0', tone.className)} aria-label={tone.label} />
      <div className="min-w-0 flex-1 basis-60 text-[13px]">
        {showProject && (
          <Link
            to="/p/$key/overview"
            params={{ key: d.project }}
            className="mr-1.5 font-medium text-muted hover:text-fg hover:underline"
          >
            {d.name}
          </Link>
        )}
        {s.title}
        {details && s.query && (
          <code className="ml-2 rounded bg-sunken px-1 text-[11px] text-muted">{s.query}</code>
        )}
      </div>
      <div className="flex shrink-0 items-center gap-1.5">
        <SeeLink s={s} project={d.project} />
        <FixButton s={s} d={d} />
      </div>
    </li>
  )
}

function SeeLink({ s, project }: { s: DashSignal; project: string }) {
  const cls = 'text-xs text-muted underline-offset-2 hover:text-fg hover:underline'
  if (s.query)
    return (
      <Link to="/p/$key/list" params={{ key: project }} search={{ q: s.query }} className={cls}>
        See {s.count > 0 ? plural(s.count, 'task') : 'tasks'}
      </Link>
    )
  switch (s.where) {
    case 'goals':
      return (
        <Link to="/p/$key/goals" params={{ key: project }} className={cls}>
          Goals
        </Link>
      )
    case 'inbox':
      return (
        <Link to="/inbox" className={cls}>
          Inbox
        </Link>
      )
    case 'reports':
      return (
        <Link to="/p/$key/reports" params={{ key: project }} className={cls}>
          Reports
        </Link>
      )
  }
  return null
}

function FixButton({ s, d }: { s: DashSignal; d: Dashboard }) {
  const qc = useQueryClient()
  const fix = useMutation({
    mutationFn: () => dashboardApi.fix(d.project, s.id),
    onSuccess: (r) => {
      toast(
        r.created
          ? `Filed ${r.issue.key} for an agent with the tasks behind it.`
          : `${r.issue.key} is already filed for this.`,
        { href: `/issue/${r.issue.key}`, ms: 8000 },
      )
      void qc.invalidateQueries({ queryKey: ['dashboard'] })
      invalidateProject(qc, d.project)
    },
    onError: toastError,
  })
  const suggest = useMutation({
    mutationFn: () => goalApi.suggest(d.project),
    onSuccess: (r) => {
      toast(
        r.goals.length || r.rewordings
          ? `Waypoint suggests ${plural(r.goals.length, 'goal')}${r.rewordings ? ` and clearer words for ${plural(r.rewordings, 'goal')}` : ''}: approve or dismiss each on the Goals tab.`
          : (r.note ?? 'Nothing new to suggest.'),
        { href: `/p/${d.project}/goals`, ms: 8000 },
      )
      invalidateKnowledge(qc)
      void qc.invalidateQueries({ queryKey: ['dashboard'] })
    },
    onError: toastError,
  })
  if (s.filed)
    return (
      <Link
        to="/issue/$key"
        params={{ key: s.filed.key }}
        className="inline-flex items-center gap-1 rounded-md bg-sunken px-2 py-0.5 text-xs text-muted hover:text-fg"
        title={s.filed.title}
      >
        <Bot className="size-3.5" aria-hidden />
        {s.filed.key}
      </Link>
    )
  if (s.fix === 'agent' && d.can_fix)
    return (
      <Button
        size="xs"
        icon={<Bot className="size-3.5" />}
        loading={fix.isPending}
        onClick={() => fix.mutate()}
        title="File a task for an agent with the tasks behind this: it finds the cause and fixes it, or proposes a fix"
      >
        Ask an agent
      </Button>
    )
  if (s.fix === 'suggest_goals' && d.can_suggest)
    return (
      <Button
        size="xs"
        icon={<Sparkles className="size-3.5" />}
        loading={suggest.isPending}
        onClick={() => suggest.mutate()}
        title="Waypoint reads the work no goal covers and proposes goals; nothing counts until you approve it"
      >
        {suggest.isPending ? 'Reading…' : 'Suggest goals'}
      </Button>
    )
  return null
}

function GoalsCard({ d }: { d: Dashboard }) {
  const g = d.goals
  if (!g) return null
  const rows: { key: string; goal?: DashGoal; label: string; open: number; done: number }[] =
    g.goals.map((x) => ({
      key: String(x.id),
      goal: x,
      label: x.title,
      open: x.coverage.open,
      done: x.coverage.finished_week,
    }))
  const max = Math.max(1, g.untraced, ...rows.map((r) => r.open))
  const action = (
    <Link
      to="/p/$key/goals"
      params={{ key: d.project }}
      className="text-xs text-muted underline-offset-2 hover:text-fg hover:underline"
    >
      All goals
    </Link>
  )
  return (
    <Section title="Goals" actions={action}>
      <div className={clsx('space-y-3 p-4', vizClass)}>
        {rows.length === 0 ? (
          <Empty title="No goals yet">
            Goals say what the work is for. Add one on the Goals tab, or let Waypoint suggest some
            from the tasks.
          </Empty>
        ) : (
          <>
            <p className="text-xs text-subtle">Open tasks serving each goal</p>
            <ul className="space-y-2" aria-label="Open tasks by goal">
              {rows.map((r) => (
                <GoalBar
                  key={r.key}
                  goal={r.goal}
                  label={r.label}
                  open={r.open}
                  done={r.done}
                  max={max}
                />
              ))}
              {g.untraced > 0 && (
                <GoalBar label="No goal" open={g.untraced} max={max} project={d.project} untraced />
              )}
            </ul>
          </>
        )}
        {g.suggested.length > 0 && (
          <div className="rounded-lg border border-dashed border-line px-3 py-2">
            <div className="mb-1 flex items-center gap-1.5 text-xs font-medium text-muted">
              <Sparkles className="size-3.5" aria-hidden />
              Suggested, waiting for you
            </div>
            <ul className="space-y-1 text-[13px]">
              {g.suggested.slice(0, 4).map((s) => (
                <li key={s.id} className="truncate" title={s.why}>
                  <Link to="/design/$id" params={{ id: String(s.id) }} className="hover:underline">
                    {s.title}
                  </Link>
                </li>
              ))}
            </ul>
          </div>
        )}
        {g.ask.length > 0 && (
          <div className="text-xs text-muted">
            <div className="mb-1 font-medium">Waypoint can’t tell</div>
            <ul className="list-disc space-y-0.5 pl-4">
              {g.ask.slice(0, 3).map((a) => (
                <li key={a}>{a}</li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </Section>
  )
}

function GoalBar({
  goal,
  label,
  open,
  done,
  max,
  project,
  untraced,
}: {
  goal?: DashGoal
  label: string
  open: number
  done?: number
  max: number
  project?: string
  untraced?: boolean
}) {
  const title = goal ? (
    <Link
      to="/design/$id"
      params={{ id: String(goal.id) }}
      className="min-w-0 flex-1 truncate hover:underline"
    >
      {label}
    </Link>
  ) : (
    <Link
      to="/p/$key/list"
      params={{ key: project! }}
      search={{ q: 'is:open goal:none' }}
      className="min-w-0 flex-1 truncate text-muted hover:underline"
    >
      {label}
    </Link>
  )
  return (
    <li style={goal?.depth ? { paddingLeft: goal.depth * 16 } : undefined}>
      <div className="mb-1 flex items-baseline gap-2 text-[13px]">
        {title}
        {goal?.unclear && (
          <span
            className="shrink-0 rounded bg-sunken px-1 text-[11px] text-muted"
            title="Its words read like a task rather than an outcome someone wants"
          >
            reads like a task
          </span>
        )}
        <span className="shrink-0 tabular-nums">
          <span className="font-medium">{open}</span>
          {!!done && <span className="ml-1.5 text-xs text-ok">+{done} done this week</span>}
        </span>
      </div>
      <div className="h-1.5 rounded-full bg-sunken">
        <div
          className="h-full rounded-full"
          style={{
            width: `max(${(open / max) * 100}%, 3px)`,
            background: untraced ? 'var(--wp-danger)' : 'var(--wp-ok)',
          }}
        />
      </div>
    </li>
  )
}

function FlowCard({ d, details }: { d: Dashboard; details: boolean }) {
  const max = Math.max(1, ...d.stages.map((s) => s.count))
  return (
    <Section title="How work moves">
      <div className="space-y-3 p-4">
        <p className="text-xs text-subtle">Open tasks in each column, and how long they’ve sat</p>
        <ul className="space-y-2.5" aria-label="Open tasks by column">
          {d.stages.map((s) => (
            <StageRow key={s.name} s={s} max={max} project={d.project} details={details} />
          ))}
        </ul>
      </div>
    </Section>
  )
}

function StageRow({
  s,
  max,
  project,
  details,
}: {
  s: DashStage
  max: number
  project: string
  details: boolean
}) {
  return (
    <li data-stage={s.name} data-bottleneck={s.bottleneck || undefined}>
      <div className="mb-1 flex items-baseline gap-2 text-[13px]">
        <Link
          to="/p/$key/list"
          params={{ key: project }}
          search={{ q: `is:open status:"${s.name}"` }}
          className="min-w-0 flex-1 truncate hover:underline"
        >
          {s.name}
        </Link>
        {s.bottleneck && (
          <span className="inline-flex shrink-0 items-center gap-1 text-xs font-medium text-warn">
            <TriangleAlert className="size-3.5" aria-hidden />
            waits longest
          </span>
        )}
        <span className="shrink-0 text-xs text-subtle">
          {s.count > 0 && `typically ${days(s.median_days)}`}
          {details && s.old > 0 && ` · ${s.old} over two weeks`}
        </span>
        <span className="w-8 shrink-0 text-right font-medium tabular-nums" data-stage-count>
          {s.count}
        </span>
      </div>
      <div className="h-2 rounded-full bg-sunken">
        <div
          className="h-full rounded-full"
          style={{
            width: s.count ? `max(${(s.count / max) * 100}%, 3px)` : 0,
            background: s.bottleneck ? 'var(--wp-warn)' : s.color || 'var(--wp-subtle)',
          }}
        />
      </div>
    </li>
  )
}

function PaceCard({ d, details }: { d: Dashboard; details: boolean }) {
  const weeks = d.pace.weeks
  const done = weeks.map((w) => w.done)
  const added = weeks.map((w) => w.added)
  return (
    <Section title="Pace">
      <div className={clsx('space-y-3 p-4', vizClass)}>
        <Legend
          items={[
            { name: 'Done', color: viz.blue, value: fmtNum(d.pace.done_4w) + ' in 4 weeks' },
            { name: 'Added', color: viz.orange, value: fmtNum(d.pace.added_4w) + ' in 4 weeks' },
          ]}
        />
        <BarChart
          ariaLabel="Tasks done and added per week, last twelve weeks"
          labels={weeks.map((w) => shortDate(w.week))}
          titles={weeks.map((w) => 'Week of ' + shortDate(w.week))}
          unit="tasks"
          integer
          height={160}
          series={[
            { name: 'Done', color: viz.blue, values: done },
            { name: 'Added', color: viz.orange, values: added },
          ]}
        />
        {details && (
          <DataTable
            columns={['Week of', 'Done', 'Added']}
            rows={weeks.map((w) => [shortDate(w.week), w.done, w.added])}
          />
        )}
      </div>
    </Section>
  )
}

type Sort = 'attention' | 'pace' | 'name'

export function OverviewPage() {
  const all = useQuery(q.dashboards())
  const [details, setDetails] = useLocal('dash-details', false)
  const [customizing, setCustomizing] = useState(false)
  const [hiddenSaved, setHidden] = useLocal<string[]>('overview-hidden', [])
  const hidden = Array.isArray(hiddenSaved) ? hiddenSaved : []
  const [sort, setSort] = useLocal<Sort>('overview-sort', 'attention')
  const projects = useMemo(() => {
    const list = (all.data?.projects ?? []).filter((p) => !hidden.includes(p.project))
    const urgent = (p: Dashboard) => p.signals.filter((s) => s.tone !== 'info').length
    return [...list].sort((a, b) =>
      sort === 'name'
        ? a.name.localeCompare(b.name)
        : sort === 'pace'
          ? b.pace.done_4w - a.pace.done_4w
          : urgent(b) - urgent(a) || b.open - a.open,
    )
  }, [all.data, hidden, sort])
  const signals = projects
    .flatMap((p) => p.signals.map((s) => ({ s, p })))
    .filter(({ s }) => s.tone !== 'info')
    .sort((a, b) => toneRank(a.s.tone) - toneRank(b.s.tone))

  return (
    <div className="flex-1 overflow-y-auto pb-16">
      <PageHeader
        title="Overview"
        subtitle="Every project you work in, at a glance"
        actions={
          <ViewControls
            details={details}
            setDetails={setDetails}
            customizing={customizing}
            setCustomizing={setCustomizing}
          />
        }
      />
      <div className="mx-auto max-w-6xl space-y-4 px-3 sm:px-5">
        {customizing && all.data && (
          <Section title="Show and sort" className="max-w-md">
            <div className="space-y-3 p-4 text-[13px]">
              <SegmentedControl<Sort>
                label="Sort projects by"
                value={sort}
                onChange={setSort}
                textFrom="always"
                options={[
                  { value: 'attention', label: 'Needs attention', icon: <TriangleAlert /> },
                  { value: 'pace', label: 'Fastest', icon: <Gauge /> },
                  { value: 'name', label: 'Name', icon: <ArrowDownAZ /> },
                ]}
              />
              <ul className="space-y-1">
                {all.data.projects.map((p) => (
                  <li key={p.project}>
                    <label className="flex items-center gap-2">
                      <input
                        type="checkbox"
                        checked={!hidden.includes(p.project)}
                        onChange={() =>
                          setHidden(
                            hidden.includes(p.project)
                              ? hidden.filter((h) => h !== p.project)
                              : [...hidden, p.project],
                          )
                        }
                      />
                      {p.name}
                    </label>
                  </li>
                ))}
              </ul>
            </div>
          </Section>
        )}
        {all.isError ? (
          <LoadError message={all.error.message} retry={() => void all.refetch()} />
        ) : !all.data ? (
          <div className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {[0, 1, 2].map((i) => (
              <Skeleton key={i} className="h-40" />
            ))}
          </div>
        ) : all.data.projects.length === 0 ? (
          <Empty title="No projects yet">Projects you’re a member of show here.</Empty>
        ) : (
          <>
            <AllTiles projects={projects} />
            <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
              {projects.map((p) => (
                <ProjectCard key={p.project} d={p} />
              ))}
            </div>
            <Section title="Needs attention">
              {signals.length === 0 ? (
                <Empty icon={<CircleCheck />} title="Nothing looks wrong anywhere" />
              ) : (
                <ul className="divide-y divide-line" aria-label="Needs attention">
                  {signals.map(({ s, p }) => (
                    <SignalRow key={p.project + s.id} s={s} d={p} details={details} showProject />
                  ))}
                </ul>
              )}
            </Section>
          </>
        )}
      </div>
    </div>
  )
}

const toneRank = (t: DashTone) => (t === 'bad' ? 0 : t === 'warn' ? 1 : 2)

function AllTiles({ projects }: { projects: Dashboard[] }) {
  const sum = (f: (p: Dashboard) => number) => projects.reduce((n, p) => n + f(p), 0)
  const open = sum((p) => p.open)
  const done4 = sum((p) => p.pace.done_4w)
  const prev4 = sum((p) => p.pace.done_prev_4w)
  const traced = sum((p) => p.traced)
  const onGoal = sum((p) => p.on_goal)
  const urgent = sum((p) => p.signals.filter((s) => s.tone !== 'info').length)
  const weeks = projects[0]?.pace.weeks ?? []
  return (
    <div className={clsx('grid grid-cols-2 gap-3 lg:grid-cols-4', vizClass)}>
      <Tile label="Open tasks" value={fmtNum(open)} sub={plural(projects.length, 'project')} />
      <Tile
        label="Done a week"
        value={fmtNum(done4 / 4)}
        sub={<Trend now={done4 / 4} before={prev4 / 4} unit="a week" />}
      >
        <Sparkbars
          name="Done"
          ariaLabel="Tasks done per week across projects, last twelve weeks"
          values={weeks.map((_, i) => sum((p) => p.pace.weeks[i]?.done ?? 0))}
          labels={weeks.map((w) => 'Week of ' + shortDate(w.week))}
          height={28}
        />
      </Tile>
      <Tile label="On a goal" value={traced > 0 ? pct(onGoal / traced) : '–'} sub="of open work" />
      <Tile
        label="Needs attention"
        value={urgent}
        sub={urgent === 0 ? 'nothing looks wrong' : plural(urgent, 'problem')}
        tone={urgent > 0 ? 'bad' : 'ok'}
      />
    </div>
  )
}

function ProjectCard({ d }: { d: Dashboard }) {
  const share = onGoalShare(d)
  const top = d.signals.filter((s) => s.tone !== 'info').slice(0, 2)
  const neck = d.stages.find((s) => s.bottleneck)
  const total = d.stages.reduce((n, s) => n + s.count, 0)
  return (
    <section
      className={clsx('flex min-w-0 flex-col rounded-xl border border-line bg-panel', vizClass)}
      data-project-card={d.project}
    >
      <Link
        to="/p/$key/overview"
        params={{ key: d.project }}
        className="flex items-center gap-2.5 border-b border-line px-4 py-2.5 hover:bg-hover"
      >
        <ProjectIcon icon={d.icon} color={d.color} name={d.name} size={20} />
        <span className="min-w-0 flex-1 truncate text-[13px] font-semibold">{d.name}</span>
        <span className="text-xs text-subtle">{plural(d.open, 'open task')}</span>
      </Link>
      <div className="flex-1 space-y-3 p-4">
        <div className="grid grid-cols-2 gap-3 text-xs text-muted">
          <div>
            <div className="text-lg font-semibold tabular-nums text-fg">
              {fmtNum(perWeek(d.pace))}
            </div>
            done a week
          </div>
          <div>
            <div className="text-lg font-semibold tabular-nums text-fg">
              {!d.scoped ? '–' : share === null ? '–' : pct(share)}
            </div>
            {d.scoped ? 'on a goal' : 'no goals yet'}
          </div>
        </div>
        <Sparkbars
          name="Done"
          ariaLabel={`${d.name}: tasks done per week`}
          values={d.pace.weeks.map((w) => w.done)}
          labels={d.pace.weeks.map((w) => 'Week of ' + shortDate(w.week))}
          height={28}
        />
        {total > 0 && (
          <div>
            <div
              className="flex h-2 gap-[2px] overflow-hidden rounded-full"
              role="img"
              aria-label={d.stages
                .filter((s) => s.count)
                .map((s) => `${s.count} ${s.name}`)
                .join(', ')}
            >
              {d.stages
                .filter((s) => s.count)
                .map((s) => (
                  <div
                    key={s.name}
                    title={`${s.count} in ${s.name}`}
                    style={{
                      flexGrow: s.count,
                      flexBasis: 0,
                      background: s.bottleneck ? 'var(--wp-warn)' : s.color,
                    }}
                  />
                ))}
            </div>
            {neck && (
              <div className="mt-1 text-xs text-warn">
                Work waits in {neck.name} ({days(neck.median_days)})
              </div>
            )}
          </div>
        )}
        {top.length > 0 ? (
          <ul className="space-y-1 text-xs">
            {top.map((s) => {
              const Icon = TONE[s.tone].icon
              return (
                <li key={s.id} className="flex items-start gap-1.5">
                  <Icon
                    className={clsx('mt-px size-3.5 shrink-0', TONE[s.tone].className)}
                    aria-label={TONE[s.tone].label}
                  />
                  <span className="text-muted">{s.title}</span>
                </li>
              )
            })}
          </ul>
        ) : (
          <div className="flex items-center gap-1.5 text-xs text-ok">
            <CircleCheck className="size-3.5" aria-hidden />
            Nothing looks wrong
          </div>
        )}
      </div>
    </section>
  )
}
