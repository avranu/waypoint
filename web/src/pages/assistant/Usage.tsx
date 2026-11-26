import { useMemo, useState } from 'react'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { BarChart3, Moon } from 'lucide-react'
import { hermesQuery, iso, type Assistant } from '@/lib/assistant'
import { dateTime, shortDate } from '@/lib/format'
import { tok } from '@/lib/usage-analytics'
import { BarChart, DataTable, HBarList, Legend, viz, type BarSeries } from '@/components/charts'
import { Empty, Section, Skeleton } from '@/components/ui/misc'
import { Segmented, SortTable, Tile, type Col } from '@/pages/agents/usage/shared'
import {
  ago,
  totalsFromDaily,
  type UsageTotals,
  money,
  n,
  periodDays,
  taskLabel,
  type ModelRow,
  type ModelsReport,
  type UsageReport,
  type UsageTask,
} from './usage/data'

type Period = '7' | '30' | '90'
const PERIOD_KEY = 'wp:assistant-usage-days'

function savedPeriod(): Period {
  try {
    const v = localStorage.getItem(PERIOD_KEY)
    if (v === '7' || v === '30' || v === '90') return v
  } catch {}
  return '30'
}

export function UsageTab({ assistant }: { assistant: Assistant }) {
  const [period, setPeriodState] = useState<Period>(savedPeriod)
  const setPeriod = (p: Period) => {
    setPeriodState(p)
    try {
      localStorage.setItem(PERIOD_KEY, p)
    } catch {}
  }
  const days = Number(period)
  const up = assistant.state.reachable && !assistant.state.frozen
  const usage = useQuery({
    ...hermesQuery<UsageReport>(assistant.id, 'analytics/usage', { days }, 60_000),
    enabled: up,
    placeholderData: keepPreviousData,
    retry: false,
  })
  const models = useQuery({
    ...hermesQuery<ModelsReport>(assistant.id, 'analytics/models', { days }, 60_000),
    enabled: up,
    placeholderData: keepPreviousData,
    retry: false,
  })

  const head = (
    <div className="flex items-center justify-between gap-2">
      <h2 className="text-[13px] font-semibold">Last {days} days</h2>
      <Segmented
        label="Period"
        value={period}
        onChange={setPeriod}
        options={[
          { id: '7', label: '7 days' },
          { id: '30', label: '30 days' },
          { id: '90', label: '90 days' },
        ]}
      />
    </div>
  )

  if (!up)
    return (
      <div className="space-y-4">
        {head}
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<Moon />} title={`${assistant.name} isn’t answering`}>
            Its usage is kept by Hermes itself, so it shows when it’s back.
          </Empty>
        </div>
      </div>
    )
  if (usage.isError)
    return (
      <div className="space-y-4">
        {head}
        <div className="rounded-xl border border-danger/40 bg-danger/5 px-4 py-3 text-[13px] text-danger">
          Couldn’t load usage:{' '}
          {usage.error instanceof Error ? usage.error.message : String(usage.error)}
        </div>
      </div>
    )
  if (!usage.data)
    return (
      <div className="space-y-4">
        {head}
        <div className="grid grid-cols-2 gap-2 lg:grid-cols-4">
          {[0, 1, 2, 3].map((i) => (
            <Skeleton key={i} className="h-[74px] rounded-xl" />
          ))}
        </div>
        <Skeleton className="h-72 rounded-xl" />
        <Skeleton className="h-40 rounded-xl" />
      </div>
    )

  const u = usage.data
  const t = u.totals ?? totalsFromDaily(u.daily)
  const tokensIn = n(t?.total_input)
  const tokensOut = n(t?.total_output)
  const cacheRead = n(t?.total_cache_read)
  const empty =
    n(t?.total_sessions) === 0 &&
    n(t?.total_api_calls) === 0 &&
    tokensIn + tokensOut === 0 &&
    !(u.daily ?? []).length &&
    !(u.tools ?? []).length
  return (
    <div
      className={clsx(
        'space-y-4 transition-opacity',
        (usage.isPlaceholderData || models.isPlaceholderData) && 'opacity-60',
      )}
    >
      {head}
      {empty ? (
        <div className="rounded-xl border border-dashed border-line">
          <Empty icon={<BarChart3 />} title={`Nothing used in the last ${days} days`}>
            Conversations and scheduled runs show their tokens and cost here.
          </Empty>
        </div>
      ) : (
        <>
          <dl className="grid grid-cols-2 gap-2 lg:grid-cols-4">
            <Tile label="Conversations" value={tok(n(t?.total_sessions))} />
            <Tile
              label="Model calls"
              value={tok(n(t?.total_api_calls))}
              sub={
                n(t?.total_sessions) > 0
                  ? `${(n(t?.total_api_calls) / n(t?.total_sessions)).toFixed(1)} per conversation`
                  : undefined
              }
            />
            <Tile
              label="Tokens"
              value={tok(tokensIn + tokensOut)}
              sub={`${tok(tokensIn)} in · ${tok(tokensOut)} out${cacheRead ? ` · ${tok(cacheRead)} cached` : ''}`}
              title={
                n(t?.total_reasoning)
                  ? `${tok(n(t?.total_reasoning))} of the output was reasoning`
                  : undefined
              }
            />
            <CostTile t={t} />
          </dl>
          <DailySection u={u} days={days} />
          <ModelsSection u={u} models={models.data} />
          <div className="grid gap-4 lg:grid-cols-2">
            <ToolsSection u={u} />
            <SkillsSection u={u} />
          </div>
          <TasksSection tasks={u.by_task ?? []} />
        </>
      )}
    </div>
  )
}

