import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { useQuery } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  AlertTriangle,
  CheckCircle2,
  CircleDashed,
  CircleSlash,
  Clock,
  ExternalLink,
  Hourglass,
  Info,
  Loader2,
  Lock,
  Plus,
  Trash2,
  Workflow,
  XCircle,
} from 'lucide-react'
import { ciBoardQuery, useSetCIPool } from '@/lib/queries'
import { duration, plural, relativeTime, safeUrl } from '@/lib/format'
import { useTick } from '@/lib/hooks'
import {
  untilSec,
  gib,
  type CIAlert,
  type CIBoard,
  type CIHost,
  type CIJob,
  type CIPool,
  type CIPoolRunner,
  type CIRef,
  type CIRun,
} from '@/lib/ci'
import { DataTable, HBarList, LineChart, viz, vizClass } from '@/components/charts'
import { Button, IconButton } from '@/components/ui/button'
import { Input, Select } from '@/components/ui/input'
import { Badge, Empty, PageHeader, Progress, Section, Skeleton } from '@/components/ui/misc'
import { SegmentedControl } from '@/components/ui/segmented'
import { CITabs } from '@/components/ci-tabs'
import { toastError } from '@/components/ui/toast'

/** 14:05 */
function clockTime(iso: string | undefined): string {
  if (!iso) return '–'
  return new Date(iso).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })
}

/** The span the board's "24h" figures cover: "24h", or less when no whole day is kept. */
function daySpan(b: CIBoard): { label: string; whole: boolean } {
  const sec = (Date.parse(b.generated_at) - Date.parse(b.summary.since)) / 1000
  if (!(sec < 24 * 3600 - 120)) return { label: '24h', whole: true }
  return { label: duration(sec), whole: false }
}

/** "in 12m", "now" */
function inTime(sec: number): string {
  return sec <= 30 ? 'now' : 'in ' + duration(sec)
}

export function CIPage() {
  const query = useQuery(ciBoardQuery())
  useTick(5000)
  useEffect(() => {
    document.title = 'CI · Waypoint'
  }, [])
  const b = query.data
  const now = Date.now()
  return (
    <div className="flex min-h-0 flex-1 flex-col overflow-y-auto pb-16">
      <div className="mx-auto w-full max-w-6xl">
        <PageHeader
          icon={<Workflow className="size-5 text-accent" />}
          title="CI"
          subtitle={
            b ? (
              <span className="inline-flex items-center gap-1.5">
                {plural(b.repos.length, 'repo')} on Forgejo Actions · read{' '}
                {relativeTime(b.generated_at)}
                {query.isFetching && (
                  <Loader2 className="size-3 animate-spin" aria-label="Reading" />
                )}
              </span>
            ) : (
              'What the runners are doing'
            )
          }
          actions={<CITabs />}
        />
        <div className="space-y-4 px-4 pb-6 sm:px-6">
          {query.isPending ? (
            <div className="space-y-3">
              <Skeleton className="h-24" />
              <Skeleton className="h-64" />
            </div>
          ) : query.isError || !b ? (
            <Empty icon={<AlertTriangle />} title="Couldn't read CI">
              {String(query.error ?? '')}
            </Empty>
          ) : b.repos.length === 0 ? (
            <Empty icon={<Workflow />} title="No Forgejo repos attached">
              The CI board reads Forgejo Actions of the repos attached to your projects. Attach one
              in a project’s settings.
            </Empty>
          ) : (
            <Board b={b} now={now} />
          )}
        </div>
      </div>
    </div>
  )
}

function Board({ b, now }: { b: CIBoard; now: number }) {
  return (
    <>
      <Alerts alerts={b.alerts} />
      <Tiles b={b} now={now} />
      <Forecast b={b} now={now} />
      <RunsSection runs={b.runs} now={now} />
      <div className="grid gap-4 lg:grid-cols-2">
        <RunningSection jobs={b.running} now={now} />
        <QueueSection jobs={b.queued} now={now} />
      </div>
      <ActivitySection b={b} />
      <RecentSection b={b} />
      <StatsSection b={b} />
      <HostsSection hosts={b.hosts ?? []} now={now} />
      <div className="grid gap-4 lg:grid-cols-2">
        <ReposSection b={b} />
        {b.pools.map((p) => (
          <PoolSection key={p.forge_id} pool={p} canEdit={b.can_edit_pool} />
        ))}
      </div>
    </>
  )
}

const ALERT_LOOK: Record<CIAlert['level'], { icon: ReactNode; cls: string }> = {
  error: { icon: <XCircle />, cls: 'border-danger/40 bg-danger/5 [&_svg]:text-danger' },
  warn: { icon: <AlertTriangle />, cls: 'border-warn/40 bg-warn/5 [&_svg]:text-warn' },
  info: { icon: <Info />, cls: 'border-line bg-panel [&_svg]:text-subtle' },
}

