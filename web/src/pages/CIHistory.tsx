import { useEffect, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import { useNavigate, useSearch } from '@tanstack/react-router'
import clsx from 'clsx'
import { AlertTriangle, CalendarDays, CalendarRange, History, Loader2 } from 'lucide-react'
import { ciHistoryQuery } from '@/lib/queries'
import { duration, relativeTime } from '@/lib/format'
import type { CIHistory, CIHistoryBucket, CIHistoryPoint, CIHistoryStats } from '@/lib/ci'
import { DataTable, Legend, LineChart, fmtNum, viz, vizClass } from '@/components/charts'
import { Select } from '@/components/ui/input'
import { Empty, PageHeader, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { CITabs } from '@/components/ci-tabs'

type Range = '30' | '90' | '365'

const minutes = (s: number | null) => (s === null ? null : Math.round(s / 6) / 10)
const fmtMinutes = (m: number) => duration(m * 60)
const hours = (s: number) => Math.round(s / 360) / 10
const failPct = (st: CIHistoryStats) => (st.jobs ? (100 * st.failed) / st.jobs : null)

export function CIHistoryPage() {
  const search = useSearch({ from: '/app/ci/history' })
  const navigate = useNavigate()
  const days = search.days ?? 30
  const bucket: CIHistoryBucket = search.bucket ?? (days >= 120 ? 'week' : 'day')
  const query = useQuery(ciHistoryQuery(days, bucket, search.repo))
  useEffect(() => {
    document.title = 'CI history · Waypoint'
  }, [])
  const set = (f: Partial<typeof search>) =>
    navigate({ to: '/ci/history', search: (s) => ({ ...s, ...f }), replace: true })
  const h = query.data
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto pb-16">
      <div className="mx-auto w-full max-w-6xl">
        <PageHeader
          icon={<History className="size-5 text-accent" />}
          title="CI history"
          subtitle={
            h ? (
              <span className="inline-flex items-center gap-1.5">
                {h.since
                  ? `kept since ${new Date(h.since).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })}`
                  : 'nothing kept yet'}
                {' · '}read {relativeTime(h.generated_at)}
                {query.isFetching && (
                  <Loader2 className="size-3 animate-spin" aria-label="Reading" />
                )}
              </span>
            ) : (
              'How CI fared over time'
            )
          }
          actions={<CITabs />}
        />
        <div className="space-y-4 px-4 pb-6 sm:px-6">
          <div className="flex flex-wrap items-center gap-2">
            <SegmentedControl<Range>
              label="Range"
              value={String(days) as Range}
              onChange={(v) => set({ days: Number(v), bucket: undefined })}
              textFrom="always"
              options={[
                { value: '30', label: '30 days', icon: <CalendarDays /> },
                { value: '90', label: '90 days', icon: <CalendarDays /> },
                { value: '365', label: 'A year', icon: <CalendarRange /> },
              ]}
            />
            <SegmentedControl<CIHistoryBucket>
              label="By"
              value={bucket}
              onChange={(v) => set({ bucket: v })}
              textFrom="always"
              options={[
                { value: 'day', label: 'Day', icon: <CalendarDays /> },
                { value: 'week', label: 'Week', icon: <CalendarRange /> },
              ]}
            />
            {h && (h.repos.length > 1 || search.repo) && (
              <Select
                aria-label="Repo"
                value={search.repo ?? ''}
                onChange={(e) => set({ repo: e.target.value || undefined })}
                className="w-48 max-w-full"
              >
                <option value="">All repos</option>
                {h.repos.map((r) => (
                  <option key={r} value={r}>
                    {r}
                  </option>
                ))}
              </Select>
            )}
          </div>
          {query.isPending ? (
            <div className="space-y-3">
              <Skeleton className="h-20" />
              <Skeleton className="h-64" />
            </div>
          ) : query.isError || !h ? (
            <Empty icon={<AlertTriangle />} title="Couldn't read CI history">
              {String(query.error ?? '')}
            </Empty>
          ) : !h.since ? (
            <Empty icon={<History />} title="No CI history kept yet">
              Waypoint keeps every finished Forgejo Actions run and job of the repos attached to
              your projects, and reads each repo’s older runs once, within the hour after it is
              attached. Check back then.
            </Empty>
          ) : (
            <Report h={h} />
          )}
        </div>
      </div>
    </div>
  )
}

function Report({ h }: { h: CIHistory }) {
  const pts = h.points
  const short: Intl.DateTimeFormatOptions = { month: 'short', day: 'numeric', timeZone: 'UTC' }
  const labels = pts.map((p) => new Date(p.at).toLocaleDateString(undefined, short))
  const titles = pts.map((p) =>
    h.bucket === 'week'
      ? `Week of ${new Date(p.at).toLocaleDateString(undefined, short)}`
      : new Date(p.at).toLocaleDateString(undefined, { ...short, weekday: 'short' }),
  )
  const since = h.since ? new Date(h.since).getTime() : 0
  const kept = (p: CIHistoryPoint) => new Date(p.to).getTime() > since
  const col = (f: (p: CIHistoryPoint) => number | null) => pts.map((p) => (kept(p) ? f(p) : null))
  const rowsOf = (f: (p: CIHistoryPoint, i: number) => (string | number)[]) =>
    pts.flatMap((p, i) => (kept(p) ? [f(p, i)] : []))
  const cell = (v: number | null, fmt: (v: number) => string = (x) => String(x)) =>
    v === null ? '–' : fmt(v)
  const per = h.bucket === 'week' ? 'week' : 'day'
  return (
    <>
      <Compare h={h} />
      <div className="grid gap-4 lg:grid-cols-2 [&>*]:min-w-0">
        <ChartSection
          title={`Jobs per ${per}`}
          help="Finished jobs (skipped ones aside), and how many of them failed."
          legend={[
            { name: 'Jobs', color: viz.blue },
            { name: 'Failed', color: viz.orange },
          ]}
          table={{
            columns: [h.bucket === 'week' ? 'Week of' : 'Day', 'Jobs', 'Failed'],
            rows: rowsOf((p, i) => [labels[i], p.jobs, p.failed]),
          }}
        >
          <LineChart
            ariaLabel={`Jobs and failed jobs per ${per}`}
            labels={labels}
            titles={titles}
            integer
            series={[
              { name: 'Jobs', color: viz.blue, values: col((p) => p.jobs), area: true },
              { name: 'Failed', color: viz.orange, values: col((p) => p.failed) },
            ]}
          />
        </ChartSection>
        <ChartSection
          title={`Runner hours per ${per}`}
          help="The time jobs held a runner, summed: what CI cost the pool."
          table={{
            columns: [h.bucket === 'week' ? 'Week of' : 'Day', 'Runner hours'],
            rows: rowsOf((p, i) => [labels[i], hours(p.busy)]),
          }}
        >
          <LineChart
            ariaLabel={`Runner hours per ${per}`}
            labels={labels}
            titles={titles}
            unit="hours"
            format={(v) => `${fmtNum(v)}h`}
            series={[
              {
                name: 'Runner hours',
                color: viz.blue,
                values: col((p) => hours(p.busy)),
                area: true,
              },
            ]}
          />
        </ChartSection>
        <ChartSection
          title="Full CI run"
          help="How long a green full run took, from the push that made it to its last job, at the median (p50) and the slow end (p90). Draft runs aren’t counted."
          legend={[
            { name: 'p50', color: viz.blue, kind: 'line' },
            { name: 'p90', color: viz.orange, kind: 'line' },
          ]}
          table={{
            columns: [h.bucket === 'week' ? 'Week of' : 'Day', 'Runs', 'p50', 'p90'],
            rows: rowsOf((p, i) => [
              labels[i],
              p.runs,
              cell(p.run_p50, duration),
              cell(p.run_p90, duration),
            ]),
          }}
        >
          <LineChart
            ariaLabel={`Full CI run time per ${per}, p50 and p90`}
            labels={labels}
            titles={titles}
            unit="minutes"
            format={fmtMinutes}
            series={[
              { name: 'p90', color: viz.orange, values: col((p) => minutes(p.run_p90)) },
              { name: 'p50', color: viz.blue, values: col((p) => minutes(p.run_p50)) },
            ]}
          />
        </ChartSection>
        <ChartSection
          title="Wait for a runner"
          help="How long a job that was ready waited before a runner took it. Waits that climb mean the pool is short for the load."
          legend={[
            { name: 'p50', color: viz.blue, kind: 'line' },
            { name: 'p90', color: viz.orange, kind: 'line' },
          ]}
          table={{
            columns: [h.bucket === 'week' ? 'Week of' : 'Day', 'p50', 'p90'],
            rows: rowsOf((p, i) => [
              labels[i],
              cell(p.wait_p50, duration),
              cell(p.wait_p90, duration),
            ]),
          }}
        >
          <LineChart
            ariaLabel={`Wait for a runner per ${per}, p50 and p90`}
            labels={labels}
            titles={titles}
            unit="minutes"
            format={fmtMinutes}
            series={[
              { name: 'p90', color: viz.orange, values: col((p) => minutes(p.wait_p90)) },
              { name: 'p50', color: viz.blue, values: col((p) => minutes(p.wait_p50)) },
            ]}
          />
        </ChartSection>
        <ChartSection
          title={`Merges per ${per}`}
          help="Pull requests merged in the repos you see."
          table={{
            columns: [h.bucket === 'week' ? 'Week of' : 'Day', 'Merges'],
            rows: rowsOf((p, i) => [labels[i], p.merges]),
          }}
        >
          <LineChart
            ariaLabel={`Merges per ${per}`}
            labels={labels}
            titles={titles}
            integer
            series={[{ name: 'Merges', color: viz.blue, values: col((p) => p.merges), area: true }]}
          />
        </ChartSection>
        <ChartSection
          title="In the merge line (p50)"
          help="The median time from a pull request’s turn (it was opened or babysat, or the one before it on the same base merged) to its merge."
          table={{
            columns: [h.bucket === 'week' ? 'Week of' : 'Day', 'Merges', 'p50'],
            rows: rowsOf((p, i) => [labels[i], p.merges, cell(p.merge_p50, duration)]),
          }}
        >
          <LineChart
            ariaLabel={`Median time per merge, per ${per}`}
            labels={labels}
            titles={titles}
            unit="minutes"
            format={fmtMinutes}
            series={[{ name: 'p50', color: viz.blue, values: col((p) => minutes(p.merge_p50)) }]}
          />
        </ChartSection>
      </div>
      <JobsSection h={h} />
    </>
  )
}

function ChartSection({
  title,
  help,
  legend,
  table,
  children,
}: {
  title: string
  help: ReactNode
  legend?: { name: string; color: string; kind?: 'line' | 'box' }[]
  table: { columns: string[]; rows: (string | number)[][] }
  children: ReactNode
}) {
  return (
    <Section title={title} help={help} helpId={`ci-history:${title}`}>
      <div className={clsx('px-4 py-3', vizClass)}>
        {legend && <Legend items={legend} className="mb-1" />}
        {children}
        <DataTable className="mt-2" columns={table.columns} rows={table.rows} />
      </div>
    </Section>
  )
}

/** "12% faster", "3m slower" against the week before; lower is better unless `higher`. */
function Change({
  now,
  before,
  fmt,
  better,
}: {
  now: number | null
  before: number | null
  fmt: (v: number) => string
  /** which way is good; none for counts */
  better?: 'lower' | 'higher'
}) {
  if (before === null) return <>no week before</>
  const was = <>was {fmt(before)}</>
  if (now === null || before === 0 || now === before) return was
  const pct = Math.round((100 * (now - before)) / before)
  if (pct === 0) return <>{was} · same</>
  const up = pct > 0
  const good = better === undefined ? null : (better === 'lower') !== up
  return (
    <>
      {was} ·{' '}
      <span className={clsx(good === true && 'text-ok', good === false && 'text-danger')}>
        {up ? '▲' : '▼'} {Math.abs(pct)}%
      </span>
    </>
  )
}

function Tile({ label, value, sub }: { label: string; value: ReactNode; sub: ReactNode }) {
  return (
    <div className="min-w-0 rounded-xl border border-line bg-panel px-4 py-3">
      <div className="text-xs text-muted">{label}</div>
      <div className="mt-0.5 truncate text-xl font-semibold tabular-nums tracking-tight">
        {value}
      </div>
      <div className="truncate text-xs text-subtle">{sub}</div>
    </div>
  )
}

/** The last seven days against the seven before. */
function Compare({ h }: { h: CIHistory }) {
  const n = h.now
  const b = h.before
  const had = b.jobs > 0
  const dur = (v: number | null) => (v === null ? '–' : duration(v))
  const pct = (v: number | null) => (v === null ? '–' : `${Math.round(v)}%`)
  const nul = (v: number | null) => (had ? v : null)
  return (
    <section aria-label="The last 7 days against the 7 before">
      <h2 className="mb-2 text-xs font-medium text-muted">Last 7 days, against the 7 before</h2>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-6">
        <Tile
          label="Jobs"
          value={n.jobs}
          sub={<Change now={n.jobs} before={nul(b.jobs)} fmt={String} />}
        />
        <Tile
          label="Failed"
          value={pct(failPct(n))}
          sub={
            <Change now={failPct(n)} before={nul(failPct(b))} fmt={(v) => pct(v)} better="lower" />
          }
        />
        <Tile
          label="Runner hours"
          value={fmtNum(hours(n.busy))}
          sub={<Change now={hours(n.busy)} before={nul(hours(b.busy))} fmt={(v) => fmtNum(v)} />}
        />
        <Tile
          label="Wait for a runner"
          value={dur(n.wait_p50)}
          sub={<Change now={n.wait_p50} before={nul(b.wait_p50)} fmt={duration} better="lower" />}
        />
        <Tile
          label="Full CI run"
          value={dur(n.run_p50)}
          sub={<Change now={n.run_p50} before={nul(b.run_p50)} fmt={duration} better="lower" />}
        />
        <Tile
          label="In the merge line (p50)"
          value={dur(n.merge_p50)}
          sub={
            <Change
              now={n.merge_p50}
              before={b.merges ? b.merge_p50 : null}
              fmt={duration}
              better="lower"
            />
          }
        />
        <Tile
          label="Opened to merged (p50)"
          value={dur(n.open_merge_p50)}
          sub={
            <Change
              now={n.open_merge_p50}
              before={b.merges ? b.open_merge_p50 : null}
              fmt={duration}
              better="lower"
            />
          }
        />
      </div>
    </section>
  )
}

function JobsSection({ h }: { h: CIHistory }) {
  if (!h.jobs.length) return null
  const many = h.repos.length > 1 && !h.repo
  return (
    <Section
      title="Jobs, last 7 days against the 7 before"
      help="Each job of a full run, most runner time first: its median (p50) green run time now and before, how often it ran and how often it failed."
      helpId="ci-history:jobs"
    >
      <div className="overflow-x-auto">
        <table className="w-full min-w-[34rem] text-[13px] tabular-nums">
          <thead className="text-xs text-muted">
            <tr className="border-b border-line">
              <th className="px-4 py-2 text-left font-medium">Job</th>
              <th className="px-2 py-2 text-right font-medium">p50</th>
              <th className="px-2 py-2 text-right font-medium">Before</th>
              <th className="px-2 py-2 text-right font-medium">Runs now / before</th>
              <th className="px-4 py-2 text-right font-medium">Failed now / before</th>
            </tr>
          </thead>
          <tbody>
            {h.jobs.map((j) => (
              <tr
                key={`${j.repo}|${j.workflow}|${j.name}`}
                className="border-b border-line last:border-0"
              >
                <td className="max-w-[16rem] truncate px-4 py-2">
                  {many && <span className="text-muted">{j.repo} · </span>}
                  {j.name}
                  <div className="truncate text-xs text-subtle">{j.workflow}</div>
                </td>
                <td className="px-2 py-2 text-right">
                  {j.p50 === null ? '–' : duration(j.p50)}
                  {j.p50 !== null && j.before_p50 !== null && j.before_p50 > 0 && (
                    <Delta pct={Math.round((100 * (j.p50 - j.before_p50)) / j.before_p50)} />
                  )}
                </td>
                <td className="px-2 py-2 text-right text-muted">
                  {j.before_p50 === null ? '–' : duration(j.before_p50)}
                </td>
                <td className="px-2 py-2 text-right">
                  {j.runs}
                  <span className="text-muted"> / {j.before_runs}</span>
                </td>
                <td className="px-4 py-2 text-right">
                  {j.failed}
                  <span className="text-muted"> / {j.before_failed}</span>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Section>
  )
}

function Delta({ pct }: { pct: number }) {
  if (Math.abs(pct) < 5) return null
  return (
    <span className={clsx('ml-1.5 text-xs', pct > 0 ? 'text-danger' : 'text-ok')}>
      {pct > 0 ? '▲' : '▼'}
      {Math.abs(pct)}%
    </span>
  )
}