function CostTile({ t }: { t: UsageTotals }) {
  const est = n(t.total_estimated_cost)
  const actual = n(t.total_actual_cost)
  const tokens = n(t.total_input) + n(t.total_output)
  return (
    <Tile
      label="Estimated cost"
      value={money(est)}
      sub={
        actual > 0
          ? `${money(actual)} billed`
          : est === 0 && tokens > 0
            ? 'no price for these models'
            : 'at list prices'
      }
      title="Hermes's estimate from the models' list prices; the provider's bill is what counts."
    />
  )
}

function DailySection({ u, days }: { u: UsageReport; days: number }) {
  const data = useMemo(() => {
    const dates = periodDays(days)
    const byDay = new Map((u.daily ?? []).map((d) => [d.day, d]))
    const pick = (f: (d: NonNullable<UsageReport['daily']>[number]) => number | null) =>
      dates.map((d) => {
        const row = byDay.get(d)
        return row ? n(f(row)) : 0
      })
    return {
      dates,
      input: pick((d) => d.input_tokens),
      output: pick((d) => d.output_tokens),
      cache: pick((d) => d.cache_read_tokens),
      cost: pick((d) => d.estimated_cost),
      sessions: pick((d) => d.sessions),
      calls: pick((d) => d.api_calls),
    }
  }, [u.daily, days])
  const labels = data.dates.map((d) => shortDate(d))
  const hasCache = data.cache.some((v) => v > 0)
  const hasCost = data.cost.some((v) => v > 0)
  const cents = Math.max(0, ...data.cost) < 1
  const series: BarSeries[] = [
    { name: 'Input', color: viz.blue, values: data.input },
    { name: 'Output', color: viz.orange, values: data.output },
    ...(hasCache ? [{ name: 'Read from cache', color: viz.aqua, values: data.cache }] : []),
  ]
  const sum = (a: number[]) => a.reduce((x, y) => x + y, 0)
  return (
    <Section title="Tokens by day">
      <div className="space-y-4 p-4">
        <Legend
          items={series.map((s) => ({ name: s.name, color: s.color, value: tok(sum(s.values)) }))}
        />
        <BarChart
          ariaLabel="Tokens per day: input, output and read from cache"
          labels={labels}
          unit="tokens"
          height={200}
          format={tok}
          series={series}
        />
        {hasCost && (
          <div className="border-t border-line pt-4">
            <div className="mb-2 text-[13px] font-medium">Estimated cost by day</div>
            <BarChart
              ariaLabel="Estimated cost per day"
              labels={labels}
              unit={cents ? 'US cents' : 'US dollars'}
              height={140}
              format={(v) => money(cents ? v / 100 : v)}
              series={[
                {
                  name: 'Estimated cost',
                  color: viz.yellow,
                  values: cents ? data.cost.map((v) => v * 100) : data.cost,
                },
              ]}
            />
          </div>
        )}
        <DataTable
          columns={['Day', 'Conversations', 'Model calls', 'Input', 'Output', 'Cache read', 'Cost']}
          rows={data.dates
            .map((d, i) => [
              shortDate(d),
              data.sessions[i],
              data.calls[i],
              tok(data.input[i]),
              tok(data.output[i]),
              tok(data.cache[i]),
              money(data.cost[i]),
            ])
            .reverse()}
        />
      </div>
    </Section>
  )
}