function Alerts({ alerts }: { alerts: CIAlert[] }) {
  if (!alerts.length) return null
  const order = { error: 0, warn: 1, info: 2 }
  const sorted = [...alerts].sort((x, y) => order[x.level] - order[y.level])
  return (
    <ul className="space-y-2" aria-label="What looks wrong">
      {sorted.map((a, i) => (
        <li
          key={i}
          className={clsx('flex gap-2.5 rounded-lg border px-3 py-2', ALERT_LOOK[a.level].cls)}
        >
          <span className="mt-0.5 shrink-0 [&_svg]:size-4">{ALERT_LOOK[a.level].icon}</span>
          <div className="min-w-0 text-[13px]">
            <div className="font-medium">
              {a.title}
              {safeUrl(a.url) && (
                <a
                  href={safeUrl(a.url)}
                  target="_blank"
                  rel="noreferrer"
                  className="ml-2 inline-flex items-center gap-0.5 font-normal text-accent hover:underline"
                >
                  log <ExternalLink className="!size-3 !text-accent" />
                </a>
              )}
            </div>
            {a.body && <div className="text-muted">{a.body}</div>}
          </div>
        </li>
      ))}
    </ul>
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
  tone?: 'danger' | 'warn'
}) {
  return (
    <div className="min-w-0 rounded-xl border border-line bg-panel px-4 py-3">
      <div className="text-xs text-muted">{label}</div>
      <div
        className={clsx(
          'mt-0.5 truncate text-xl font-semibold tabular-nums tracking-tight',
          tone === 'danger' && 'text-danger',
          tone === 'warn' && 'text-warn',
        )}
      >
        {value}
      </div>
      {sub && <div className="truncate text-xs text-subtle">{sub}</div>}
      {children}
    </div>
  )
}

function Tiles({ b, now }: { b: CIBoard; now: number }) {
  const s = b.summary
  const busy = s.running + s.waiting + s.blocked > 0
  let done: ReactNode = 'Idle'
  let doneSub: ReactNode = 'nothing running or queued'
  let tone: 'danger' | undefined
  if (s.stuck > 0) {
    done = 'Never'
    doneSub = `${plural(s.stuck, 'job')} can't start`
    tone = 'danger'
  } else if (s.all_done_at) {
    done = (
      <>
        {clockTime(s.all_done_at)}{' '}
        <span className="text-sm font-normal text-muted">
          {inTime(untilSec(s.all_done_at, now))}
        </span>
      </>
    )
    doneSub = `if jobs run slow: ${clockTime(s.all_done_p90)} (${inTime(untilSec(s.all_done_p90, now))})`
  }
  const oldest = s.oldest_waiting ? -untilSec(s.oldest_waiting, now) : 0
  const failRate = s.finished_24h ? s.failed_24h / s.finished_24h : 0
  const day = daySpan(b)
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-5 [&>*]:min-w-0">
      <div className="col-span-2 md:col-span-1">
        <Tile label="Everything done" value={done} sub={doneSub} tone={tone} />
      </div>
      <Tile
        label="Running"
        value={
          <>
            {s.running}
            <span className="text-sm font-normal text-muted"> of {s.capacity} slots</span>
          </>
        }
      >
        <Progress
          value={s.running}
          max={s.capacity}
          className="mt-2"
          color={s.running >= s.capacity && busy ? 'var(--wp-warn)' : undefined}
        />
      </Tile>
      <Tile
        label="Queued"
        value={
          <>
            {s.waiting}
            {s.blocked > 0 && (
              <span className="text-sm font-normal text-muted"> + {s.blocked} blocked</span>
            )}
          </>
        }
        sub={s.waiting ? `oldest waiting ${duration(oldest)}` : 'none waiting for a runner'}
        tone={oldest > 3600 ? 'warn' : undefined}
      />
      <Tile
        label={`Wait for a runner · ${day.label}`}
        value={
          <>
            {duration(s.wait_p50)}
            <span className="text-sm font-normal text-muted"> typical</span>
          </>
        }
        sub={`p90 ${duration(s.wait_p90)} · jobs run ${duration(s.run_p50)} typical`}
      />
      <Tile
        label={`Finished · ${day.label}`}
        value={
          <>
            {s.finished_24h}
            {s.failed_24h > 0 && (
              <span
                className={clsx(
                  'text-sm font-normal',
                  failRate > 0.2 ? 'text-danger' : 'text-muted',
                )}
              >
                {' '}
                · {s.failed_24h} failed
              </span>
            )}
          </>
        }
        sub={
          day.whole
            ? `pool ${Math.round(s.utilization_24h * 100)}% busy · ${duration(s.busy_seconds_24h)} of runner time`
            : `pool ${Math.round(s.utilization_24h * 100)}% busy · only ${day.label} of jobs kept`
        }
      />
    </div>
  )
}

function what(r: CIRef): string {
  if (r.hidden) return 'Another project'
  return r.repo.split('/').pop() ?? r.repo
}

function RefLabel({ r, className }: { r: CIRef; className?: string }) {
  if (r.hidden)
    return (
      <span className={clsx('inline-flex items-center gap-1 text-subtle', className)}>
        <Lock className="size-3" /> another project
      </span>
    )
  return (
    <span className={clsx('min-w-0 truncate text-subtle', className)}>
      {r.repo} · {r.workflow.replace(/\.ya?ml$/, '')} #{r.run_number}
      {r.pr ? (
        <>
          {' · '}
          <a
            href={safeUrl(r.pr.url)}
            target="_blank"
            rel="noreferrer"
            className="hover:text-fg hover:underline"
            title={r.pr.title}
          >
            PR {r.pr.number}
          </a>
        </>
      ) : r.ref ? (
        ` · ${r.ref}`
      ) : null}
    </span>
  )
}

function JobName({ j }: { j: CIJob }) {
  const url = safeUrl(j.url)
  const label = (
    <span className="font-medium">
      {j.hidden ? '' : what(j) + ' / '}
      {j.name}
    </span>
  )
  return url ? (
    <a href={url} target="_blank" rel="noreferrer" className="hover:underline">
      {label}
    </a>
  ) : (
    label
  )
}

