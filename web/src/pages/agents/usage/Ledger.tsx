// Usage → Token ledger (WP-35): the numbers every optimization is judged by.
// Context per call (and how often it passes 200k and 300k), forks against
// fresh subagents, cold cache starts, and what a closed task or a merged pull
// request cost, by model, beside the X4 baseline measured by hand.

import { useMemo, useState } from 'react'
import clsx from 'clsx'
import { useQuery } from '@tanstack/react-query'
import { LineChart, viz } from '@/components/charts'
import { Section } from '@/components/ui/misc'
import { usd } from '@/lib/activity'
import { shortDate } from '@/lib/format'
import { usageLedgerQuery } from '@/lib/queries'
import {
  LEDGER_BASELINE,
  pct,
  tok,
  type LedgerPrepGroup,
  type LedgerRow,
  type LedgerSpawn,
  type LedgerYield,
  type UsageFilters,
  type UsageLedger,
} from '@/lib/usage-analytics'
import { Segmented, SortTable, Tile, type Col } from './shared'

export function LedgerSection({ filters }: { filters: UsageFilters }) {
  const q = useQuery(usageLedgerQuery(filters))
  const l = q.data
  if (!l) return null
  if (l.overall.calls === 0 && l.unmeasured_messages === 0) return null
  return (
    <div
      className={clsx('space-y-4 transition-opacity', q.isPlaceholderData && 'opacity-60')}
      data-testid="token-ledger"
    >
      <Tiles l={l} />
      {l.unmeasured_messages > 0 && (
        <p className="rounded-lg border border-line bg-sunken px-3 py-2 text-xs text-muted">
          {l.unmeasured_messages.toLocaleString()} messages in this period were reported before the
          ledger existed. Run <code>wp usage sync</code> on each machine to measure them; it reads
          the transcripts again once and counts nothing twice.
        </p>
      )}
      {l.overall.calls > 0 && (
        <>
          <Trend l={l} />
          <Spawns spawns={l.spawns} />
          <Where l={l} />
          <div className="grid gap-4 lg:grid-cols-2">
            <Yield title="Closed tasks" noun="task" y={l.closed_tasks} helpId="ledger:closed">
              Everything the sessions that held each task used, whenever they used it. A task counts
              when it was closed in this period and a session was registered on it by the hooks.
            </Yield>
            <Yield title="Merged pull requests" noun="PR" y={l.merged_prs} helpId="ledger:merged">
              Everything the sessions on each pull request’s branch used (matched by repository and
              branch name), for pull requests merged in this period.
            </Yield>
          </div>
          <PrepCompare groups={l.prep ?? []} />
        </>
      )}
    </div>
  )
}

const base = LEDGER_BASELINE

function Tiles({ l }: { l: UsageLedger }) {
  const o = l.overall
  const sp = l.spawns[0]
  return (
    <dl className="grid grid-cols-2 gap-2 lg:grid-cols-5">
      <Tile
        label="Context per call"
        value={o.calls ? tok(o.avg_context) : '–'}
        sub={`X4: ${tok(base.avgContext)} · ${o.calls.toLocaleString()} calls`}
        title="Average input + cache read + cache write per assistant message. Every call re-reads this whole context, so it is what cache reads (and cost) scale with."
      />
      <Tile
        label="Calls over 300k"
        value={pct(o.share_300k)}
        sub={`X4: ${pct(base.share300k)} · over 200k: ${pct(o.share_200k)}`}
        title="Share of calls whose context was over 300k tokens"
      />
      <Tile
        label="Reads from calls over 300k"
        value={pct(o.read_share_300k)}
        sub={`X4: ${pct(base.readShare300k)} of cache reads`}
        title="Share of all cache-read tokens made by calls whose context was over 300k"
      />
      <Tile
        label="Cold cache starts (inferred)"
        value={o.cold_starts.toLocaleString()}
        sub={`${pct(o.cold_share, 1)} of calls · ${tok(o.cold_tokens)} rewritten · est. +${usd(o.cold_extra_est_usd)}`}
        title="Inferred, not measured: the CLI guessed that a call wrote to the cache after a gap longer than the cache lived (5 minutes, or an hour when the previous call wrote the 1 hour cache). The estimate is what those writes would cost beyond reading the same tokens from a warm cache, if the guess is right."
      />
      <Tile
        label="Forks / fresh"
        value={sp ? `${sp.forks} / ${sp.fresh}` : '–'}
        sub={
          sp
            ? `at spawn ${tok(sp.fork_context)} vs ${tok(sp.fresh_context)}`
            : 'no subagents in this period'
        }
        title="Subagents that began from their parent’s whole context (forks) against ones that began empty, and their average context at the first call"
      />
    </dl>
  )
}

