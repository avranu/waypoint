import { useMemo, useState } from 'react'
import { Link } from '@tanstack/react-router'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { ArrowDown, ArrowUp, ChartLine, GitCommitHorizontal, Minus, Tag } from 'lucide-react'
import {
  DataTable,
  Legend,
  LineChart,
  StackedAreaChart,
  fmtNum,
  viz,
  vizClass,
  type PlotMark,
} from '@/components/charts'
import { Input, Select } from '@/components/ui/input'
import { Empty, Section, Skeleton } from '@/components/ui/misc'
import { shortDate } from '@/lib/format'
import { usd } from '@/lib/activity'
import {
  fresh,
  pct,
  tok,
  usageCompareQuery,
  usageTrendsQuery,
  type CompareStat,
  type TrendAvg,
  type TrendSum,
  type UsageFactor,
  type UsageMark,
  type UsageTrendTask,
  type UsageTrends,
} from '@/lib/usage-analytics'
import type { HubFilters, HubTabProps } from './search'
import { Segmented, SortTable, type Col } from './usage/shared'

interface Measure {
  id: TrendSum | TrendAvg
  label: string
  avg?: boolean
  /** Adds up across repos. */
  byRepo?: boolean
  fmt: (v: number) => string
  unit: string
}

const count = (v: number) => fmtNum(v, 0)
const localToday = () => {
  const d = new Date()
  return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`
}

const MEASURES: Measure[] = [
  { id: 'cost', label: 'Cost', byRepo: true, fmt: usd, unit: 'USD at API list prices' },
  { id: 'tokens', label: 'Fresh tokens', byRepo: true, fmt: tok, unit: 'tokens' },
  { id: 'cache_read', label: 'Cache reads', byRepo: true, fmt: tok, unit: 'tokens' },
  { id: 'messages', label: 'Messages', byRepo: true, fmt: count, unit: 'messages' },
  { id: 'sessions', label: 'Sessions', byRepo: true, fmt: count, unit: 'sessions' },
  { id: 'tasks', label: 'Tasks worked on', fmt: count, unit: 'tasks' },
  { id: 'tool_calls', label: 'Tool calls', byRepo: true, fmt: count, unit: 'calls' },
  { id: 'tool_errors', label: 'Tool errors', byRepo: true, fmt: count, unit: 'errors' },
  { id: 'subagents', label: 'Subagents', byRepo: true, fmt: count, unit: 'subagents' },
  { id: 'avg_context', label: 'Average context', avg: true, fmt: tok, unit: 'tokens per call' },
  { id: 'over_200k', label: 'Calls over 200k context', avg: true, fmt: pct, unit: 'of calls' },
  { id: 'hit_rate', label: 'Cache hit rate', avg: true, fmt: pct, unit: 'of input' },
  { id: 'cost_per_session', label: 'Cost per session', avg: true, fmt: usd, unit: 'USD' },
]

/** Repo slots in the order the server ranks them; every other repo is neutral. */
const REPO_COLORS = [viz.blue, viz.orange, viz.aqua, viz.yellow]
const repoColor = (repo: string, i: number) =>
  repo === '*' ? viz.neutral : (REPO_COLORS[i] ?? viz.neutral)

function markLabel(m: UsageMark) {
  if (m.kind === 'cli') return m.count > 1 ? `${m.title} (${m.count} versions)` : m.title
  const more = m.count > 1 ? ` (+${m.count - 1} more)` : ''
  return `${m.repo}: ${m.title}${more}`
}

export function TrendsTab({ filters }: HubTabProps) {
  const tq = useQuery(usageTrendsQuery(filters))
  const o = tq.data
  const [pivot, setPivot] = useState<Pivot | null>(null)
  if (tq.error)
    return (
      <div className="rounded-xl border border-dashed border-line">
        <Empty icon={<ChartLine />} title="Couldn’t load trends">
          {tq.error instanceof Error ? tq.error.message : String(tq.error)}
        </Empty>
      </div>
    )
  if (!o)
    return (
      <div className="space-y-4">
        <Skeleton className="h-72 rounded-xl" />
        <Skeleton className="h-48 rounded-xl" />
      </div>
    )
  if (!o.has_data)
    return (
      <div className="rounded-xl border border-dashed border-line">
        <Empty icon={<ChartLine />} title={`No usage in the last ${filters.days} days`}>
          Trends are drawn from the usage `wp` uploads (see the Usage tab for how to start).
        </Empty>
      </div>
    )
  const pick = (p: Pivot) => {
    setPivot(p)
    document.getElementById('trends-compare')?.scrollIntoView({ behavior: 'smooth' })
  }
  return (
    <div className={clsx('space-y-4 transition-opacity', tq.isPlaceholderData && 'opacity-60')}>
      <OverTime o={o} pick={pick} />
      <CompareSection o={o} filters={filters} pivot={pivot} setPivot={setPivot} />
      <TasksSection o={o} />
      <FactorsSection factors={o.factors} />
    </div>
  )
}

function OverTime({ o, pick }: { o: UsageTrends; pick: (p: Pivot) => void }) {
  const [id, setId] = useState<Measure['id']>('cost')
  const [split, setSplit] = useState<'total' | 'repo'>('total')
  const m = MEASURES.find((x) => x.id === id) ?? MEASURES[0]
  const labels = o.dates.map((d) => shortDate(d))
  const marks: PlotMark[] = useMemo(
    () =>
      o.marks
        .map((mk) => ({ i: o.dates.indexOf(mk.date), label: markLabel(mk) }))
        .filter((x) => x.i >= 0),
    [o.marks, o.dates],
  )
  const byRepo = !!m.byRepo && split === 'repo' && o.repos.length > 1
  const values: (number | null)[] = m.avg
    ? (o.avgs[m.id as TrendAvg] ?? [])
    : (o.sums[m.id as TrendSum] ?? [])
  const repoSeries = byRepo
    ? o.repos.map((r, i) => ({
        name: r.label,
        color: repoColor(r.repo, i),
        values: r.series[m.id as TrendSum] ?? o.dates.map(() => 0),
      }))
    : []
  return (
    <Section
      title="Over time"
      helpId="trends:over-time"
      help={
        <p>
          One measure per day. Dashed rules mark days an agent instruction file changed on a repo’s
          default branch (CLAUDE.md, AGENTS.md, anything under .claude/) and days a new Claude Code
          version first ran. Averages leave a gap on days without data. Pick a change below to
          compare usage before and after it.
        </p>
      }
    >
      <div className="space-y-3 p-4">
        <div className="flex flex-wrap items-center gap-2">
          <Select
            aria-label="Measure"
            value={m.id}
            onChange={(e) => setId(e.target.value as Measure['id'])}
            className="!w-auto min-w-44"
          >
            <optgroup label="Per day">
              {MEASURES.filter((x) => !x.avg).map((x) => (
                <option key={x.id} value={x.id}>
                  {x.label}
                </option>
              ))}
            </optgroup>
            <optgroup label="Daily average">
              {MEASURES.filter((x) => x.avg).map((x) => (
                <option key={x.id} value={x.id}>
                  {x.label}
                </option>
              ))}
            </optgroup>
          </Select>
          {m.byRepo && o.repos.length > 1 && (
            <Segmented
              label="Split"
              value={split}
              onChange={setSplit}
              options={[
                { id: 'total', label: 'Total' },
                { id: 'repo', label: 'By repo' },
              ]}
            />
          )}
        </div>
        {byRepo && (
          <Legend
            items={repoSeries.map((s) => ({ name: s.name, color: s.color }))}
            className="pt-1"
          />
        )}
        {byRepo ? (
          <StackedAreaChart
            ariaLabel={`${m.label} per day by repository`}
            labels={labels}
            unit={m.unit}
            height={220}
            format={m.fmt}
            marks={marks}
            series={[...repoSeries].reverse()}
          />
        ) : (
          <LineChart
            ariaLabel={`${m.label} per day`}
            labels={labels}
            unit={m.unit}
            height={220}
            format={m.fmt}
            marks={marks}
            series={[{ name: m.label, color: viz.blue, values, area: !m.avg }]}
          />
        )}
        <DataTable
          columns={['Day', ...(byRepo ? repoSeries.map((s) => s.name) : [m.label])]}
          rows={o.dates.map((d, i) => [
            shortDate(d),
            ...(byRepo
              ? repoSeries.map((s) => m.fmt(s.values[i] ?? 0))
              : [values[i] === null || values[i] === undefined ? '–' : m.fmt(values[i]!)]),
          ])}
        />
        {o.marks.length > 0 && <MarkList marks={o.marks} pick={pick} />}
      </div>
    </Section>
  )
}

function MarkList({ marks, pick }: { marks: UsageMark[]; pick: (p: Pivot) => void }) {
  const [all, setAll] = useState(false)
  const list = [...marks].reverse()
  const shown = all ? list : list.slice(0, 6)
  return (
    <div className="border-t border-line pt-3">
      <h4 className="mb-1.5 text-xs font-medium text-muted">Changes in this period</h4>
      <ul className="space-y-1 text-[13px]">
        {shown.map((mk) => (
          <li key={`${mk.kind}:${mk.repo}:${mk.at}`} className="flex min-w-0 items-center gap-2">
            {mk.kind === 'cli' ? (
              <Tag aria-label="Claude Code version" className="size-3.5 shrink-0 text-subtle" />
            ) : (
              <GitCommitHorizontal
                aria-label="Instruction change"
                className="size-3.5 shrink-0 text-subtle"
              />
            )}
            <span className="w-14 shrink-0 tabular-nums text-subtle">{shortDate(mk.date)}</span>
            <span className="min-w-0 flex-1 truncate" title={mk.paths?.join(', ')}>
              {mk.repo && <span className="mr-1.5 text-subtle">{mk.repo}</span>}
              {mk.url ? (
                <a href={mk.url} target="_blank" rel="noreferrer" className="hover:underline">
                  {mk.title}
                </a>
              ) : (
                mk.title
              )}
              {mk.count > 1 && mk.kind === 'instructions' && (
                <span className="ml-1.5 text-subtle">+{mk.count - 1} more</span>
              )}
            </span>
            <button
              onClick={() =>
                pick({
                  at: mk.at,
                  label: markLabel(mk),
                  repo: mk.kind === 'instructions' ? (mk.repo ?? '') : '',
                })
              }
              className="shrink-0 text-xs text-accent hover:underline"
            >
              Compare
            </button>
          </li>
        ))}
      </ul>
      {list.length > 6 && (
        <button
          onClick={() => setAll(!all)}
          className="mt-1.5 text-xs text-muted hover:text-fg hover:underline"
        >
          {all ? 'Show fewer' : `Show all ${list.length}`}
        </button>
      )}
    </div>
  )
}

interface Pivot {
  at: string
  label: string
  repo: string
}

type CompareView = 'sessions' | 'tasks' | 'daily'

function CompareSection({
  o,
  filters,
  pivot,
  setPivot,
}: {
  o: UsageTrends
  filters: HubFilters
  pivot: Pivot | null
  setPivot: (p: Pivot | null) => void
}) {
  const [win, setWin] = useState<'7' | '14' | '30'>('7')
  const [view, setView] = useState<CompareView>('sessions')
  const [date, setDate] = useState('')
  const cq = useQuery(usageCompareQuery(filters, pivot?.at ?? '', Number(win), pivot?.repo ?? ''))
  const c = cq.data
  const choices = useMemo(() => {
    const out: { group: string; value: string; pivot: Pivot }[] = []
    for (const mk of [...o.marks].reverse())
      out.push({
        group: mk.kind === 'cli' ? 'Claude Code versions' : 'Instruction changes',
        value: `${mk.kind}|${mk.repo ?? ''}|${mk.at}`,
        pivot: {
          at: mk.at,
          label: markLabel(mk),
          repo: mk.kind === 'instructions' ? (mk.repo ?? '') : '',
        },
      })
    for (const pr of o.prs.slice(0, 40))
      out.push({
        group: 'Merged pull requests',
        value: `pr|${pr.repo}|${pr.number}`,
        pivot: { at: pr.merged_at, label: `${pr.repo}#${pr.number} ${pr.title}`, repo: pr.repo },
      })
    return out
  }, [o.marks, o.prs])
  const groups = [...new Set(choices.map((x) => x.group))]
  const selected = choices.find((x) => x.pivot.at === pivot?.at && x.pivot.label === pivot?.label)
  const value = selected ? selected.value : pivot ? 'date' : date ? 'date' : ''
  const repos = o.repos.filter((r) => r.repo !== '*' && r.repo !== '').map((r) => r.repo)
  if (pivot?.repo && !repos.includes(pivot.repo)) repos.push(pivot.repo)
  const stats = c ? c[view] : []
  return (
    <Section
      title="Before and after"
      helpId="trends:compare"
      className="scroll-mt-4"
      help={
        <p>
          Usage in the days before a moment against the days after it, compared by medians: per
          session, per task, or per whole day (the moment’s own day and today are left out). The
          last column says how likely a difference this size is by chance (a Mann–Whitney test);
          with few sessions on a side there is too little to say. Other things changed too, so read
          a difference as a lead, not a verdict.
        </p>
      }
    >
      <div id="trends-compare" className="space-y-3 p-4">
        <div className="flex flex-wrap items-center gap-2">
          <Select
            aria-label="Compare around"
            value={value}
            onChange={(e) => {
              const v = e.target.value
              if (v === 'date' || v === '') {
                setPivot(
                  v === 'date' && date
                    ? { at: new Date(date + 'T00:00:00').toISOString(), label: date, repo: '' }
                    : null,
                )
                if (v === 'date' && !date) setDate(localToday())
                return
              }
              const ch = choices.find((x) => x.value === v)
              if (ch) setPivot(ch.pivot)
            }}
            className="min-w-0 basis-full sm:!w-auto sm:max-w-md sm:flex-1 sm:basis-auto"
          >
            <option value="">Pick a moment…</option>
            {groups.map((g) => (
              <optgroup key={g} label={g}>
                {choices
                  .filter((x) => x.group === g)
                  .map((x) => (
                    <option key={x.value} value={x.value}>
                      {shortDate(x.pivot.at)} · {x.pivot.label}
                    </option>
                  ))}
              </optgroup>
            ))}
            <option value="date">A date…</option>
          </Select>
          {value === 'date' && (
            <Input
              type="date"
              aria-label="Date"
              value={date}
              max={localToday()}
              onChange={(e) => {
                setDate(e.target.value)
                if (e.target.value)
                  setPivot({
                    at: new Date(e.target.value + 'T00:00:00').toISOString(),
                    label: e.target.value,
                    repo: pivot?.repo ?? '',
                  })
              }}
              className="!w-auto"
            />
          )}
          <Segmented
            label="Days each side"
            value={win}
            onChange={setWin}
            options={[
              { id: '7', label: '7 days' },
              { id: '14', label: '14' },
              { id: '30', label: '30' },
            ]}
          />
          {pivot && repos.length > 0 && (
            <Select
              aria-label="Repository"
              value={pivot.repo}
              onChange={(e) => setPivot({ ...pivot, repo: e.target.value })}
              className="!w-auto min-w-0 max-w-56"
            >
              <option value="">All repos</option>
              {repos.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </Select>
          )}
        </div>
        {!pivot ? (
          <p className="py-4 text-center text-[13px] text-muted">
            Pick an instruction change, a Claude Code version, a merged pull request or a date.
          </p>
        ) : cq.error ? (
          <p className="py-4 text-center text-[13px] text-muted">
            {cq.error instanceof Error ? cq.error.message : String(cq.error)}
          </p>
        ) : !c ? (
          <Skeleton className="h-40 rounded-lg" />
        ) : (
          <div
            className={clsx('space-y-2 transition-opacity', cq.isPlaceholderData && 'opacity-60')}
          >
            <p className="text-xs text-muted">
              Before: {shortDate(c.before.from)} – {shortDate(c.before.to)} · After:{' '}
              {shortDate(c.after.from)} – {shortDate(c.after.to)}
              {c.after.days < c.window - 0.05 &&
                ` (only ${fmtNum(c.after.days)} day${c.after.days === 1 ? '' : 's'} so far)`}
            </p>
            <div className="flex">
              <Segmented
                label="Compare by"
                value={view}
                onChange={setView}
                options={[
                  { id: 'sessions', label: 'Per session' },
                  { id: 'tasks', label: 'Per task' },
                  { id: 'daily', label: 'Per day' },
                ]}
              />
            </div>
            <CompareTable stats={stats} minN={c.min_n} />
          </div>
        )}
      </div>
    </Section>
  )
}

