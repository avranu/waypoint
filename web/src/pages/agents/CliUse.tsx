import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import { SquareTerminal, X } from 'lucide-react'
import { DataTable, HBarList, Legend, LineChart, fmtNum, viz } from '@/components/charts'
import { Button } from '@/components/ui/button'
import { Select } from '@/components/ui/input'
import { Empty, Section, Skeleton } from '@/components/ui/misc'
import { shortDate } from '@/lib/format'
import {
  pct,
  usageCliQuery,
  type UsageCli,
  type UsageCliCommand,
  type UsageCliPeriod,
  type UsageCliRepo,
} from '@/lib/usage-analytics'
import type { HubTabProps } from './search'
import { SortTable, Tile, type Col } from './usage/shared'

const num = (v: number | null | undefined, digits = 1) =>
  v === null || v === undefined ? '–' : v.toFixed(digits)
const short = (head: string) => head.replace(/^wp /, '')

type Week = UsageCli['weeks'][number]

/** The table page X11 first drew by hand, one column a week. */
const MEASURES: { label: string; cell: (w: UsageCliPeriod) => string | number }[] = [
  { label: 'Sessions, subagents included', cell: (w) => w.sessions },
  { label: 'Bash calls per session', cell: (w) => num(w.bash_per_session, 0) },
  { label: 'wp calls per session', cell: (w) => num(w.wp_per_session) },
  { label: 'Help lookups per wp call', cell: (w) => pct(w.help_rate) },
  { label: 'Usage errors per wp call', cell: (w) => pct(w.error_rate, 1) },
  { label: 'Help per main session', cell: (w) => num(w.help_per_main, 2) },
  { label: 'curl to a forge API per session', cell: (w) => num(w.curl_per_session, 2) },
  { label: 'sleep per session', cell: (w) => num(w.sleep_per_session, 2) },
  { label: 'sleep beside wp pr or wp ci', cell: (w) => w.sleep_ci },
  { label: 'gh or tea calls', cell: (w) => w.gh_tea },
]

export function CliUseTab({ filters, setFilters }: HubTabProps) {
  const cq = useQuery(usageCliQuery(filters))
  const o = cq.data
  if (cq.error)
    return (
      <div className="rounded-xl border border-dashed border-line">
        <Empty icon={<SquareTerminal />} title="Couldn’t load wp use">
          {cq.error instanceof Error ? cq.error.message : String(cq.error)}
        </Empty>
      </div>
    )
  if (!o)
    return (
      <div className="space-y-4">
        <Skeleton className="h-20 rounded-xl" />
        <Skeleton className="h-72 rounded-xl" />
      </div>
    )
  const repoChip = filters.repo && (
    <Button size="sm" onClick={() => setFilters({ repo: undefined })}>
      {filters.repo}
      <X />
    </Button>
  )
  if (!o.has_data || (o.total.wp === 0 && o.total.sessions === 0))
    return (
      <div className="space-y-3">
        {repoChip}
        <div className="rounded-xl border border-dashed border-line">
          <Empty
            icon={<SquareTerminal />}
            title={o.has_data ? `No wp use in the last ${o.days} days` : 'No wp use counted yet'}
          >
            {o.has_data ? (
              'Try a longer period.'
            ) : (
              <>
                Each machine counts wp calls from its transcripts when it next syncs, including ones
                it synced before this view existed.
              </>
            )}
          </Empty>
        </div>
      </div>
    )
  const t = o.total
  return (
    <div className={clsx('space-y-4 transition-opacity', cq.isPlaceholderData && 'opacity-60')}>
      {repoChip}
      <dl className="grid grid-cols-2 gap-2 lg:grid-cols-4">
        <Tile label="Sessions" value={fmtNum(t.sessions, 0)} sub={`${t.main_sessions} main`} />
        <Tile
          label="wp calls per session"
          value={num(t.wp_per_session)}
          sub={`${fmtNum(t.wp, 0)} calls`}
        />
        <Tile label="Help lookups" value={pct(t.help_rate)} sub={`${t.help} of wp calls`} />
        <Tile
          label="Usage errors"
          value={pct(t.error_rate, 1)}
          sub={`${t.usage_errors} of wp calls`}
        />
      </dl>
      <ByWeek weeks={o.weeks} />
      <Commands commands={o.commands} />
      <Mistakes o={o} />
      {!filters.repo && o.repos.length > 1 && (
        <Repos repos={o.repos} pick={(repo) => setFilters({ repo })} />
      )}
    </div>
  )
}

