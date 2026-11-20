// Usage → Quota: the subscription's seven-day and five-hour windows, as the
// status line last reported them, and where they are heading at the rate so far.

import { CircleCheck, CircleDashed, TriangleAlert } from 'lucide-react'
import clsx from 'clsx'
import { LineChart, Legend, Sparkbars, viz } from '@/components/charts'
import { Section } from '@/components/ui/misc'
import { paceText, weekdayTime, type QuotaWindow } from '@/lib/usage-analytics'
import { relativeTime } from '@/lib/format'

const STEPS = 42 // points across the current window

export function QuotaSection({
  windows,
  statusAge,
}: {
  windows: QuotaWindow[]
  /** When any machine last reported the status line. */
  statusAge: string | null
}) {
  const week = windows.find((w) => w.span === 'seven_day')
  return (
    <Section
      title="Quota"
      helpId="usage:quota"
      help={
        <p>
          The subscription’s limits as Claude Code’s status line reports them (Pro and Max plans):
          the share of each window used, and when it resets. <b>Pace</b> carries the average rate so
          far to the end of the window. It is a projection, not a promise: a quiet weekend moves it.
          Nothing here calls Anthropic; <code>wp statusline</code> passes on what Claude Code
          already shows you.
        </p>
      }
      actions={
        statusAge && (
          <span className="text-xs text-subtle">Last report {relativeTime(statusAge)}</span>
        )
      }
    >
      <div className="grid gap-x-6 gap-y-4 p-4 md:grid-cols-2">
        {windows.map((w) => (
          <Gauge key={w.span} w={w} />
        ))}
      </div>
      {week && week.status !== 'reset' && <WeekChart w={week} />}
      {week && <PastWindows w={week} />}
    </Section>
  )
}

function Gauge({ w }: { w: QuotaWindow }) {
  const pace = paceText(w)
  const stale = w.status === 'reset'
  const used = Math.min(100, Math.max(0, w.percent))
  const Icon = pace.tone === 'bad' ? TriangleAlert : pace.tone === 'ok' ? CircleCheck : CircleDashed
  return (
    <div className="min-w-0">
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-[13px] font-medium">{w.label}</span>
        <span className="text-xs text-subtle">
          {stale
            ? 'reset'
            : `resets ${w.span === 'seven_day' ? weekdayTime(w.resets_at) : relativeTime(w.resets_at)}`}
        </span>
      </div>
      <div className="mt-1 text-2xl font-semibold tabular-nums tracking-tight">
        {stale ? '–' : `${Math.round(w.percent)}%`}
        <span className="ml-1.5 text-[13px] font-normal text-muted">used</span>
      </div>
      <div
        role="meter"
        aria-label={`${w.label} quota used`}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={stale ? undefined : Math.round(w.percent)}
        className="relative mt-2 h-2.5 rounded-full bg-sunken"
      >
        {!stale && (
          <div
            className={clsx('h-full rounded-full', pace.tone === 'bad' ? 'bg-danger' : 'bg-accent')}
            style={{ width: `${used}%` }}
          />
        )}
        {!stale && w.even > 0 && w.even < 100 && (
          <div
            aria-hidden
            title={`Even pace: ${Math.round(w.even)}%`}
            className="absolute -top-1 h-[18px] w-0.5 rounded bg-fg/60"
            style={{ left: `${w.even}%` }}
          />
        )}
      </div>
      <p
        className={clsx(
          'mt-2 flex items-start gap-1.5 text-[13px]',
          pace.tone === 'bad' ? 'text-danger' : pace.tone === 'ok' ? 'text-ok' : 'text-muted',
        )}
      >
        <Icon className="mt-0.5 size-3.5 shrink-0" />
        <span>{pace.text}</span>
      </p>
      {!stale && w.even > 0 && w.even < 100 && w.status !== 'too_early' && (
        <p className="mt-0.5 text-xs text-subtle">
          Spread evenly it would be {Math.round(w.even)}% by now (the tick).
        </p>
      )}
    </div>
  )
}

