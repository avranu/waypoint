import { useMemo, useState, type ReactNode } from 'react'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { ChevronDown, ChevronRight, Filter, Map as MapIcon, X } from 'lucide-react'
import { q } from '@/lib/queries'
import {
  duration,
  hours,
  last30Days,
  localISO,
  plural,
  points as fmtPoints,
  shortDate,
} from '@/lib/format'
import { useLocal } from '@/lib/hooks'
import { Tip } from '@/components/ui/help'
import { Empty, Progress, Section, Skeleton } from '@/components/ui/misc'
import {
  BarChart,
  DataTable,
  fmtNum,
  HBarList,
  Legend,
  LineChart,
  StackedAreaChart,
  viz,
} from '@/components/charts'
import { useOpenIssue, useProject } from './Project'
import type { Category, Forecast, Project, Sprint, WeekStat } from '@/lib/types'

export function ReportsPage() {
  const project = useProject()
  const key = project.key
  const active = project.sprints?.find((s) => s.state === 'active')
  const velocity = useQuery(q.velocity(key))
  const showVelocity = project.settings.mode === 'sprints' || (velocity.data?.length ?? 0) > 0
  const [weeks, setWeeks] = useLocal<number>('reports-weeks', 12)

  return (
    <div className="flex-1 overflow-y-auto">
      <div className="mx-auto grid max-w-screen-2xl grid-cols-1 gap-3 px-3 pb-16 pt-3 sm:gap-4 sm:px-5 sm:pt-4 lg:grid-cols-2 2xl:grid-cols-3">
        {active && <BurndownCard sprint={active} />}
        <WeeklyCards projectKey={key} weeks={weeks} setWeeks={setWeeks} />
        <FlowCard projectKey={key} />
        {showVelocity && <VelocityCard project={project} data={velocity.data} />}
        <TimeCard projectKey={key} />
        <ForecastCard projectKey={key} />
        <RoadmapCard projectKey={key} className="lg:col-span-2 2xl:col-span-3" />
      </div>
    </div>
  )
}

function Card({
  title,
  actions,
  subtitle,
  children,
  className,
  dim,
}: {
  title: string
  actions?: ReactNode
  subtitle?: ReactNode
  children: ReactNode
  className?: string
  dim?: boolean
}) {
  return (
    <Section title={title} actions={actions} className={clsx('min-w-0', className)}>
      <div className={clsx('space-y-3 p-4 transition-opacity', dim && 'opacity-60')}>
        {subtitle && <p className="text-xs text-subtle">{subtitle}</p>}
        {children}
      </div>
    </Section>
  )
}

function Segmented({
  value,
  options,
  onChange,
  label,
}: {
  value: number
  options: { value: number; label: string }[]
  onChange: (v: number) => void
  label: string
}) {
  return (
    <div
      role="radiogroup"
      aria-label={label}
      className="-my-1 flex shrink-0 rounded-md bg-sunken p-0.5"
    >
      {options.map((o) => (
        <button
          key={o.value}
          role="radio"
          aria-checked={o.value === value}
          onClick={() => onChange(o.value)}
          className={clsx(
            'h-7 rounded px-2 text-xs font-medium transition-colors sm:h-6',
            o.value === value ? 'bg-panel text-fg shadow-card' : 'text-muted hover:text-fg',
          )}
        >
          {o.label}
        </button>
      ))}
    </div>
  )
}

function ChartSkeleton({ height = 200 }: { height?: number }) {
  return <div style={{ height }} className="w-full animate-pulse rounded-md bg-sunken" />
}

function Note({ children }: { children: ReactNode }) {
  return <p className="py-6 text-center text-[13px] text-muted">{children}</p>
}

function ErrorNote({ error }: { error: unknown }) {
  return (
    <p className="text-[13px] text-danger">
      {error instanceof Error ? error.message : 'Could not load this report.'}
    </p>
  )
}

/** "Nov 3" plus "in 3 weeks" relative to today. */
function fromNow(d: string): string {
  const days = Math.round(
    (new Date(d + 'T00:00:00').getTime() - new Date(localISO(new Date()) + 'T00:00:00').getTime()) /
      86400000,
  )
  if (days <= 0) return 'today'
  if (days < 7) return `in ${plural(days, 'day')}`
  const w = Math.round(days / 7)
  return `in about ${plural(w, 'week')}`
}

function lastValue(vals: (number | null)[]): number | null {
  for (let i = vals.length - 1; i >= 0; i--) if (vals[i] !== null) return vals[i]
  return null
}