function Trend({ l }: { l: UsageLedger }) {
  const labels = l.daily.map((d) => shortDate(d.date))
  const same = <T,>(v: T) => l.daily.map(() => v)
  const avg = l.daily.map((d) => d.avg_context)
  const share = l.daily.map((d) => (d.share_300k === null ? null : d.share_300k * 100))
  return (
    <Section
      title="Context per call, by day"
      helpId="ledger:trend"
      help={
        <p>
          The average context of the day’s calls, and the share of calls over 300k, against the
          numbers X4 measured on 2026-10-14 (dashed). Shorter contexts are the biggest lever on
          cache reads; whichever change is meant to shorten them should bend these lines.
        </p>
      }
    >
      <div className="grid gap-4 p-4 md:grid-cols-2">
        <LineChart
          ariaLabel="Average context per call by day"
          labels={labels}
          unit="tokens"
          height={180}
          format={tok}
          series={[
            { name: 'Average context', color: viz.blue, values: avg, area: true },
            { name: 'X4 baseline', color: viz.orange, values: same(base.avgContext), dashed: true },
          ]}
        />
        <LineChart
          ariaLabel="Share of calls over 300k by day"
          labels={labels}
          unit="% of calls"
          height={180}
          format={(v) => `${v.toFixed(0)}%`}
          series={[
            { name: 'Calls over 300k', color: viz.blue, values: share, area: true },
            {
              name: 'X4 baseline',
              color: viz.orange,
              values: same(base.share300k * 100),
              dashed: true,
            },
          ]}
        />
      </div>
    </Section>
  )
}

function Spawns({ spawns }: { spawns: LedgerSpawn[] }) {
  if (spawns.length === 0) return null
  const cols: Col<LedgerSpawn>[] = [
    {
      id: 'type',
      label: 'Subagent',
      asc: true,
      value: (r) => r.type.toLowerCase(),
      cell: (r) => <span className="font-medium">{r.type === 'all' ? 'All types' : r.type}</span>,
    },
    { id: 'forks', label: 'Forks', value: (r) => r.forks, bar: true },
    { id: 'fresh', label: 'Fresh', value: (r) => r.fresh, bar: true },
    {
      id: 'forkctx',
      label: 'Fork context',
      value: (r) => r.fork_context,
      cell: (r) => (r.forks ? tok(r.fork_context) : '–'),
      wide: true,
    },
    {
      id: 'freshctx',
      label: 'Fresh context',
      value: (r) => r.fresh_context,
      cell: (r) => (r.fresh ? tok(r.fresh_context) : '–'),
      wide: true,
    },
  ]
  return (
    <Section
      title="Forks and fresh subagents"
      helpId="ledger:spawns"
      help={
        <p>
          A <b>fork</b> inherits its parent’s whole conversation (Claude Code marks it with a{' '}
          <code>fork-context-ref</code> line in the subagent’s transcript); a <b>fresh</b> subagent
          starts from its prompt alone. <b>Context</b> is the size of the subagent’s first call, so
          what it carried from the moment it was spawned.
        </p>
      }
    >
      <SortTable
        caption="Subagents by how they began"
        rows={spawns}
        rowKey={(r) => r.type}
        sort="forks"
        cols={cols}
      />
    </Section>
  )
}

type By = 'task' | 'session' | 'machine'

function Where({ l }: { l: UsageLedger }) {
  const [by, setBy] = useState<By>('task')
  const rows = by === 'task' ? l.tasks : by === 'session' ? l.sessions : l.machines
  const cols = useMemo(() => ledgerCols(by), [by])
  return (
    <Section
      title="Per task, session and machine"
      helpId="ledger:where"
      actions={
        <Segmented
          label="Show by"
          value={by}
          onChange={setBy}
          options={[
            { id: 'task', label: 'Task' },
            { id: 'session', label: 'Session' },
            { id: 'machine', label: 'Machine' },
          ]}
        />
      }
      help={
        <p>
          Tasks are the issues sessions were registered on (by the hooks); a task’s row adds up all
          its sessions. Sorted by cost by default.
        </p>
      }
    >
      {rows.length === 0 ? (
        <p className="px-4 py-8 text-center text-[13px] text-muted">
          {by === 'task'
            ? 'No session in this period was registered on a task.'
            : 'Nothing in this period.'}
        </p>
      ) : (
        <SortTable
          caption={`Token ledger by ${by}`}
          rows={rows}
          rowKey={(r) => r.key}
          sort="cost"
          cols={cols}
        />
      )}
    </Section>
  )
}

function ledgerCols(by: By): Col<LedgerRow>[] {
  return [
    {
      id: 'name',
      label: { task: 'Task', session: 'Session', machine: 'Machine' }[by],
      asc: true,
      value: (r) => r.label.toLowerCase(),
      cell: (r) => (
        <div className="min-w-0">
          <div className="flex min-w-0 items-baseline gap-1.5">
            {by === 'task' && <span className="shrink-0 text-subtle">{r.key}</span>}
            <span className="truncate font-medium" title={r.label}>
              {r.label}
            </span>
          </div>
          {r.sub && <div className="truncate text-xs text-subtle">{r.sub}</div>}
        </div>
      ),
    },
    { id: 'calls', label: 'Calls', value: (r) => r.calls, cell: (r) => r.calls.toLocaleString() },
    {
      id: 'avg',
      label: 'Avg context',
      value: (r) => r.avg_context,
      cell: (r) => tok(r.avg_context),
      bar: true,
    },
    {
      id: 'o300',
      label: '>300k',
      value: (r) => r.share_300k ?? 0,
      cell: (r) => pct(r.share_300k),
      wide: true,
    },
    {
      id: 'cold',
      label: 'Cold (inferred)',
      value: (r) => r.cold_starts,
      cell: (r) => r.cold_starts.toLocaleString(),
      wide: true,
    },
    {
      id: 'cost',
      label: 'Cost',
      value: (r) => r.cost_usd,
      cell: (r) => usd(r.cost_usd),
      bar: true,
    },
  ]
}

