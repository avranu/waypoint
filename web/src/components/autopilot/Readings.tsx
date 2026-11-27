import type { ReactNode } from 'react'
import clsx from 'clsx'
import { Clock, Gauge, Plane, Wallet } from 'lucide-react'
import { useSetAutopilot } from '@/lib/queries'
import type { AutopilotLimits, AutopilotMeter } from '@/lib/autopilot'
import { relativeTime } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/misc'
import { MenuList, Popover, usePopover, type MenuItem } from '@/components/ui/popover'
import { toastError } from '@/components/ui/toast'
import { ChevronDown } from 'lucide-react'

const usd = (n: number) => `$${n >= 100 ? n.toFixed(0) : n.toFixed(2)}`
const pct = (n: number) => `${Math.round(n)}%`
const clamp01 = (n: number) => Math.min(1, Math.max(0, n))

interface BarMark {
  /** 0..1 along the bar */
  at: number
  tip: string
  tone: 'pace' | 'limit'
}

/**
 * A bar from 0 to its maximum. `max` is written at the right end (a tooltip
 * says what it is); marks sit on the bar with a tooltip each.
 */
function MeterBar({
  fraction,
  color,
  marks = [],
  maxLabel,
  maxTip,
  label,
}: {
  fraction: number
  color: string
  marks?: BarMark[]
  maxLabel?: string
  maxTip?: string
  label: string
}) {
  return (
    <div className="mt-2 flex items-center gap-2">
      <div className="relative min-w-0 flex-1 py-1">
        <div
          role="progressbar"
          aria-label={label}
          aria-valuemin={0}
          aria-valuemax={100}
          aria-valuenow={Math.round(clamp01(fraction) * 100)}
          className="h-2 overflow-hidden rounded-full bg-sunken"
        >
          <div
            className="h-full rounded-full transition-[width]"
            style={{ width: `${clamp01(fraction) * 100}%`, background: color }}
          />
        </div>
        {marks.map((m) => (
          <span
            key={m.tone + m.at}
            title={m.tip}
            aria-label={m.tip}
            className={clsx(
              'absolute top-0 h-4 w-2 -translate-x-1/2 cursor-help',
              'after:absolute after:left-1/2 after:top-0 after:h-4 after:-translate-x-1/2 after:rounded-full',
              m.tone === 'pace' ? 'after:w-0.5 after:bg-fg/70' : 'after:w-[3px] after:bg-warn',
            )}
            style={{ left: `${clamp01(m.at) * 100}%` }}
          />
        ))}
      </div>
      {maxLabel && (
        <span
          title={maxTip}
          className="shrink-0 cursor-help text-xs font-medium tabular-nums text-muted"
        >
          {maxLabel}
        </span>
      )}
    </div>
  )
}

function Card({
  icon,
  title,
  tip,
  aside,
  children,
}: {
  icon: ReactNode
  title: string
  tip: string
  aside?: ReactNode
  children: ReactNode
}) {
  return (
    <div className="rounded-xl border border-line bg-panel px-3.5 py-3">
      <div className="flex items-center justify-between gap-2">
        <div
          title={tip}
          className="flex min-w-0 cursor-help items-center gap-1.5 text-xs text-muted [&_svg]:size-3.5"
        >
          {icon}
          <span className="truncate">{title}</span>
        </div>
        {aside}
      </div>
      {children}
    </div>
  )
}

const Value = ({ children }: { children: ReactNode }) => (
  <span className="text-xl font-semibold tabular-nums">{children}</span>
)