function ForecastCard({ projectKey, className }: { projectKey: string; className?: string }) {
  const [draft, setDraft] = useState('')
  const [scope, setScope] = useState('')
  const [scoping, setScoping] = useState(false)
  const fq = useQuery({
    ...q.forecast(projectKey, scope),
    placeholderData: keepPreviousData,
    retry: false,
  })
  const fc = fq.data
  const apply = (v: string) => setScope(v.trim())

  return (
    <Card
      title="Forecast"
      className={className}
      dim={fq.isPlaceholderData}
      actions={
        <>
          <button
            type="button"
            aria-label="Limit the forecast to a query"
            title={scope ? `Limited to ${scope}` : 'Limit the forecast to a query'}
            aria-pressed={scoping || !!scope}
            onClick={() => setScoping((v) => !v)}
            className={clsx(
              'flex size-6 items-center justify-center rounded text-subtle hover:bg-hover hover:text-fg',
              (scoping || scope) && 'text-accent',
            )}
          >
            <Filter className="size-3.5" />
          </button>
          <Tip label="How the forecast works">
            Dates come from replaying recent weekly throughput 2,000 times. P85 means 85% of
            simulations finished by that date, a safer date to promise. They are estimates and move
            as the weekly pace changes.
          </Tip>
        </>
      }
    >
      {(scoping || scope) && (
        <form
          onSubmit={(e) => {
            e.preventDefault()
            apply(draft)
          }}
          className="flex items-center gap-2"
        >
          <label className="flex min-w-0 flex-1 items-center gap-2 sm:max-w-md">
            <span className="shrink-0 text-xs font-medium text-muted">Scope</span>
            <span className="relative min-w-0 flex-1">
              <input
                autoFocus={!scope}
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
                onBlur={() => apply(draft)}
                placeholder="Whole project"
                title="Limit the forecast to a query, e.g. under:WAYP-3 for one epic"
                className="h-8 w-full rounded-lg border border-line bg-panel pl-3 pr-8 text-[13px] placeholder:text-subtle focus:border-accent focus:outline-none focus:ring-2 focus:ring-accent/20"
                enterKeyHint="search"
                spellCheck={false}
                autoCapitalize="off"
              />
              {draft && (
                <button
                  type="button"
                  aria-label="Clear scope"
                  onClick={() => {
                    setDraft('')
                    apply('')
                  }}
                  className="absolute right-1 top-1/2 flex size-6 -translate-y-1/2 items-center justify-center rounded text-subtle hover:bg-hover hover:text-fg"
                >
                  <X className="size-3.5" />
                </button>
              )}
            </span>
            <Tip label="About the scope">
              Filters the forecast to the issues a query matches, for example under:WAYP-3 for one
              epic and its children. Empty means the whole project.
            </Tip>
          </label>
        </form>
      )}
      {fq.error && <ErrorNote error={fq.error} />}
      {!fc ? !fq.error && <Skeleton className="h-40 w-full" /> : <ForecastBody fc={fc} />}
    </Card>
  )
}

function ForecastBody({ fc }: { fc: Forecast }) {
  const unit = fc.basis === 'points' ? 'pts' : 'issues'
  const n = fc.weekly_throughput.length
  const weeks = useMemo(() => {
    const now = new Date()
    const monday = new Date(
      now.getFullYear(),
      now.getMonth(),
      now.getDate() - ((now.getDay() + 6) % 7),
    )
    return Array.from({ length: n }, (_, i) => {
      const d = new Date(monday)
      d.setDate(d.getDate() - 7 * (n - i))
      return localISO(d)
    })
  }, [n])
  const done = fc.remaining === 0
  const date = (label: string, d?: string) =>
    d ? <Stat key={label} label={label} value={shortDate(d)} sub={fromNow(d)} /> : null

  return (
    <div className="space-y-4">
      {done ? (
        <p className="text-[13px] text-muted">All done: nothing remaining.</p>
      ) : !fc.p50 ? (
        <p className="text-[13px] text-muted">{fc.message || 'No forecast yet'}</p>
      ) : null}
      <dl className="grid grid-cols-2 gap-x-4 gap-y-3 sm:grid-cols-3">
        {!done && date('Likely (P50)', fc.p50)}
        {!done && date('P85', fc.p85)}
        {!done && date('P95', fc.p95)}
        <Stat
          label="Remaining"
          value={`${fmtNum(fc.remaining)} ${unit}`}
          sub={fc.basis === 'points' ? plural(fc.remaining_issues, 'issue') : undefined}
        />
        <Stat label="Avg per week" value={`${fmtNum(fc.average_per_week)} ${unit}`} />
        <Stat
          label="Unestimated"
          value={String(fc.unestimated)}
          sub={
            fc.basis === 'points'
              ? fc.unestimated
                ? `counted as ${fmtPoints(fc.assumed_points)} pts each`
                : undefined
              : 'forecast by issue count'
          }
        />
      </dl>
      {n > 0 && fc.weekly_throughput.some((v) => v > 0) && (
        <div>
          <div className="mb-1 text-xs text-muted">
            {fc.basis === 'points' ? 'Points' : 'Issues'} finished per week, last{' '}
            {plural(n, 'week')}
          </div>
          <BarChart
            ariaLabel={`Weekly throughput over the last ${n} weeks`}
            labels={weeks.map((w) => shortDate(w))}
            titles={weeks.map((w) => 'Week of ' + shortDate(w))}
            unit={`${fc.basis === 'points' ? 'points' : 'issues'} per week`}
            integer={fc.basis !== 'points'}
            height={160}
            series={[
              {
                name: fc.basis === 'points' ? 'Points finished' : 'Issues finished',
                color: viz.blue,
                values: fc.weekly_throughput,
              },
            ]}
          />
        </div>
      )}
    </div>
  )
}