function fmtUnit(unit: CompareStat['unit'], v: number) {
  switch (unit) {
    case 'usd':
      return usd(v)
    case 'tokens':
      return tok(v)
    case 'ratio':
      return pct(v)
    default:
      return fmtNum(v)
  }
}

function verdict(s: CompareStat, minN: number): { text: string; strong: boolean } {
  if (s.before.n < minN || s.after.n < minN) return { text: 'Too few to say', strong: false }
  if (s.p === null) return { text: 'No difference', strong: false }
  if (s.p < 0.05) return { text: 'Unlikely chance', strong: true }
  if (s.p < 0.2) return { text: 'Maybe chance', strong: false }
  return { text: 'Could be chance', strong: false }
}

function CompareTable({ stats, minN }: { stats: CompareStat[]; minN: number }) {
  const n = stats[0]
  return (
    <div className="overflow-x-auto">
      <table className="w-full text-[13px]">
        <caption className="sr-only">Medians before and after</caption>
        <thead>
          <tr className="border-b border-line text-xs text-muted">
            <th scope="col" className="px-2 py-1.5 text-left font-medium">
              Median
            </th>
            <th scope="col" className="px-2 py-1.5 text-right font-medium">
              Before{n ? ` (${n.before.n})` : ''}
            </th>
            <th scope="col" className="px-2 py-1.5 text-right font-medium">
              After{n ? ` (${n.after.n})` : ''}
            </th>
            <th scope="col" className="px-2 py-1.5 text-right font-medium">
              Change
            </th>
          </tr>
        </thead>
        <tbody>
          {stats.map((s) => {
            const v = verdict(s, minN)
            const up = s.change !== null && s.change > 0.005
            const down = s.change !== null && s.change < -0.005
            return (
              <tr key={s.key} className="border-b border-line last:border-0">
                <td className="px-2 py-1.5">{s.label}</td>
                <td className="px-2 py-1.5 text-right tabular-nums text-muted">
                  {s.before.n ? fmtUnit(s.unit, s.before.median) : '–'}
                </td>
                <td className="px-2 py-1.5 text-right tabular-nums">
                  {s.after.n ? fmtUnit(s.unit, s.after.median) : '–'}
                </td>
                <td className="px-2 py-1.5 text-right">
                  <div className="inline-flex items-center gap-1 tabular-nums">
                    {up ? (
                      <ArrowUp aria-label="up" className="size-3" />
                    ) : down ? (
                      <ArrowDown aria-label="down" className="size-3" />
                    ) : (
                      <Minus aria-hidden className="size-3 text-subtle" />
                    )}
                    {s.change === null
                      ? '–'
                      : !up && !down
                        ? '0%'
                        : `${up ? '+' : ''}${pct(s.change)}`}
                  </div>
                  <div
                    className={clsx(
                      'text-[11px]',
                      v.strong ? 'font-medium text-fg' : 'text-subtle',
                    )}
                    title={s.p === null ? undefined : `p = ${s.p.toFixed(3)}`}
                  >
                    {v.text}
                  </div>
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function TasksSection({ o }: { o: UsageTrends }) {
  const cols: Col<UsageTrendTask>[] = [
    {
      id: 'task',
      label: 'Task',
      asc: true,
      value: (t) => t.key,
      cell: (t) => (
        <Link
          to="/issue/$key"
          params={{ key: t.key }}
          className="block truncate hover:underline"
          title={`${t.key} ${t.title}`}
        >
          <span className="mr-1.5 text-subtle">{t.key}</span>
          {t.title}
        </Link>
      ),
    },
    {
      id: 'cost',
      label: 'Cost',
      value: (t) => t.cost_usd,
      cell: (t) => usd(t.cost_usd),
      bar: true,
    },
    { id: 'tokens', label: 'Tokens', value: (t) => fresh(t), cell: (t) => tok(fresh(t)) },
    { id: 'tools', label: 'Tool calls', value: (t) => t.tool_calls, wide: true },
    { id: 'sessions', label: 'Sessions', value: (t) => t.sessions, wide: true },
    { id: 'subagents', label: 'Subagents', value: (t) => t.subagents, wide: true },
    { id: 'hours', label: 'Hours', value: (t) => t.active_hours, wide: true },
    {
      id: 'context',
      label: 'Avg context',
      value: (t) => t.avg_context ?? -1,
      cell: (t) => (t.avg_context === null ? '–' : tok(t.avg_context)),
      wide: true,
    },
    {
      id: 'lines',
      label: 'Lines',
      value: (t) => (t.prs ? t.lines : -1),
      cell: (t) => (t.prs ? fmtNum(t.lines) : '–'),
      wide: true,
    },
  ]
  return (
    <Section
      title="Tasks"
      helpId="trends:tasks"
      help={
        <p>
          What each task used in this period. An hour of a session counts for the task its agent was
          running then (from two hours before its first run to an hour after a run ended), or for
          the task the session held. Lines are added plus deleted in the task’s merged pull
          requests.
        </p>
      }
    >
      {o.tasks.length === 0 ? (
        <p className="p-6 text-center text-[13px] text-muted">
          No session in this period worked on a task.
        </p>
      ) : (
        <div className="space-y-2 pb-3">
          <SortTable rows={o.tasks} cols={cols} rowKey={(t) => t.key} sort="cost" caption="Tasks" />
          <p className="px-4 text-xs text-subtle">
            {o.task_rows > o.tasks.length && `The ${o.tasks.length} costliest of ${o.task_rows}. `}
            {o.no_task.cost_usd > 0 || fresh(o.no_task) > 0
              ? `Not tied to a task: ${usd(o.no_task.cost_usd)} (${tok(fresh(o.no_task))} tokens).`
              : 'All of it was tied to a task.'}
          </p>
        </div>
      )}
    </Section>
  )
}

function strength(r: number) {
  const a = Math.abs(r)
  return a >= 0.7 ? 'strong' : a >= 0.4 ? 'moderate' : a >= 0.2 ? 'weak' : 'none'
}

function FactorsSection({ factors }: { factors: UsageFactor[] }) {
  const ranked = factors.filter((f) => f.rho !== null)
  const short = factors.filter((f) => f.rho === null)
  return (
    <Section
      title="What goes with cost"
      helpId="trends:factors"
      help={
        <p>
          For tasks in this period, how closely each trait ranks with the task’s cost (Spearman’s
          rank correlation, from −1 to 1). Near 1: tasks with more of it cost more; near −1: they
          cost less. It shows what goes together, not what causes what. A trait needs eight tasks
          with a cost to be ranked.
        </p>
      }
    >
      <div className={clsx('space-y-2 p-4', vizClass)}>
        {ranked.length === 0 && (
          <p className="text-[13px] text-muted">
            Too few tasks with a cost in this period to rank anything.
          </p>
        )}
        <ul className="space-y-2" aria-label="Traits by how closely they rank with cost">
          {ranked.map((f) => {
            const r = f.rho!
            const w = Math.abs(r) * 50
            return (
              <li key={f.key} className="flex items-center gap-3 text-[13px]">
                <span className="min-w-0 flex-1 truncate">{f.label}</span>
                <span
                  aria-hidden
                  className="relative h-2 w-24 shrink-0 rounded-full bg-sunken sm:w-40"
                >
                  <span className="absolute inset-y-0 left-1/2 w-px bg-line-strong" />
                  <span
                    className="absolute inset-y-0 rounded-full"
                    style={{
                      left: r >= 0 ? '50%' : `${50 - w}%`,
                      width: `max(${w}%, 2px)`,
                      background: r >= 0 ? viz.blue : viz.orange,
                    }}
                  />
                </span>
                <span className="w-12 shrink-0 text-right tabular-nums">
                  {r > 0 ? '+' : ''}
                  {r.toFixed(2)}
                </span>
                <span className="hidden w-36 shrink-0 text-xs text-subtle sm:block">
                  {strength(r) === 'none'
                    ? 'no link'
                    : `${strength(r)}: more → ${r > 0 ? 'costlier' : 'cheaper'}`}{' '}
                  · {f.n}
                </span>
              </li>
            )
          })}
        </ul>
        {short.length > 0 && ranked.length > 0 && (
          <p className="text-xs text-subtle">
            Not ranked:{' '}
            {short
              .map((f) => (f.enough ? `${f.label} (all the same)` : `${f.label} (${f.n} tasks)`))
              .join(', ')}
            .
          </p>
        )}
      </div>
    </Section>
  )
}