export function Readings({ meter, limits }: { meter: AutopilotMeter; limits: AutopilotLimits }) {
  const hourly = limits.max_hourly_usd ?? 0
  const quotaOff = !!meter.quota_off_until
  const weekCap = quotaOff ? 0 : (meter.week_cap ?? limits.week_max ?? 0)
  const fiveCap = quotaOff ? 0 : (meter.five_cap ?? limits.five_hour_max ?? 0)
  const weekPct = meter.week_percent
  const even = meter.week_even
  const aheadLimit = quotaOff ? 0 : (limits.week_ahead ?? 0)
  const ahead = weekPct !== undefined && even !== undefined ? weekPct - even : undefined
  const paceLine = even !== undefined && aheadLimit > 0 ? even + aheadLimit : 0
  const bites = [paceLine, weekCap].filter((n) => n > 0 && n < 100)
  const pauseAt = bites.length ? Math.min(...bites) : 0
  const warn = 'var(--wp-warn)'
  // The ceiling holds from its value on, the pace line only past it.
  const over =
    weekPct !== undefined &&
    ((weekCap > 0 && weekPct >= weekCap) || (paceLine > 0 && weekPct > paceLine))
  const spendOver = hourly > 0 && meter.hourly_usd >= hourly

  const weekPause = [
    paceLine > 0 &&
      paceLine < 100 &&
      `${aheadLimit} points ahead of an even pace (${pct(paceLine)} now)`,
    weekCap > 0 && `${pct(weekCap)} outright`,
  ].filter(Boolean)

  const weekMarks: BarMark[] = []
  if (even !== undefined)
    weekMarks.push({
      at: even / 100,
      tone: 'pace',
      tip: `Even pace ${pct(even)}${ahead === undefined ? '' : `: ${Math.abs(Math.round(ahead)) < 1 ? 'right on it' : `${Math.abs(Math.round(ahead))} points ${ahead > 0 ? 'ahead' : 'behind'}`}`}`,
    })
  if (pauseAt > 0 && pauseAt < 100)
    weekMarks.push({
      at: pauseAt / 100,
      tone: 'limit',
      tip: `Pauses at ${pct(pauseAt)}: ${weekPause.join(', or ')}, whichever comes first`,
    })

  const fiveShown = meter.five_hour_percent !== undefined
  const fiveNone =
    'No five-hour reading: Claude Code’s status line posts one while a session runs, and none has come since the window last reset'
  return (
    <section aria-label="Readings" className="space-y-2">
      <PauseBanner meter={meter} />
      <div className="grid gap-2 sm:grid-cols-2 xl:grid-cols-3">
        <Card
          icon={<Wallet />}
          title="Last hour"
          tip="Spend at list price, all your sessions, over the last 60 minutes"
        >
          <div className="mt-0.5 flex items-baseline gap-2">
            <Value>{usd(meter.hourly_usd)}</Value>
            {hourly <= 0 && <Badge title="No hourly limit set">no limit</Badge>}
          </div>
          {hourly > 0 && (
            <MeterBar
              label="Spend in the last hour"
              fraction={meter.hourly_usd / hourly}
              color={spendOver ? warn : 'var(--wp-accent)'}
              maxLabel={usd(hourly)}
              maxTip={`Hourly spend limit: autopilot pauses when the last hour reaches ${usd(hourly)}`}
            />
          )}
        </Card>

        {weekPct !== undefined ? (
          <Card
            icon={<Gauge />}
            title="Week’s quota"
            tip="How much of this week’s Claude quota you have used"
            aside={
              <span className="flex items-center gap-1.5">
                {quotaOff && (
                  <Badge title="You turned the quota limits off for a while">limits off</Badge>
                )}
              </span>
            }
          >
            <div className="mt-0.5 flex items-baseline gap-2">
              <Value>{pct(weekPct)}</Value>
              {ahead !== undefined && Math.abs(ahead) >= 0.5 && (
                <Badge
                  color={over ? warn : ahead > 0 ? undefined : 'var(--wp-ok)'}
                  title={`${Math.abs(Math.round(ahead))} points ${ahead > 0 ? 'ahead of' : 'behind'} an even pace (${pct(even ?? 0)})`}
                >
                  {Math.abs(Math.round(ahead))} pts {ahead > 0 ? 'over' : 'under'} pace
                </Badge>
              )}
            </div>
            <MeterBar
              label="Week’s quota used"
              fraction={weekPct / 100}
              color={over ? warn : 'var(--wp-accent)'}
              marks={weekMarks}
              maxLabel="100%"
              maxTip="The week’s whole quota"
            />
            {pauseAt > 0 && (
              <div
                title={`Autopilot pauses when use passes ${weekPause.join(', or ')}: whichever comes first`}
                className="mt-0.5 flex cursor-help items-center gap-1 text-xs text-muted"
              >
                <Plane className="size-3 text-warn" />
                Pauses at {pct(pauseAt)}
              </div>
            )}
          </Card>
        ) : (
          <Card
            icon={<Gauge />}
            title="Week’s quota"
            tip="Quota limits wait for a reading from Claude Code’s status line"
          >
            <div title="No recent quota reading: limits wait for one from Claude Code’s status line">
              <Value>–</Value>
            </div>
          </Card>
        )}

        <Card
          icon={<Clock />}
          title="Five hours"
          tip="Use of the five-hour quota window"
          aside={
            fiveShown &&
            meter.five_hour_resets_at && (
              <span
                className="text-xs text-subtle"
                title={`The window resets ${relativeTime(meter.five_hour_resets_at)}`}
              >
                ↻ {relativeTime(meter.five_hour_resets_at)}
              </span>
            )
          }
        >
          {fiveShown ? (
            <>
              <div className="mt-0.5 flex items-baseline gap-2">
                <Value>{pct(meter.five_hour_percent!)}</Value>
                {quotaOff && (
                  <Badge title="You turned the quota limits off for a while">limits off</Badge>
                )}
              </div>
              <MeterBar
                label="Five-hour window used"
                fraction={meter.five_hour_percent! / 100}
                color={
                  fiveCap > 0 && meter.five_hour_percent! >= fiveCap ? warn : 'var(--wp-accent)'
                }
                marks={
                  fiveCap > 0 && fiveCap < 100
                    ? [
                        {
                          at: fiveCap / 100,
                          tone: 'limit',
                          tip: `Pauses at ${pct(fiveCap)} until the window resets`,
                        },
                      ]
                    : []
                }
                maxLabel="100%"
                maxTip="The window’s whole quota"
              />
            </>
          ) : (
            <div title={fiveNone} className="mt-0.5 cursor-help">
              <span className="text-sm text-muted">No reading yet</span>
            </div>
          )}
        </Card>
      </div>
    </section>
  )
}