/** The current seven-day window: used so far, even pace, and the projection. */
function WeekChart({ w }: { w: QuotaWindow }) {
  const end = new Date(w.resets_at).getTime()
  const len = w.seconds * 1000
  const start = end - len
  const step = len / (STEPS - 1)
  const samples = w.history
    .filter((p) => new Date(p.resets_at).getTime() === end)
    .map((p) => ({ t: new Date(p.at).getTime(), v: p.percent }))
  if (samples.length === 0) return null
  const lastT = samples[samples.length - 1].t
  const used: (number | null)[] = []
  let j = -1
  for (let i = 0; i < STEPS; i++) {
    const t = start + i * step
    while (j + 1 < samples.length && samples[j + 1].t <= t) j++
    used.push(j >= 0 && t <= lastT + step ? samples[j].v : null)
  }
  // the last sample, on the step it falls in
  const k = Math.min(STEPS - 1, Math.max(0, Math.round((lastT - start) / step)))
  used[k] = samples[samples.length - 1].v
  for (let i = k + 1; i < STEPS; i++) used[i] = null
  const projected = used.map((_, i) =>
    i >= k && (w.status === 'on_track' || w.status === 'will_run_out')
      ? samples[samples.length - 1].v +
        ((w.projected - samples[samples.length - 1].v) * (i - k)) / Math.max(1, STEPS - 1 - k)
      : null,
  )
  const even = used.map((_, i) => (i / (STEPS - 1)) * 100)
  const titles = used.map((_, i) =>
    new Date(start + i * step).toLocaleString(undefined, {
      weekday: 'short',
      hour: '2-digit',
      minute: '2-digit',
    }),
  )
  // short axis labels: the chart shows as many as fit
  const labels = used.map((_, i) =>
    new Date(start + i * step).toLocaleString(undefined, { weekday: 'short', hour: 'numeric' }),
  )
  return (
    <div className="border-t border-line px-4 py-3">
      <div className="mb-2 flex flex-wrap items-center justify-between gap-2">
        <h4 className="text-xs font-medium text-muted">This week’s window</h4>
        <Legend
          items={[
            { name: 'Used', color: viz.blue, kind: 'line' },
            { name: 'Even pace', color: viz.neutral, kind: 'dash' },
            { name: 'At this rate', color: viz.orange, kind: 'dash' },
          ]}
        />
      </div>
      <LineChart
        ariaLabel="Seven-day quota used over the window, with even pace and projection"
        labels={labels}
        titles={titles}
        unit="percent"
        height={170}
        format={(v) => `${Math.round(v)}%`}
        series={[
          { name: 'Even pace', color: viz.neutral, values: even, dashed: true },
          { name: 'At this rate', color: viz.orange, values: projected, dashed: true },
          { name: 'Used', color: viz.blue, values: used, area: true },
        ]}
      />
    </div>
  )
}

/** The peak of each past seven-day window in the period. */
function PastWindows({ w }: { w: QuotaWindow }) {
  const byWindow = new Map<string, number>()
  for (const p of w.history)
    byWindow.set(p.resets_at, Math.max(byWindow.get(p.resets_at) ?? 0, p.percent))
  const past = [...byWindow.entries()]
    .filter(([end]) => new Date(end).getTime() < new Date(w.resets_at).getTime())
    .sort((a, b) => a[0].localeCompare(b[0]))
    .slice(-12)
  if (past.length < 2) return null
  return (
    <div className="border-t border-line px-4 pb-4 pt-3">
      <h4 className="mb-1 text-xs font-medium text-muted">Past weeks: most used</h4>
      <Sparkbars
        ariaLabel="Peak seven-day quota used in past windows"
        name="Peak used"
        values={past.map(([, v]) => v)}
        labels={past.map(([end]) => `Window ending ${weekdayTime(end)}`)}
        format={(v) => `${Math.round(v)}%`}
        height={48}
      />
    </div>
  )
}