function Stat({ label, value, sub }: { label: string; value: string; sub?: string }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-muted">{label}</dt>
      <dd className="truncate text-[13px] font-medium">{value}</dd>
      {sub && <dd className="truncate text-xs text-subtle">{sub}</dd>}
    </div>
  )
}

function RoadmapCard({ projectKey, className }: { projectKey: string; className?: string }) {
  const rq = useQuery(q.roadmap(projectKey))
  const openIssue = useOpenIssue()
  const epics = rq.data
  const [open, setOpen] = useState<boolean | undefined>(undefined)
  const shown = open ?? (!epics || epics.length <= 5)
  const fold = epics && epics.length > 0 && (
    <button
      onClick={() => setOpen(!shown)}
      aria-expanded={shown}
      className="flex items-center gap-1 text-xs text-muted hover:text-fg"
    >
      {shown ? <ChevronDown className="size-3.5" /> : <ChevronRight className="size-3.5" />}
      {plural(epics.length, 'epic')}
    </button>
  )
  return (
    <Section title="Roadmap" actions={fold || undefined} className={clsx('min-w-0', className)}>
      {rq.error ? (
        <div className="p-4">
          <ErrorNote error={rq.error} />
        </div>
      ) : !shown ? null : !epics ? (
        <div className="space-y-2 p-4">
          <Skeleton className="h-10 w-full" />
          <Skeleton className="h-10 w-full" />
        </div>
      ) : epics.length === 0 ? (
        <Empty icon={<MapIcon />} title="No open epics">
          Create epics and nest work under them to see progress and projected finish dates here.
        </Empty>
      ) : (
        <>
          <div className="hidden grid-cols-[minmax(0,1fr)_200px_88px_88px] gap-4 border-b border-line px-4 py-1.5 text-xs text-subtle sm:grid">
            <span className="flex items-center gap-1">
              Epic, in rank order
              <Tip label="About these dates">
                Dates are 85% confident. “In this order” assumes the team finishes each epic before
                starting the next.
              </Tip>
            </span>
            <span>Progress</span>
            <span title="85% of simulations finished this epic by then, if worked on alone">
              On its own
            </span>
            <span title="85% confident, if epics are worked one after another in this order">
              In this order
            </span>
          </div>
          <ol className="divide-y divide-line">
            {epics.map((e) => (
              <li
                key={e.epic.key}
                className="grid gap-2 px-4 py-3 sm:grid-cols-[minmax(0,1fr)_200px_88px_88px] sm:items-center sm:gap-4"
              >
                <button onClick={() => openIssue(e.epic.key)} className="min-w-0 text-left">
                  <div className="truncate text-[13px] font-medium hover:underline">
                    {e.epic.title}
                  </div>
                  <div className="text-xs text-subtle">{e.epic.key}</div>
                </button>
                <div className="min-w-0">
                  <Progress value={e.done} max={e.total} />
                  <div className="mt-1 text-xs text-muted">
                    {e.done}/{e.total} issues
                    {e.points > 0 && ` · ${fmtPoints(e.done_points)}/${fmtPoints(e.points)} pts`}
                  </div>
                </div>
                <div className="flex gap-4 text-xs sm:contents">
                  <EpicDate label="On its own" date={e.forecast.p85} message={e.forecast.message} />
                  <EpicDate label="In this order" date={e.sequential_p85} />
                </div>
              </li>
            ))}
          </ol>
        </>
      )}
    </Section>
  )
}