const HOUR = 3_600_000

/** The hourly limit guards against a runaway; only the quota ones can be turned off for a while. */
const quotaPause = (why: string) => !why.startsWith('the last hour')

/**
 * Why autopilot is paused, with a way to turn the quota limits off for a
 * while; or, while they are off, until when, and a way to turn them back on.
 */
function PauseBanner({ meter }: { meter: AutopilotMeter }) {
  const set = useSetAutopilot()
  const pop = usePopover()
  const setOff = (until: Date) =>
    set.mutate(
      { limits: {}, merge: true, quota_off_until: until.toISOString() },
      { onError: (e) => toastError(e) },
    )
  const off = meter.quota_off_until
  if (!meter.paused_because && !off) return null
  const now = new Date(meter.measured_at).getTime()
  const reset = meter.week_resets_at ? new Date(meter.week_resets_at).getTime() : 0
  const choices: MenuItem[] = [
    { label: 'For an hour', onSelect: () => setOff(new Date(now + HOUR)) },
    { label: 'For five hours', onSelect: () => setOff(new Date(now + 5 * HOUR)) },
    ...(reset > now && reset - now <= 7 * 24 * HOUR
      ? [
          {
            label: 'Until the week resets',
            hint: relativeTime(meter.week_resets_at!),
            onSelect: () => setOff(new Date(reset)),
          },
        ]
      : []),
  ]
  return (
    <div
      role="status"
      className="flex flex-wrap items-center gap-x-2 gap-y-1.5 rounded-lg border border-warn/40 bg-warn/10 px-3 py-2 text-[13px]"
    >
      <Plane className="size-4 shrink-0 text-warn" />
      <span
        className="min-w-0 flex-1 basis-60"
        title={
          meter.paused_because
            ? 'It resumes by itself when the reading drops back.'
            : 'Autopilot goes on whatever the quota reads; the hourly limit still holds.'
        }
      >
        {meter.paused_because ? (
          <>
            <b className="font-medium">Autopilot is paused:</b> {meter.paused_because}
          </>
        ) : (
          <>
            <b className="font-medium">Quota limits are off</b> until{' '}
            <time dateTime={off} title={new Date(off!).toLocaleString()}>
              {new Date(off!).toLocaleString(undefined, {
                weekday: 'short',
                hour: 'numeric',
                minute: '2-digit',
              })}
            </time>
            , so autopilot keeps starting tasks
            {meter.quota_held ? `. It would pause otherwise: ${meter.quota_held}.` : '.'}
          </>
        )}
      </span>
      {off ? (
        <Button
          size="xs"
          loading={set.isPending}
          onClick={() => setOff(new Date(0))}
          className="ml-auto"
        >
          Turn back on
        </Button>
      ) : (
        meter.paused_because &&
        quotaPause(meter.paused_because) && (
          <>
            <Button
              ref={pop.anchor}
              size="xs"
              loading={set.isPending}
              onClick={pop.toggle}
              aria-expanded={pop.open}
              className="ml-auto"
            >
              Turn quota limits off
              <ChevronDown className="size-3" />
            </Button>
            <Popover open={pop.open} onClose={pop.close} anchor={pop.anchor} align="end">
              <MenuList items={choices} onClose={pop.close} />
            </Popover>
          </>
        )
      )}
    </div>
  )
}
