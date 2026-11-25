import type { ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { CircleDollarSign, ListChecks, Plane } from 'lucide-react'
import { autopilotActivityQuery, type AutopilotDay } from '@/lib/autopilot'
import { BarChart, Legend, viz, vizClass } from '@/components/charts'
import { Skeleton } from '@/components/ui/misc'

const DAYS = 14

const parse = (d: string) => {
  const [y, m, day] = d.split('-').map(Number)
  return new Date(y, m - 1, day)
}
const dayLabel = (d: string) =>
  parse(d).toLocaleDateString(undefined, { month: 'short', day: 'numeric' })
const weekday = (d: string) =>
  parse(d).toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' })

function Panel({
  icon,
  title,
  total,
  children,
}: {
  icon: ReactNode
  title: string
  total: string
  children: ReactNode
}) {
  return (
    <div className="min-w-0 rounded-xl border border-line bg-panel px-3.5 py-3">
      <div className="mb-1 flex items-center justify-between gap-2">
        <div className="flex items-center gap-1.5 text-xs text-muted [&_svg]:size-3.5">
          {icon}
          {title}
        </div>
        <span className="text-xs font-semibold tabular-nums">{total}</span>
      </div>
      {children}
    </div>
  )
}

const money = (v: number) => `$${v >= 100 ? Math.round(v) : v.toFixed(2)}`

export function Activity() {
  const { data, error } = useQuery(autopilotActivityQuery(DAYS))
  if (!data && error)
    return (
      <p className="rounded-xl border border-line p-4 text-[13px] text-danger">
        The last two weeks didn’t load: {error.message}
      </p>
    )
  if (!data)
    return (
      <div className="grid gap-2 md:grid-cols-3">
        <Skeleton className="h-44" />
        <Skeleton className="h-44" />
        <Skeleton className="h-44" />
      </div>
    )
  const days: AutopilotDay[] = data.days
  const labels = days.map((d) => dayLabel(d.day))
  const titles = days.map((d) => weekday(d.day))
  const sum = (f: (d: AutopilotDay) => number) => days.reduce((n, d) => n + f(d), 0)
  const finished = sum((d) => d.finished)
  const moved = sum((d) => d.moved_on)
  const started = sum((d) => d.started)
  const spend = sum((d) => d.spend_usd)
  return (
    <section
      aria-label="Last two weeks"
      data-testid="autopilot-activity"
      className={`grid gap-2 md:grid-cols-3 ${vizClass}`}
    >
      <Panel icon={<ListChecks />} title="Tasks finished" total={String(finished)}>
        <BarChart
          ariaLabel="Tasks finished per day"
          labels={labels}
          titles={titles}
          height={132}
          integer
          series={[
            { name: 'Finished', color: viz.blue, values: days.map((d) => d.finished) },
            { name: 'Autopilot moved on', color: viz.aqua, values: days.map((d) => d.moved_on) },
          ]}
        />
        <Legend
          className="mt-1"
          items={[
            { name: 'Finished', color: viz.blue, kind: 'box', value: String(finished) },
            { name: 'Moved on', color: viz.aqua, kind: 'box', value: String(moved) },
          ]}
        />
      </Panel>
      <Panel icon={<Plane />} title="Autopilot starts" total={String(started)}>
        <BarChart
          ariaLabel="Next tasks autopilot started per day"
          labels={labels}
          titles={titles}
          height={132}
          integer
          series={[{ name: 'Started', color: viz.orange, values: days.map((d) => d.started) }]}
        />
      </Panel>
      <Panel icon={<CircleDollarSign />} title="Spend at list price" total={money(spend)}>
        <BarChart
          ariaLabel="Spend per day"
          labels={labels}
          titles={titles}
          height={132}
          format={money}
          series={[{ name: 'Spend', color: viz.yellow, values: days.map((d) => d.spend_usd) }]}
        />
      </Panel>
    </section>
  )
}