function EpicDate({ label, date, message }: { label: string; date?: string; message?: string }) {
  return (
    <div title={!date ? message : undefined}>
      <div className="text-subtle sm:hidden">{label}</div>
      <div className="text-[13px] font-medium tabular-nums">
        {date ? shortDate(date) : <span className="text-subtle">—</span>}
      </div>
    </div>
  )
}

function BurndownCard({ sprint }: { sprint: Sprint }) {
  const bq = useQuery(q.burndown(sprint.id))
  const bd = bq.data
  const view = useMemo(() => {
    if (!bd || bd.days.length === 0) return null
    const usePoints = bd.total_points > 0
    const remaining = bd.days.map((d) => (usePoints ? d.remaining_points : d.remaining_issues))
    const n0 = bd.days[0].remaining_issues ?? Math.max(0, ...remaining.map((v) => v ?? 0))
    const len = bd.days.length
    const ideal = usePoints
      ? bd.days.map((d) => d.ideal_points)
      : bd.days.map((_, i) => (len > 1 ? n0 * (1 - i / (len - 1)) : n0))
    return {
      unit: usePoints ? 'points' : 'issues',
      labels: bd.days.map((d) => shortDate(d.date)),
      titles: bd.days.map((d) =>
        new Date(d.date + 'T00:00:00').toLocaleDateString(undefined, {
          weekday: 'short',
          month: 'short',
          day: 'numeric',
        }),
      ),
      remaining,
      ideal,
    }
  }, [bd])
  return (
    <Card
      title="Sprint burndown"
      subtitle={
        <>
          {sprint.name}
          {sprint.start_date &&
            sprint.end_date &&
            ` · ${shortDate(sprint.start_date)} – ${shortDate(sprint.end_date)}`}
        </>
      }
    >
      {bq.error ? (
        <ErrorNote error={bq.error} />
      ) : !bd ? (
        <ChartSkeleton />
      ) : !view ? (
        <Note>Give this sprint start and end dates to see its burndown.</Note>
      ) : (
        <>
          <Legend
            items={[
              {
                name: 'Remaining',
                color: viz.blue,
                kind: 'line',
                value: fmtNum(lastValue(view.remaining)),
              },
              { name: 'Ideal', color: viz.neutral, kind: 'dash' },
            ]}
          />
          <LineChart
            ariaLabel={`Burndown of ${view.unit} remaining in ${sprint.name}`}
            labels={view.labels}
            titles={view.titles}
            unit={`${view.unit} remaining`}
            integer={view.unit === 'issues'}
            series={[
              { name: 'Ideal', color: viz.neutral, values: view.ideal, dashed: true },
              { name: 'Remaining', color: viz.blue, values: view.remaining, area: true },
            ]}
          />
          <DataTable
            columns={['Day', 'Remaining', 'Ideal']}
            rows={view.labels.map((l, i) => [
              l,
              view.remaining[i] === null ? '–' : view.remaining[i]!,
              Math.round(view.ideal[i] * 10) / 10,
            ])}
          />
        </>
      )}
    </Card>
  )
}

const weekOptions = [
  { value: 4, label: '4w' },
  { value: 12, label: '12w' },
  { value: 26, label: '26w' },
]

function WeeklyCards({
  projectKey,
  weeks,
  setWeeks,
}: {
  projectKey: string
  weeks: number
  setWeeks: (n: number) => void
}) {
  const tq = useQuery({ ...q.throughput(projectKey, weeks), placeholderData: keepPreviousData })
  const picker = (
    <Segmented label="Weeks shown" value={weeks} options={weekOptions} onChange={setWeeks} />
  )
  return (
    <>
      <BurnupCard
        data={tq.data}
        error={tq.error}
        dim={tq.isPlaceholderData}
        picker={picker}
        weeks={weeks}
      />
      <ThroughputCard data={tq.data} error={tq.error} dim={tq.isPlaceholderData} weeks={weeks} />
    </>
  )
}

function weekTitles(data: WeekStat[]) {
  return {
    labels: data.map((w) => shortDate(w.week)),
    titles: data.map((w) => 'Week of ' + shortDate(w.week)),
  }
}