const percent = (v: number | null) => (v === null ? null : v * 100)

function ByWeek({ weeks }: { weeks: Week[] }) {
  const labels = weeks.map((w) => shortDate(w.week))
  const seen = weeks.filter((w) => w.sessions > 0 || w.wp > 0)
  const rates = [
    {
      name: 'Help lookups',
      color: viz.blue,
      values: weeks.map((w) => percent(w.help_rate)),
    },
    {
      name: 'Usage errors',
      color: viz.orange,
      values: weeks.map((w) => percent(w.error_rate)),
    },
  ]
  const workarounds = [
    {
      name: 'sleep',
      color: viz.blue,
      values: weeks.map((w) => w.sleep_per_session),
    },
    {
      name: 'curl to a forge API',
      color: viz.orange,
      values: weeks.map((w) => w.curl_per_session),
    },
  ]
  return (
    <Section
      title="By week"
      helpId="cli-use:weeks"
      help={
        <p>
          Weeks start on Monday, in your time zone. A session is one transcript, so a subagent is a
          session of its own. A help lookup is a call with <code>-h</code>, <code>--help</code> or{' '}
          <code>wp help</code>; a usage error is wp refusing how it was called. A week still running
          counts only the days so far.
        </p>
      }
    >
      <div className="space-y-4 p-4">
        <div className="grid gap-4 lg:grid-cols-3">
          <figure className="min-w-0">
            <figcaption className="mb-1 text-xs text-muted">Share of wp calls</figcaption>
            <Legend items={rates.map((s) => ({ name: s.name, color: s.color }))} className="mb-1" />
            <LineChart
              ariaLabel="Help lookups and usage errors as a share of wp calls, by week"
              labels={labels}
              unit="% of wp calls"
              height={170}
              format={(v) => `${v.toFixed(1)}%`}
              series={rates}
            />
          </figure>
          <figure className="min-w-0">
            <figcaption className="mb-1 text-xs text-muted">wp calls per session</figcaption>
            <div className="mb-1 h-4" aria-hidden />
            <LineChart
              ariaLabel="wp calls per session, by week"
              labels={labels}
              unit="calls per session"
              height={170}
              format={(v) => v.toFixed(1)}
              series={[
                {
                  name: 'wp calls per session',
                  color: viz.blue,
                  values: weeks.map((w) => w.wp_per_session),
                },
              ]}
            />
          </figure>
          <figure className="min-w-0">
            <figcaption className="mb-1 text-xs text-muted">Workarounds per session</figcaption>
            <Legend
              items={workarounds.map((s) => ({ name: s.name, color: s.color }))}
              className="mb-1"
            />
            <LineChart
              ariaLabel="sleep and curl to a forge API per session, by week"
              labels={labels}
              unit="per session"
              height={170}
              format={(v) => v.toFixed(2)}
              series={workarounds}
            />
          </figure>
        </div>
        <div className="overflow-x-auto rounded-md border border-line">
          <table className="w-full text-xs tabular-nums">
            <caption className="sr-only">wp use by week</caption>
            <thead className="bg-sunken text-muted">
              <tr>
                <th
                  scope="col"
                  className="sticky left-0 bg-sunken px-2 py-1.5 text-left font-medium"
                >
                  Measure
                </th>
                {seen.map((w) => (
                  <th
                    key={w.week}
                    scope="col"
                    className="whitespace-nowrap px-2 py-1.5 text-right font-medium"
                  >
                    {shortDate(w.week)}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {MEASURES.map((m) => (
                <tr key={m.label} className="border-t border-line">
                  <th
                    scope="row"
                    className="sticky left-0 max-w-44 bg-panel px-2 py-1 text-left font-normal text-muted"
                  >
                    {m.label}
                  </th>
                  {seen.map((w) => (
                    <td key={w.week} className="px-2 py-1 text-right">
                      {m.cell(w)}
                    </td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>
    </Section>
  )
}

function Commands({ commands }: { commands: UsageCliCommand[] }) {
  const usable = commands.filter((c) => c.follows.length > 0)
  const [picked, setPicked] = useState('')
  const c =
    usable.find((x) => x.head === picked) ??
    usable.find((x) => x.head === 'wp next') ??
    usable[0] ??
    null
  const cols: Col<UsageCliCommand>[] = [
    { id: 'head', label: 'Command', value: (x) => x.head, asc: true },
    { id: 'calls', label: 'Calls', value: (x) => x.calls, bar: true },
    { id: 'help', label: 'Help', value: (x) => x.help },
    { id: 'errors', label: 'Errors', value: (x) => x.errors },
  ]
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Section title="Commands">
        <div className="py-2">
          <SortTable
            rows={commands}
            cols={cols}
            rowKey={(x) => x.head}
            sort="calls"
            caption="wp commands by calls"
            onRow={(x) => x.follows.length > 0 && setPicked(x.head)}
          />
        </div>
      </Section>
      <Section
        title="What follows"
        helpId="cli-use:follows"
        help={
          <p>
            Of the runs of a command (help lookups aside), the share where this was among the next
            five shell commands. One run can lead to several, so shares add up to more than 100%.
          </p>
        }
        actions={
          usable.length > 0 && (
            <Select
              aria-label="Command"
              value={c?.head ?? ''}
              onChange={(e) => setPicked(e.target.value)}
              className="!w-auto min-w-36"
            >
              {usable.map((x) => (
                <option key={x.head} value={x.head}>
                  {x.head}
                </option>
              ))}
            </Select>
          )
        }
      >
        <div className="space-y-3 p-4">
          {c ? (
            <>
              <HBarList
                ariaLabel={`What follows ${c.head}`}
                items={c.follows.map((f) => ({
                  key: f.name,
                  label: f.name,
                  value: f.share ?? 0,
                  display: pct(f.share),
                }))}
              />
              {c.flags.length > 0 && (
                <p className="text-xs text-subtle">
                  Flags used: {c.flags.map((f) => `${f.name} ${fmtNum(f.n, 0)}`).join(' · ')}
                </p>
              )}
              <DataTable
                columns={['Next', 'Runs', 'Share']}
                rows={c.follows.map((f) => [f.name, f.n, pct(f.share)])}
              />
            </>
          ) : (
            <p className="text-[13px] text-muted">Nothing counted after a command yet.</p>
          )}
        </div>
      </Section>
    </div>
  )
}

function Mistakes({ o }: { o: UsageCli }) {
  const lists = [
    {
      title: 'Flags that don’t exist',
      items: o.unknown_flags.map((f) => ({
        key: `${f.head} ${f.flag}`,
        prefix: short(f.head),
        label: f.flag,
        value: f.n,
        display: fmtNum(f.n, 0),
      })),
    },
    {
      title: 'Commands that don’t exist',
      items: o.unknown_commands.map((c) => ({
        key: c.name,
        label: c.name,
        value: c.n,
        display: fmtNum(c.n, 0),
      })),
    },
    {
      title: 'Usage errors',
      items: o.error_classes.map((c) => ({
        key: c.name,
        label: c.name.replaceAll('_', ' '),
        value: c.n,
        display: fmtNum(c.n, 0),
      })),
    },
  ].filter((l) => l.items.length > 0)
  if (lists.length === 0) return null
  return (
    <div className="grid gap-4 lg:grid-cols-3">
      {lists.map((l) => (
        <Section key={l.title} title={l.title}>
          <div className="p-4">
            <HBarList ariaLabel={l.title} items={l.items.slice(0, 12)} color={viz.orange} />
          </div>
        </Section>
      ))}
    </div>
  )
}

function Repos({ repos, pick }: { repos: UsageCliRepo[]; pick: (repo: string) => void }) {
  const cols: Col<UsageCliRepo>[] = [
    { id: 'repo', label: 'Repo', value: (x) => x.repo || '(no repo)', asc: true },
    { id: 'wp', label: 'wp calls', value: (x) => x.wp, bar: true },
    {
      id: 'per',
      label: 'Per session',
      value: (x) => x.wp_per_session ?? 0,
      cell: (x) => num(x.wp_per_session),
      wide: true,
    },
    { id: 'help', label: 'Help', value: (x) => x.help_rate ?? 0, cell: (x) => pct(x.help_rate) },
    {
      id: 'err',
      label: 'Errors',
      value: (x) => x.error_rate ?? 0,
      cell: (x) => pct(x.error_rate, 1),
    },
  ]
  return (
    <Section title="By repo">
      <div className="py-2">
        <SortTable
          rows={repos}
          cols={cols}
          rowKey={(x) => x.repo}
          sort="wp"
          caption="wp use by repository"
          onRow={(x) => x.repo && pick(x.repo)}
        />
      </div>
    </Section>
  )
}