function ModelsSection({ u, models }: { u: UsageReport; models?: ModelsReport }) {
  const rows: ModelRow[] =
    models?.models ??
    (u.by_model ?? []).map((m) => ({
      ...m,
      provider: '',
      cache_read_tokens: null,
      reasoning_tokens: null,
      actual_cost: null,
      tool_calls: null,
      last_used_at: null,
      avg_tokens_per_session: null,
    }))
  if (!rows.length) return null
  const cols: Col<ModelRow>[] = [
    {
      id: 'model',
      label: 'Model',
      asc: true,
      value: (r) => r.model.toLowerCase(),
      cell: (r) => (
        <div className="min-w-0">
          <div className="truncate font-medium" title={r.model}>
            {r.model}
          </div>
          {r.provider && <div className="truncate text-xs text-subtle">{r.provider}</div>}
        </div>
      ),
    },
    {
      id: 'tokens',
      label: 'Tokens',
      value: (r) => n(r.input_tokens) + n(r.output_tokens),
      bar: true,
      cell: (r) => tok(n(r.input_tokens) + n(r.output_tokens)),
    },
    { id: 'calls', label: 'Calls', value: (r) => n(r.api_calls), cell: (r) => tok(n(r.api_calls)) },
    {
      id: 'sessions',
      label: 'Conversations',
      value: (r) => n(r.sessions),
      wide: true,
      cell: (r) => tok(n(r.sessions)),
    },
    {
      id: 'cache',
      label: 'Cached',
      value: (r) => n(r.cache_read_tokens),
      wide: true,
      cell: (r) => (r.cache_read_tokens ? tok(r.cache_read_tokens) : '–'),
    },
    {
      id: 'cost',
      label: 'Cost',
      value: (r) => n(r.estimated_cost),
      cell: (r) => money(r.estimated_cost),
    },
    {
      id: 'last',
      label: 'Last used',
      value: (r) => n(r.last_used_at),
      wide: true,
      cell: (r) => (
        <span
          className="whitespace-nowrap"
          title={r.last_used_at ? dateTime(iso(r.last_used_at)!) : undefined}
        >
          {ago(r.last_used_at)}
        </span>
      ),
    },
  ]
  return (
    <Section title="Models">
      <div className="py-1">
        <SortTable
          rows={rows}
          cols={cols}
          rowKey={(r) => `${r.provider}:${r.model}`}
          sort="tokens"
          caption="Usage by model"
        />
      </div>
    </Section>
  )
}

function ToolsSection({ u }: { u: UsageReport }) {
  const tools = (u.tools ?? []).filter((t) => n(t.count) > 0).slice(0, 10)
  return (
    <Section title="Top tools">
      <div className="p-4">
        {tools.length === 0 ? (
          <p className="py-4 text-center text-[13px] text-muted">No tool calls in this period.</p>
        ) : (
          <HBarList
            ariaLabel="Tool calls by tool"
            items={tools.map((t) => ({
              key: t.tool,
              label: t.tool,
              value: n(t.count),
              display: `${tok(n(t.count))}${t.percentage !== null && t.percentage !== undefined ? ` · ${Math.round(t.percentage)}%` : ''}`,
            }))}
          />
        )}
      </div>
    </Section>
  )
}

function SkillsSection({ u }: { u: UsageReport }) {
  const s = u.skills?.summary
  const top = (u.skills?.top_skills ?? []).filter((k) => n(k.total_count) > 0).slice(0, 10)
  return (
    <Section
      title="Top skills"
      actions={
        s && n(s.distinct_skills_used) > 0 ? (
          <span className="text-xs text-subtle">
            {tok(n(s.total_skill_loads))} loads · {tok(n(s.total_skill_edits))} edits
          </span>
        ) : undefined
      }
    >
      <div className="p-4">
        {top.length === 0 ? (
          <p className="py-4 text-center text-[13px] text-muted">No skills used in this period.</p>
        ) : (
          <HBarList
            ariaLabel="Skill use by skill"
            color={viz.aqua}
            items={top.map((k) => ({
              key: k.skill,
              label: k.skill,
              value: n(k.total_count),
              display:
                n(k.manage_count) > 0
                  ? `${n(k.view_count)} loads · ${n(k.manage_count)} edits`
                  : `${n(k.view_count)} loads`,
            }))}
          />
        )}
      </div>
    </Section>
  )
}

function TasksSection({ tasks }: { tasks: UsageTask[] }) {
  if (!tasks.length) return null
  const cols: Col<UsageTask>[] = [
    {
      id: 'task',
      label: 'Task',
      asc: true,
      value: (r) => taskLabel(r.task),
      cell: (r) => (
        <div className="min-w-0">
          <div className="truncate font-medium">{taskLabel(r.task)}</div>
          {!!r.models?.length && (
            <div className="truncate text-xs text-subtle" title={r.models.join(', ')}>
              {r.models.join(', ')}
            </div>
          )}
        </div>
      ),
    },
    {
      id: 'tokens',
      label: 'Tokens',
      value: (r) => n(r.input_tokens) + n(r.output_tokens),
      bar: true,
      cell: (r) => tok(n(r.input_tokens) + n(r.output_tokens)),
    },
    { id: 'calls', label: 'Calls', value: (r) => n(r.api_calls), cell: (r) => tok(n(r.api_calls)) },
    {
      id: 'cost',
      label: 'Cost',
      value: (r) => n(r.estimated_cost),
      cell: (r) => money(r.estimated_cost),
    },
  ]
  return (
    <Section
      title="Side tasks"
      helpId="assistant:usage-tasks"
      help={
        <p>
          Model calls Hermes makes besides the conversation itself: naming conversations,
          compressing long ones, reading images and the like. The models table counts them; the
          totals above don’t.
        </p>
      }
    >
      <div className="py-1">
        <SortTable
          rows={tasks}
          cols={cols}
          rowKey={(r) => r.task}
          sort="tokens"
          caption="Side tasks"
        />
      </div>
    </Section>
  )
}