function Yield({
  title,
  noun,
  y,
  helpId,
  children,
}: {
  title: string
  noun: string
  y: LedgerYield
  helpId: string
  children: React.ReactNode
}) {
  const per = y.measured || 1
  const t = y.totals
  const inputs = t.input + t.cache_write + t.cache_read
  const cols: Col<LedgerYield['models'][number]>[] = [
    { id: 'model', label: 'Model', asc: true, value: (r) => r.model, cell: (r) => r.model },
    { id: 'items', label: `${noun}s`, value: (r) => r.items },
    {
      id: 'fresh',
      label: 'Fresh / ' + noun,
      value: (r) => (r.input + r.output + r.cache_write) / Math.max(r.items, 1),
      cell: (r) => tok((r.input + r.output + r.cache_write) / Math.max(r.items, 1)),
      wide: true,
    },
    {
      id: 'cost',
      label: 'Cost / ' + noun,
      value: (r) => r.cost_usd / Math.max(r.items, 1),
      cell: (r) => usd(r.cost_usd / Math.max(r.items, 1)),
      bar: true,
    },
  ]
  return (
    <Section title={title} helpId={helpId} help={<p>{children}</p>}>
      {y.done === 0 ? (
        <p className="px-4 py-8 text-center text-[13px] text-muted">None in this period.</p>
      ) : (
        <div>
          <dl className="grid grid-cols-3 gap-2 p-4 pb-3">
            <Tile label="Done" value={y.done} sub={`${y.measured} with usage`} />
            <Tile
              label={`Cost / ${noun}`}
              value={y.measured ? usd(t.cost_usd / per) : '–'}
              sub="API-equivalent"
            />
            <Tile
              label={`Input / ${noun}`}
              value={y.measured ? tok(inputs / per) : '–'}
              sub={`${tok(t.output / per)} out`}
              title="Uncached input, cache writes and cache reads, per item with usage"
            />
          </dl>
          {y.models.length > 0 && (
            <SortTable
              caption={`Cost per ${noun} by model`}
              rows={y.models}
              rowKey={(r) => r.model}
              sort="cost"
              cols={cols}
            />
          )}
        </div>
      )}
    </Section>
  )
}

const PREP_LABELS: Record<LedgerPrepGroup['group'], string> = {
  shown: 'Prepared, shown',
  held_out: 'Prepared, held out',
  none: 'Not prepared',
}

/** Closed tasks by task prep (WP-212): does a free model's prep save tokens? */
function PrepCompare({ groups }: { groups: LedgerPrepGroup[] }) {
  if (!groups.some((g) => g.group !== 'none' && g.done > 0)) return null
  const per = (g: LedgerPrepGroup, n: number) => (g.measured ? n / g.measured : null)
  const cols: Col<LedgerPrepGroup>[] = [
    {
      id: 'group',
      label: 'Tasks',
      asc: true,
      value: (g) => ['none', 'held_out', 'shown'].indexOf(g.group), // shown first
      cell: (g) => <span className="font-medium">{PREP_LABELS[g.group]}</span>,
    },
    { id: 'done', label: 'Closed', value: (g) => g.done },
    {
      id: 'cost',
      label: 'Cost / task',
      value: (g) => per(g, g.totals.cost_usd) ?? -1,
      cell: (g) => {
        const v = per(g, g.totals.cost_usd)
        return v === null ? '–' : usd(v)
      },
      bar: true,
    },
    {
      id: 'input',
      label: 'Input / task',
      value: (g) => per(g, g.totals.input + g.totals.cache_write + g.totals.cache_read) ?? -1,
      cell: (g) => {
        const v = per(g, g.totals.input + g.totals.cache_write + g.totals.cache_read)
        return v === null ? '–' : tok(v)
      },
      wide: true,
    },
    {
      id: 'tools',
      label: 'Tool calls / task',
      value: (g) => per(g, g.tool_calls) ?? -1,
      cell: (g) => {
        const v = per(g, g.tool_calls)
        return v === null ? '–' : v.toFixed(0)
      },
    },
  ]
  return (
    <Section
      title="Task prep"
      helpId="ledger:prep"
      help={
        <p>
          Closed tasks by whether a free model prepared them before an agent took them (
          <code>wp prep</code>). One prepared task in five is held out: made, but never shown to the
          agent, so <b>held out</b> is the fair comparison for <b>shown</b>. Per task with usage;
          tool calls are every call the task’s sessions made.
        </p>
      }
    >
      <SortTable
        caption="Closed tasks by task prep"
        rows={groups}
        rowKey={(g) => g.group}
        sort="group"
        cols={cols}
      />
    </Section>
  )
}