const FORECAST_ROWS = 30

function Forecast({ b, now }: { b: CIBoard; now: number }) {
  const [all, setAll] = useState(false)
  const rows = [...b.running, ...b.queued]
  if (!rows.length) return null
  const ends = rows.flatMap((j) =>
    [j.est_finish_p90, j.est_finish].filter(Boolean).map((t) => new Date(t!).getTime()),
  )
  const end = Math.max(now + 10 * 60_000, ...ends)
  const span = end - now
  const pct = (iso: string | undefined) =>
    iso ? Math.max(0, Math.min(100, ((new Date(iso).getTime() - now) / span) * 100)) : 0
  const step = [5, 10, 15, 30, 60, 120, 240, 480].find((m) => span / 60_000 / m <= 6) ?? 960
  const ticks: number[] = []
  const first = Math.ceil(now / (step * 60_000)) * step * 60_000
  for (let t = first; t <= end; t += step * 60_000) ticks.push(t)
  const shown = all ? rows : rows.slice(0, FORECAST_ROWS)
  const doneAt = b.summary.all_done_at
  return (
    <Section
      title="Forecast"
      help={
        <>
          Each job from now until it should finish: solid with its usual (median) run time, the
          paler tail if it runs as slow as its slowest tenth. Queued jobs start when the pool should
          have a free runner that offers their <code>runs-on</code> labels, oldest first, after the
          jobs they need. Run times come from the last day’s successful runs of the same job (else
          jobs of the same name, else the repo’s).
        </>
      }
      actions={
        doneAt && (
          <span className="text-xs text-muted">
            all done ~{clockTime(doneAt)}, slow case {clockTime(b.summary.all_done_p90)}
          </span>
        )
      }
    >
      <div className={clsx('px-4 py-3', vizClass)}>
        <div className="relative ml-0 h-5 text-[11px] text-subtle sm:ml-[34%]">
          {ticks.map((t) => (
            <span
              key={t}
              className="absolute -translate-x-1/2 tabular-nums"
              style={{ left: `${((t - now) / span) * 100}%` }}
            >
              {new Date(t).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' })}
            </span>
          ))}
        </div>
        <ul className="space-y-1.5">
          {shown.map((j) => {
            const startPct = j.status === 'running' ? 0 : pct(j.est_start)
            const endPct = pct(j.est_finish)
            const slowPct = pct(j.est_finish_p90)
            const running = j.status === 'running'
            const tip = running
              ? `${j.name}: running ${duration(j.duration ?? 0)}, usually ${duration(j.typical)}; done ~${clockTime(j.est_finish)} (slow ${clockTime(j.est_finish_p90)})`
              : j.no_runner
                ? `${j.name}: no runner offers ${j.runs_on?.join(', ')}`
                : `${j.name}: starts ~${clockTime(j.est_start)}, done ~${clockTime(j.est_finish)} (slow ${clockTime(j.est_finish_p90)})`
            return (
              <li key={`${j.forge_id}-${j.job_id}`} className="sm:flex sm:items-center" title={tip}>
                <div className="flex min-w-0 items-baseline gap-1.5 text-[12px] sm:w-[34%] sm:pr-3">
                  <span className="min-w-0 truncate">
                    <JobName j={j} />
                  </span>
                  <span className="shrink-0 text-subtle">
                    {running ? duration(j.duration ?? 0) : j.status === 'blocked' ? 'blocked' : ''}
                  </span>
                </div>
                <div className="relative h-4 flex-1 rounded bg-sunken">
                  {ticks.map((t) => (
                    <span
                      key={t}
                      className="absolute inset-y-0 w-px bg-line"
                      style={{ left: `${((t - now) / span) * 100}%` }}
                    />
                  ))}
                  {j.no_runner ? (
                    <span className="absolute inset-0 flex items-center px-2 text-[11px] text-danger">
                      never: no runner offers {j.runs_on?.join(', ')}
                    </span>
                  ) : (
                    <>
                      <span
                        className="absolute inset-y-0.5 rounded-sm opacity-35"
                        style={{
                          left: `${startPct}%`,
                          width: `max(${slowPct - startPct}%, 2px)`,
                          background: running ? viz.blue : viz.aqua,
                        }}
                      />
                      <span
                        className={clsx(
                          'absolute inset-y-0.5 rounded-sm',
                          j.overrun && 'outline outline-1 outline-danger',
                        )}
                        style={{
                          left: `${startPct}%`,
                          width: `max(${endPct - startPct}%, 2px)`,
                          background: running ? viz.blue : viz.aqua,
                        }}
                      />
                    </>
                  )}
                </div>
              </li>
            )
          })}
        </ul>
        <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-[11px] text-subtle">
          <span className="inline-flex items-center gap-1.5">
            <span className="size-2.5 rounded-sm" style={{ background: viz.blue }} /> running
          </span>
          <span className="inline-flex items-center gap-1.5">
            <span className="size-2.5 rounded-sm" style={{ background: viz.aqua }} /> queued
          </span>
          <span className="inline-flex items-center gap-1.5">
            <span className="size-2.5 rounded-sm opacity-35" style={{ background: viz.aqua }} /> if
            slow (p90)
          </span>
          {rows.length > FORECAST_ROWS && (
            <button className="ml-auto text-accent hover:underline" onClick={() => setAll(!all)}>
              {all ? 'Show fewer' : `Show all ${rows.length}`}
            </button>
          )}
        </div>
      </div>
    </Section>
  )
}

function RunsSection({ runs, now }: { runs: CIRun[]; now: number }) {
  if (!runs.length) return null
  return (
    <Section
      title={`Runs in progress · ${runs.length}`}
      help="Each unfinished workflow run, the one finishing last first: how many of its jobs are done, and when its last job should finish."
    >
      <ul className="divide-y divide-line">
        {runs.map((r) => (
          <li
            key={`${r.forge_id}-${r.run_id}`}
            className="flex flex-col gap-1.5 px-4 py-2.5 sm:flex-row sm:items-center sm:gap-4"
          >
            <div className="min-w-0 flex-1">
              <div className="truncate text-[13px] font-medium">
                {r.hidden ? (
                  'A run of another project'
                ) : safeUrl(r.url) ? (
                  <a
                    href={safeUrl(r.url)}
                    target="_blank"
                    rel="noreferrer"
                    className="hover:underline"
                  >
                    {r.title || r.workflow}
                  </a>
                ) : (
                  r.title || r.workflow
                )}
              </div>
              <RefLabel r={r} className="block text-xs" />
            </div>
            <div className="flex items-center gap-3 sm:w-[46%]">
              <div className="min-w-0 flex-1">
                <Progress
                  value={r.done}
                  max={r.jobs}
                  color={r.failed ? 'var(--wp-danger)' : undefined}
                />
                <div className="mt-1 text-[11px] text-subtle">
                  {r.done}/{r.jobs} done
                  {r.running > 0 && ` · ${r.running} running`}
                  {r.waiting > 0 && ` · ${r.waiting} queued`}
                  {r.failed > 0 && <span className="text-danger"> · {r.failed} failed</span>} ·
                  started {relativeTime(r.created)}
                </div>
              </div>
              <div className="w-28 shrink-0 text-right text-[13px] tabular-nums">
                {r.cant_finish ? (
                  <span className="text-danger">can't finish</span>
                ) : (
                  <>
                    <div>~{clockTime(r.est_finish)}</div>
                    <div className="text-[11px] text-subtle">
                      {inTime(untilSec(r.est_finish, now))}
                    </div>
                  </>
                )}
              </div>
            </div>
          </li>
        ))}
      </ul>
    </Section>
  )
}

function RunningSection({ jobs, now }: { jobs: CIJob[]; now: number }) {
  return (
    <Section
      title={`Running · ${jobs.length}`}
      help="How long each running job has run against how long it usually takes. Red: past its slow (p90) time; it may be stuck."
    >
      {!jobs.length ? (
        <Empty icon={<CircleDashed />} title="Nothing running" />
      ) : (
        <ul className="divide-y divide-line">
          {jobs.map((j) => {
            const ran = j.started ? -untilSec(j.started, now) : (j.duration ?? 0)
            return (
              <li key={`${j.forge_id}-${j.job_id}`} className="px-4 py-2.5">
                <div className="flex items-baseline justify-between gap-3 text-[13px]">
                  <span className="min-w-0 truncate">
                    <JobName j={j} />
                  </span>
                  <span className="shrink-0 tabular-nums">
                    <span className={clsx(j.overrun && 'font-medium text-danger')}>
                      {duration(ran)}
                    </span>
                    <span className="text-subtle"> / ~{duration(j.typical)}</span>
                  </span>
                </div>
                <Progress
                  value={ran}
                  max={Math.max(j.typical, 1)}
                  className="my-1.5"
                  color={
                    j.overrun ? 'var(--wp-danger)' : ran > j.typical ? 'var(--wp-warn)' : viz.blue
                  }
                />
                <div className="flex flex-col gap-0.5 text-xs sm:flex-row sm:items-baseline sm:justify-between sm:gap-3">
                  <RefLabel r={j} />
                  <span className="shrink-0 text-subtle">
                    done ~{clockTime(j.est_finish)}
                    {j.runs_on?.length ? ` · ${j.runs_on.join(', ')}` : ''}
                    {j.host ? ` · on ${j.host}` : ''}
                    {j.mem_peak ? ` · peak ${gib(j.mem_peak)}` : ''}
                  </span>
                </div>
              </li>
            )
          })}
        </ul>
      )}
    </Section>
  )
}

function QueueSection({ jobs, now }: { jobs: CIJob[]; now: number }) {
  return (
    <Section
      title={`Queue · ${jobs.length}`}
      help="In the order they should start. Waiting: ready, waiting for a runner that offers its labels. Blocked: waiting for other jobs of its run (needs)."
    >
      {!jobs.length ? (
        <Empty icon={<CheckCircle2 />} title="Nothing queued" />
      ) : (
        <ol className="divide-y divide-line">
          {jobs.map((j) => (
            <li key={`${j.forge_id}-${j.job_id}`} className="flex gap-3 px-4 py-2.5">
              <span className="w-5 shrink-0 text-right text-xs tabular-nums text-subtle">
                {j.position}
              </span>
              <div className="min-w-0 flex-1">
                <div className="flex items-baseline justify-between gap-3 text-[13px]">
                  <span className="min-w-0 truncate">
                    <JobName j={j} />
                  </span>
                  <span className="shrink-0 tabular-nums">
                    {j.no_runner ? (
                      <span className="text-danger">never</span>
                    ) : (
                      <>
                        ~{clockTime(j.est_start)}
                        <span className="text-subtle"> → {clockTime(j.est_finish)}</span>
                      </>
                    )}
                  </span>
                </div>
                <div className="flex flex-col gap-0.5 text-xs sm:flex-row sm:items-baseline sm:justify-between sm:gap-3">
                  <RefLabel r={j} />
                  <span className="shrink-0 text-subtle">
                    {j.status === 'blocked' ? (
                      <>needs {j.needs?.join(', ')}</>
                    ) : j.ready_at ? (
                      <>waiting {duration(-untilSec(j.ready_at, now))}</>
                    ) : (
                      'waiting'
                    )}
                    {j.runs_on?.length ? ` · ${j.runs_on.join(', ')}` : ''}
                  </span>
                </div>
              </div>
            </li>
          ))}
        </ol>
      )}
    </Section>
  )
}

function ActivitySection({ b }: { b: CIBoard }) {
  const tl = b.timeline
  if (!tl.length) return null
  const labels = tl.map((p) =>
    new Date(p.at).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }),
  )
  const round = (v: number) => Math.round(v * 10) / 10
  const day = daySpan(b)
  return (
    <Section
      title={day.whole ? 'Last 24 hours' : `Last ${day.label} (no whole day of jobs kept)`}
      help="Jobs running and jobs waiting for a runner, averaged over each 15 minutes, against the pool’s slots. Waiting that climbs while running sits at the slot line means the pool is too small for the load; waiting with little running means runners aren’t taking jobs."
    >
      <div className={clsx('px-4 py-3', vizClass)}>
        <LineChart
          ariaLabel={`Jobs running and waiting over the last ${day.whole ? '24 hours' : day.label}`}
          labels={labels}
          height={180}
          format={(v) => String(round(v))}
          series={[
            {
              name: 'Slots',
              color: viz.neutral,
              values: tl.map(() => b.summary.capacity),
              dashed: true,
            },
            { name: 'Waiting', color: viz.orange, values: tl.map((p) => round(p.waiting)) },
            {
              name: 'Running',
              color: viz.blue,
              values: tl.map((p) => round(p.running)),
              area: true,
            },
          ]}
        />
        <DataTable
          className="mt-2"
          columns={['Time', 'Running', 'Waiting']}
          rows={tl.map((p, i) => [labels[i], round(p.running), round(p.waiting)])}
        />
      </div>
    </Section>
  )
}