function BurnupCard({
  data,
  error,
  dim,
  picker,
  weeks,
}: {
  data?: WeekStat[]
  error: unknown
  dim: boolean
  picker: ReactNode
  weeks: number
}) {
  // Use points only when done work carries them too; otherwise a flat zero line looks broken.
  const usePoints =
    !!data?.some((w) => w.scope_points > 0) && !!data?.some((w) => w.done_points > 0)
  const unit = usePoints ? 'points' : 'issues'
  const scope = data?.map((w) => (usePoints ? w.scope_points : w.scope_issues)) ?? []
  const done = data?.map((w) => (usePoints ? w.done_points : w.done_issues)) ?? []
  const empty = !!data && scope.every((v) => v === 0)
  return (
    <Card
      title="Burnup"
      actions={picker}
      dim={dim}
      subtitle={`Total scope vs work done, in ${unit}, over the last ${weeks} weeks`}
    >
      {error ? (
        <ErrorNote error={error} />
      ) : !data ? (
        <ChartSkeleton />
      ) : empty ? (
        <Note>No issues yet.</Note>
      ) : (
        <>
          <Legend
            items={[
              { name: 'Done', color: viz.blue, kind: 'line', value: fmtNum(done[done.length - 1]) },
              {
                name: 'Scope',
                color: viz.neutral,
                kind: 'line',
                value: fmtNum(scope[scope.length - 1]),
              },
            ]}
          />
          <LineChart
            ariaLabel={`Burnup: cumulative scope and done ${unit} per week`}
            {...weekTitles(data)}
            unit={unit}
            integer={!usePoints}
            series={[
              { name: 'Scope', color: viz.neutral, values: scope },
              { name: 'Done', color: viz.blue, values: done, area: true },
            ]}
          />
          <DataTable
            columns={['Week of', 'Done', 'Scope']}
            rows={data.map((w, i) => [shortDate(w.week), done[i], scope[i]])}
          />
        </>
      )}
    </Card>
  )
}

function ThroughputCard({
  data,
  error,
  dim,
  weeks,
}: {
  data?: WeekStat[]
  error: unknown
  dim: boolean
  weeks: number
}) {
  const completed = data?.map((w) => w.completed_issues) ?? []
  const created = data?.map((w) => w.created_issues) ?? []
  const sum = (a: number[]) => a.reduce((s, v) => s + v, 0)
  return (
    <Card
      title="Throughput"
      dim={dim}
      subtitle={`Issues finished vs created each week, last ${weeks} weeks`}
    >
      {error ? (
        <ErrorNote error={error} />
      ) : !data ? (
        <ChartSkeleton />
      ) : (
        <>
          <Legend
            items={[
              { name: 'Completed', color: viz.blue, value: fmtNum(sum(completed)) },
              { name: 'Created', color: viz.orange, value: fmtNum(sum(created)) },
            ]}
          />
          <BarChart
            ariaLabel="Issues completed and created per week"
            {...weekTitles(data)}
            unit="issues"
            integer
            series={[
              { name: 'Completed', color: viz.blue, values: completed },
              { name: 'Created', color: viz.orange, values: created },
            ]}
          />
          <DataTable
            columns={['Week of', 'Completed', 'Created']}
            rows={data.map((w) => [shortDate(w.week), w.completed_issues, w.created_issues])}
          />
        </>
      )}
    </Card>
  )
}

const flowSeries: { cat: Category; name: string; color: string }[] = [
  { cat: 'done', name: 'Done', color: viz.aqua },
  { cat: 'in_progress', name: 'In progress', color: viz.blue },
  { cat: 'todo', name: 'To do', color: viz.yellow },
  { cat: 'backlog', name: 'Backlog', color: viz.neutral },
]

const dayOptions = [
  { value: 14, label: '14d' },
  { value: 30, label: '30d' },
  { value: 90, label: '90d' },
]