type RecentFilter = 'all' | 'failed' | 'slow'

const STATUS_ICON: Record<string, ReactNode> = {
  success: <CheckCircle2 className="size-3.5 shrink-0 text-ok" aria-label="success" />,
  failure: <XCircle className="size-3.5 shrink-0 text-danger" aria-label="failed" />,
  cancelled: <CircleSlash className="size-3.5 shrink-0 text-subtle" aria-label="cancelled" />,
  skipped: <CircleDashed className="size-3.5 shrink-0 text-subtle" aria-label="skipped" />,
}

function RecentSection({ b }: { b: CIBoard }) {
  const [filter, setFilter] = useState<RecentFilter>('all')
  const [repo, setRepo] = useState('')
  const [limit, setLimit] = useState(25)
  const typical = useMemo(() => {
    const m = new Map<string, number>()
    for (const s of b.stats) m.set(`${s.repo}|${s.workflow}|${s.name}`, s.p50)
    return m
  }, [b.stats])
  const repos = [...new Set(b.recent.filter((j) => !j.hidden).map((j) => j.repo))].sort()
  const list = b.recent.filter((j) => {
    if (repo && j.repo !== repo) return false
    const t = typical.get(`${j.repo}|${j.workflow}|${j.name}`) ?? 0
    if (filter === 'failed') return j.status === 'failure'
    if (filter === 'slow') return t > 0 && (j.duration ?? 0) > t * 1.5
    return true
  })
  return (
    <Section
      title="Recently finished"
      actions={
        <div className="flex items-center gap-2">
          {repos.length > 1 && (
            <Select
              aria-label="Repo"
              value={repo}
              onChange={(e) => setRepo(e.target.value)}
              className="hidden w-36 sm:block"
            >
              <option value="">All repos</option>
              {repos.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </Select>
          )}
          <SegmentedControl<RecentFilter>
            label="Show"
            value={filter}
            onChange={setFilter}
            textFrom="md"
            options={[
              { value: 'all', label: 'All', icon: <Clock /> },
              { value: 'failed', label: 'Failed', icon: <XCircle />, hint: 'only failures' },
              {
                value: 'slow',
                label: 'Slow',
                icon: <Hourglass />,
                hint: 'ran 1.5× longer than usual',
              },
            ]}
          />
        </div>
      }
    >
      {!list.length ? (
        <Empty icon={<CheckCircle2 />} title="Nothing here in the last day" />
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[620px] text-[13px]">
              <thead className="text-left text-xs text-muted">
                <tr className="border-b border-line">
                  <th className="w-1/2 px-4 py-2 font-medium">Job</th>
                  <th className="px-2 py-2 text-right font-medium">Waited</th>
                  <th className="px-2 py-2 text-right font-medium">Ran</th>
                  <th className="px-2 py-2 text-right font-medium">vs usual</th>
                  <th className="px-4 py-2 text-right font-medium">Finished</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line">
                {list.slice(0, limit).map((j, i) => {
                  const t = typical.get(`${j.repo}|${j.workflow}|${j.name}`) ?? 0
                  const d = j.duration ?? 0
                  const delta = t > 0 && j.status === 'success' ? (d - t) / t : null
                  return (
                    <tr key={i}>
                      <td className="max-w-0 px-4 py-2">
                        <div className="flex items-center gap-2">
                          {STATUS_ICON[j.status] ?? <CircleDashed className="size-3.5" />}
                          <span className="min-w-0 truncate">
                            <JobName j={j} />
                          </span>
                        </div>
                        <RefLabel r={j} className="block pl-5 text-xs" />
                      </td>
                      <td
                        className={clsx(
                          'px-2 py-2 text-right tabular-nums',
                          (j.wait ?? 0) > 1800 ? 'text-warn' : 'text-muted',
                        )}
                      >
                        {j.wait === undefined ? '–' : duration(j.wait)}
                      </td>
                      <td className="px-2 py-2 text-right tabular-nums">{duration(d)}</td>
                      <td
                        className={clsx(
                          'px-2 py-2 text-right tabular-nums',
                          delta !== null && delta > 0.5
                            ? 'text-warn'
                            : delta !== null && delta < -0.3
                              ? 'text-ok'
                              : 'text-subtle',
                        )}
                      >
                        {delta === null ? '' : `${delta > 0 ? '+' : ''}${Math.round(delta * 100)}%`}
                      </td>
                      <td className="whitespace-nowrap px-4 py-2 text-right text-muted">
                        {relativeTime(j.finished)}
                      </td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
          {list.length > limit && (
            <div className="border-t border-line px-4 py-2 text-center">
              <button
                className="text-xs text-accent hover:underline"
                onClick={() => setLimit(limit + 50)}
              >
                Show more ({list.length - limit} left)
              </button>
            </div>
          )}
        </>
      )}
    </Section>
  )
}

function StatsSection({ b }: { b: CIBoard }) {
  const [all, setAll] = useState(false)
  const day = daySpan(b)
  if (!b.stats.length) return null
  const rows = all ? b.stats : b.stats.slice(0, 15)
  return (
    <Section
      title={day.whole ? 'Jobs · last 24 hours' : `Jobs · last ${day.label}`}
      help="Each job of each workflow, the ones taking the most runner time first. Run times are of successful runs; a wide gap between p50 and p90 means a job’s time swings, which makes every ETA behind it shakier."
    >
      <div className="overflow-x-auto">
        <table className="w-full min-w-[680px] text-[13px] tabular-nums">
          <thead className="text-left text-xs text-muted">
            <tr className="border-b border-line">
              <th className="w-[32%] px-4 py-2 font-medium">Job</th>
              <th className="px-2 py-2 text-right font-medium">Runs</th>
              <th className="px-2 py-2 text-right font-medium">Passed</th>
              <th className="px-2 py-2 text-right font-medium">p50</th>
              <th className="px-2 py-2 text-right font-medium">p90</th>
              <th className="px-2 py-2 text-right font-medium">Longest</th>
              <th className="hidden px-2 py-2 text-right font-medium sm:table-cell">
                Peak mem (p95)
              </th>
              <th className="hidden px-2 py-2 text-right font-medium sm:table-cell">
                CPU cores (p50)
              </th>
              <th className="px-2 py-2 text-right font-medium">Waited</th>
              <th className="px-4 py-2 text-right font-medium">Runner time</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-line">
            {rows.map((s, i) => {
              const decided = s.success + s.failure
              const rate = decided ? s.success / decided : 1
              return (
                <tr key={i}>
                  <td className="max-w-0 px-4 py-2">
                    <div className="flex min-w-0 items-center gap-2">
                      {STATUS_ICON[s.last]}
                      <span className="truncate font-medium">
                        {s.hidden ? 'Another project' : `${s.repo.split('/').pop()} / ${s.name}`}
                      </span>
                    </div>
                    {!s.hidden && (
                      <div className="truncate pl-5 text-xs text-subtle">{s.workflow}</div>
                    )}
                  </td>
                  <td className="px-2 py-2 text-right">{s.runs}</td>
                  <td
                    className={clsx(
                      'px-2 py-2 text-right',
                      rate < 0.7 ? 'text-danger' : rate < 0.9 ? 'text-warn' : 'text-muted',
                    )}
                  >
                    {decided ? `${Math.round(rate * 100)}%` : '–'}
                  </td>
                  <td className="px-2 py-2 text-right">{s.p50 ? duration(s.p50) : '–'}</td>
                  <td className="px-2 py-2 text-right text-muted">
                    {s.p90 ? duration(s.p90) : '–'}
                  </td>
                  <td className="px-2 py-2 text-right text-muted">{duration(s.max)}</td>
                  <td className="hidden px-2 py-2 text-right text-muted sm:table-cell">
                    {s.mem_p95 ? gib(s.mem_p95) : '-'}
                  </td>
                  <td className="hidden px-2 py-2 text-right text-muted sm:table-cell">
                    {s.cores_p50 ? s.cores_p50.toFixed(1) : '-'}
                  </td>
                  <td className="px-2 py-2 text-right text-muted">{duration(s.wait_p50)}</td>
                  <td className="px-4 py-2 text-right">{duration(s.busy)}</td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      {b.stats.length > 15 && (
        <div className="border-t border-line px-4 py-2 text-center">
          <button className="text-xs text-accent hover:underline" onClick={() => setAll(!all)}>
            {all ? 'Show fewer' : `Show all ${b.stats.length}`}
          </button>
        </div>
      )}
    </Section>
  )
}

function ReposSection({ b }: { b: CIBoard }) {
  const day = daySpan(b)
  const total = b.repos.reduce((n, r) => n + r.busy, 0)
  const shown = b.repos.filter((r) => r.busy > 0 || r.running || r.queued || r.error)
  return (
    <Section
      title={`Runner time by repo · ${day.label}`}
      help="Who the pool worked for. A repo with a big share and many queued jobs is what the others wait behind."
    >
      <div className="px-4 py-3">
        {!shown.length ? (
          <div className="text-[13px] text-subtle">No runner time in the last day.</div>
        ) : (
          <HBarList
            ariaLabel="Runner time by repo"
            items={shown.map((r) => ({
              key: `${r.forge_id}-${r.repo}`,
              label: r.hidden ? 'Another project' : r.repo,
              value: r.busy,
              display:
                (total ? `${Math.round((r.busy / total) * 100)}% · ` : '') +
                duration(r.busy) +
                (r.running || r.queued ? ` · ${r.running} running, ${r.queued} queued` : '') +
                (r.error ? ' · unreadable' : ''),
            }))}
          />
        )}
      </div>
    </Section>
  )
}

function HostsSection({ hosts, now }: { hosts: CIHost[]; now: number }) {
  if (!hosts.length) return null
  return (
    <Section
      title={`Hosts · ${hosts.length}`}
      help="What each runner host last reported: slots in use, its CPUs and memory, the limit each job gets, free space for Docker, and load. Low disk or a silent host is flagged."
    >
      <ul className="divide-y divide-line">
        {hosts.map((h) => {
          const freePct = h.disk_total > 0 ? (h.disk_free / h.disk_total) * 100 : 100
          const bad = h.health === 'low_disk'
          const warn = freePct < 15
          return (
            <li key={`${h.forge_id}-${h.name}`} className="space-y-1.5 px-4 py-2.5 text-[13px]">
              <div className="flex flex-wrap items-center gap-x-3 gap-y-1">
                <span className="font-medium">{h.name}</span>
                <span className="tabular-nums text-muted">
                  {h.running}/{h.capacity} slots
                </span>
                {h.health !== 'ok' && (
                  <Badge color={bad ? 'var(--wp-danger)' : 'var(--wp-warn)'}>
                    {h.health === 'low_disk' ? 'Low disk' : 'Stale'}
                    {h.why ? ` · ${h.why}` : ''}
                  </Badge>
                )}
                <span className="flex min-w-0 flex-wrap gap-1">
                  {(h.labels ?? []).map((l) => (
                    <Badge key={l}>{l}</Badge>
                  ))}
                </span>
              </div>
              <div className="text-xs text-muted">
                {h.cpus} CPUs, {gib(h.memory)}
                {h.job_cpus > 0 || h.job_memory > 0
                  ? ` · each job ${h.job_cpus} CPUs, ${gib(h.job_memory)}`
                  : ''}
                {' · load ' + h.load1.toFixed(2)}
              </div>
              <div className="flex items-center gap-2 text-xs">
                <Progress
                  value={h.disk_total - h.disk_free}
                  max={h.disk_total}
                  className="w-28 shrink-0"
                  color={bad ? 'var(--wp-danger)' : warn ? 'var(--wp-warn)' : viz.blue}
                />
                <span className={clsx('tabular-nums', bad ? 'text-danger' : warn && 'text-warn')}>
                  {gib(h.disk_free)} free of {gib(h.disk_total)}
                </span>
                <span className="ml-auto shrink-0 text-subtle">
                  reported {duration(Math.max(0, -untilSec(h.reported_at, now)))} ago
                </span>
              </div>
            </li>
          )
        })}
      </ul>
    </Section>
  )
}

const SOURCE_TEXT: Record<CIPool['source'], string> = {
  configured: 'set by an admin',
  reported: 'from the hosts’ reports',
  runners: 'guessed: the forge’s runners, sharing the busiest moment’s slots evenly',
  observed: 'guessed: one runner as busy as the busiest moment of the last day',
}

function PoolSection({ pool, canEdit }: { pool: CIPool; canEdit: boolean }) {
  const [editing, setEditing] = useState(false)
  return (
    <Section
      title={`Runner pool · ${pool.forge_name}`}
      help="The runners the ETAs play the queue over: the labels each offers and how many jobs it runs at once. Forgejo doesn’t say a runner’s capacity, so set it here for good ETAs."
      actions={
        canEdit &&
        !editing && (
          <Button size="xs" variant="ghost" onClick={() => setEditing(true)}>
            Edit
          </Button>
        )
      }
    >
      <div className="space-y-3 px-4 py-3 text-[13px]">
        {editing ? (
          <PoolEditor pool={pool} onDone={() => setEditing(false)} />
        ) : (
          <>
            <div className="text-xs text-muted">
              {pool.capacity} slots, {SOURCE_TEXT[pool.source]}. At most {pool.peak_24h} ran at once
              in the last day.
            </div>
            <ul className="divide-y divide-line rounded-lg border border-line">
              {pool.runners.map((r, i) => {
                const live = pool.forge_runners?.find((f) => f.name === r.name)
                return (
                  <li key={i} className="flex items-center gap-3 px-3 py-2">
                    <span
                      className={clsx(
                        'size-2 shrink-0 rounded-full',
                        !live
                          ? 'bg-line-strong'
                          : live.status === 'offline'
                            ? 'bg-danger'
                            : live.status === 'active'
                              ? 'bg-ok'
                              : 'bg-subtle',
                      )}
                      role="img"
                      aria-label={live ? live.status : 'not known to the forge (or not visible)'}
                      title={live ? live.status : 'not known to the forge (or not visible)'}
                    />
                    <span className="font-medium">{r.name}</span>
                    <span className="flex min-w-0 flex-1 flex-wrap gap-1">
                      {(r.labels ?? []).length ? (
                        r.labels!.map((l) => <Badge key={l}>{l}</Badge>)
                      ) : (
                        <span className="text-xs text-subtle">any label</span>
                      )}
                    </span>
                    <span className="shrink-0 tabular-nums text-muted">×{r.capacity}</span>
                  </li>
                )
              })}
            </ul>
            {pool.forge_runners === null ? (
              <div className="text-xs text-subtle">
                The forge lists runners only to a site administrator’s token, so whether each is
                online isn’t known here.
              </div>
            ) : (
              pool.forge_runners
                .filter((f) => !pool.runners.some((r) => r.name === f.name))
                .map((f) => (
                  <div key={f.id} className="text-xs text-subtle">
                    The forge also has {f.name} ({f.status}, {(f.labels ?? []).join(', ')}), which
                    the pool above leaves out.
                  </div>
                ))
            )}
          </>
        )}
      </div>
    </Section>
  )
}

interface DraftRunner {
  name: string
  labels: string
  capacity: string
}

function PoolEditor({ pool, onDone }: { pool: CIPool; onDone: () => void }) {
  const seed: DraftRunner[] =
    pool.source === 'configured'
      ? pool.runners.map((r) => ({
          name: r.name,
          labels: (r.labels ?? []).join(', '),
          capacity: String(r.capacity),
        }))
      : (pool.forge_runners ?? []).map((f) => ({
          name: f.name,
          labels: (f.labels ?? []).join(', '),
          capacity: '',
        }))
  const [rows, setRows] = useState<DraftRunner[]>(
    seed.length ? seed : [{ name: '', labels: '', capacity: '' }],
  )
  const save = useSetCIPool()
  const set = (i: number, patch: Partial<DraftRunner>) =>
    setRows(rows.map((r, k) => (k === i ? { ...r, ...patch } : r)))
  const submit = (runners: CIPoolRunner[]) =>
    save.mutate({ forge: pool.forge_id, runners }, { onSuccess: onDone, onError: toastError })
  const parsed = rows
    .filter((r) => r.name.trim())
    .map((r) => ({
      name: r.name.trim(),
      labels: r.labels
        .split(',')
        .map((l) => l.trim())
        .filter(Boolean),
      capacity: Number(r.capacity),
    }))
  const valid =
    parsed.length > 0 && parsed.every((r) => Number.isInteger(r.capacity) && r.capacity >= 1)
  return (
    <form
      className="space-y-2"
      onSubmit={(e) => {
        e.preventDefault()
        if (valid) submit(parsed)
      }}
    >
      <div className="hidden grid-cols-[1fr_2fr_4.5rem_1.75rem] gap-2 text-xs text-muted sm:grid">
        <span>Runner</span>
        <span>Labels (comma separated)</span>
        <span>Capacity</span>
      </div>
      {rows.map((r, i) => (
        <div
          key={i}
          className="grid grid-cols-[1fr_4.5rem_1.75rem] gap-2 sm:grid-cols-[1fr_2fr_4.5rem_1.75rem]"
        >
          <Input
            aria-label="Runner name"
            placeholder="runner1"
            value={r.name}
            onChange={(e) => set(i, { name: e.target.value })}
          />
          <Input
            aria-label="Labels"
            placeholder="forgejo-ci, forgejo-ci-heavy"
            value={r.labels}
            onChange={(e) => set(i, { labels: e.target.value })}
            className="col-span-3 row-start-2 sm:col-span-1 sm:row-start-auto"
          />
          <Input
            aria-label="Capacity"
            inputMode="numeric"
            placeholder="2"
            value={r.capacity}
            onChange={(e) => set(i, { capacity: e.target.value })}
            className="col-start-2 row-start-1 sm:col-start-auto sm:row-start-auto"
          />
          <IconButton
            label="Remove runner"
            variant="ghost"
            size="sm"
            icon={<Trash2 />}
            onClick={() => setRows(rows.filter((_, k) => k !== i))}
            className="col-start-3 row-start-1 sm:col-start-auto sm:row-start-auto"
          />
        </div>
      ))}
      <div className="flex flex-wrap items-center gap-2 pt-1">
        <Button
          size="xs"
          variant="ghost"
          icon={<Plus className="size-3.5" />}
          onClick={() => setRows([...rows, { name: '', labels: '', capacity: '' }])}
        >
          Add runner
        </Button>
        <span className="flex-1" />
        {pool.source === 'configured' && (
          <Button size="xs" variant="ghost" onClick={() => submit([])} disabled={save.isPending}>
            Clear (guess again)
          </Button>
        )}
        <Button size="xs" onClick={onDone}>
          Cancel
        </Button>
        <Button
          size="xs"
          variant="primary"
          type="submit"
          disabled={!valid}
          loading={save.isPending}
        >
          Save
        </Button>
      </div>
    </form>
  )
}