function FlowCard({ projectKey }: { projectKey: string }) {
  const [days, setDays] = useLocal<number>('reports-flow-days', 30)
  const fq = useQuery({ ...q.flow(projectKey, days), placeholderData: keepPreviousData })
  const data = fq.data
  const series = useMemo(
    () =>
      flowSeries.map((s) => ({
        name: s.name,
        color: s.color,
        values: data?.map((d) => d.counts[s.cat] ?? 0) ?? [],
      })),
    [data],
  )
  const last = (data?.length ?? 0) - 1
  return (
    <Card
      title="Cumulative flow"
      dim={fq.isPlaceholderData}
      actions={
        <Segmented label="Days shown" value={days} options={dayOptions} onChange={setDays} />
      }
    >
      {fq.error ? (
        <ErrorNote error={fq.error} />
      ) : !data ? (
        <ChartSkeleton />
      ) : (
        <>
          <Legend
            items={[...series]
              .reverse()
              .map((s) => ({ name: s.name, color: s.color, value: fmtNum(s.values[last]) }))}
          />
          <StackedAreaChart
            ariaLabel="Cumulative flow: issues per state per day"
            labels={data.map((d) => shortDate(d.date))}
            unit="issues"
            integer
            series={series}
          />
          <DataTable
            columns={['Day', ...[...flowSeries].reverse().map((s) => s.name)]}
            rows={data.map((d) => [
              shortDate(d.date),
              ...[...flowSeries].reverse().map((s) => d.counts[s.cat] ?? 0),
            ])}
          />
        </>
      )}
    </Card>
  )
}

function VelocityCard({
  project,
  data,
}: {
  project: Project
  data?: { name: string; committed: number; completed: number }[]
}) {
  const avg = data && data.length ? data.reduce((s, v) => s + v.completed, 0) / data.length : 0
  return (
    <Card
      title="Velocity"
      subtitle="Points committed at sprint start vs completed, per closed sprint"
    >
      {!data ? (
        <ChartSkeleton />
      ) : data.length === 0 ? (
        <Note>
          {project.settings.mode === 'sprints'
            ? 'Close a sprint to start tracking velocity.'
            : 'No closed sprints yet.'}
        </Note>
      ) : (
        <>
          <Legend
            items={[
              { name: 'Completed', color: viz.blue, value: `avg ${fmtNum(avg)}` },
              { name: 'Committed', color: viz.neutral },
            ]}
          />
          <BarChart
            ariaLabel="Velocity: committed and completed points per sprint"
            labels={data.map((v) => (v.name.length > 10 ? v.name.slice(0, 9) + '…' : v.name))}
            titles={data.map((v) => v.name)}
            unit="points"
            series={[
              { name: 'Committed', color: viz.neutral, values: data.map((v) => v.committed) },
              { name: 'Completed', color: viz.blue, values: data.map((v) => v.completed) },
            ]}
          />
          <DataTable
            columns={['Sprint', 'Committed', 'Completed']}
            rows={data.map((v) => [v.name, v.committed, v.completed])}
          />
        </>
      )}
    </Card>
  )
}

function TimeCard({ projectKey }: { projectKey: string }) {
  const openIssue = useOpenIssue()
  const [from, to] = useMemo(last30Days, [])
  const tq = useQuery(q.timeReport(from, to, projectKey))
  const r = tq.data
  const users = r?.by_user ?? []
  const issues = (r?.by_issue ?? []).slice(0, 8)
  return (
    <Card title="Time tracked" subtitle="Last 30 days, everyone on the project">
      {tq.error ? (
        <ErrorNote error={tq.error} />
      ) : !r ? (
        <ChartSkeleton height={160} />
      ) : r.total_seconds === 0 ? (
        <Note>No time tracked in the last 30 days.</Note>
      ) : (
        <>
          <div>
            <span className="text-3xl font-semibold tracking-tight">{hours(r.total_seconds)}</span>
            <span className="ml-1 text-[13px] text-muted">hours total</span>
          </div>
          <div className="grid gap-5 sm:grid-cols-2 lg:grid-cols-1 xl:grid-cols-2">
            {users.length > 0 && (
              <div className="min-w-0">
                <h4 className="mb-2 text-xs font-medium text-muted">By person</h4>
                <HBarList
                  ariaLabel="Time tracked by person"
                  items={users.map((u) => ({
                    key: u.key,
                    label: u.label || u.key,
                    value: u.seconds,
                    display: duration(u.seconds),
                  }))}
                />
              </div>
            )}
            <div className="min-w-0">
              <h4 className="mb-2 text-xs font-medium text-muted">Top issues</h4>
              <HBarList
                ariaLabel="Time tracked by issue"
                items={issues.map((b) => ({
                  key: b.key,
                  prefix: b.key === '—' ? undefined : b.key,
                  label: b.label,
                  value: b.seconds,
                  display: duration(b.seconds),
                  onClick: b.key === '—' ? undefined : () => openIssue(b.key),
                }))}
              />
            </div>
          </div>
        </>
      )}
    </Card>
  )
}
