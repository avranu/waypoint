// How Hermes and its machine are doing: version, gateway, conversations,
// memory and disk; restarting the gateway; the skill curator.

import { useState, type ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { RotateCw } from 'lucide-react'
import { hermes, hermesQuery } from '@/lib/assistant'
import { duration, plural, relativeTime } from '@/lib/format'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Switch } from '@/components/ui/input'
import { Badge, Progress, Section, Skeleton } from '@/components/ui/misc'
import { toastError } from '@/components/ui/toast'
import { LoadError, LogTail, Rows, useGatewayRestart } from './common'

interface Status {
  version: string
  release_date: string
  config_version: number
  latest_config_version: number
  gateway_running: boolean
  gateway_state: string | null
  gateway_exit_reason: string | null
  gateway_updated_at: string | null
  active_agents: number
  gateway_busy: boolean
  active_sessions: number
  components: Record<
    string,
    { status: string; state?: string; configured?: number; connected?: number }
  >
  overall: 'ok' | 'degraded'
  memory: {
    pressure: string
    gateway_rss_mb?: number
    system_total_mb?: number
    system_available_mb?: number
  }
  disk: { pressure: string; total_mb?: number; free_mb?: number; used_percent?: number }
}

interface Stats {
  os: string
  arch: string
  hostname: string
  python_version: string
  hermes_version: string
  cpu_count: number | null
  cpu_percent?: number
  load_avg?: number[]
  uptime_seconds?: number
  memory?: { total: number; available: number; used: number; percent: number }
  disk?: { total: number; used: number; free: number; percent: number }
}

interface Curator {
  enabled: boolean
  paused: boolean
  interval_hours: number | null
  last_run_at: string | null
  min_idle_hours: number | null
  stale_after_days: number | null
  archive_after_days: number | null
}

const GATEWAY: Record<string, [string, string]> = {
  running: ['Running', 'var(--wp-ok)'],
  starting: ['Starting', 'var(--wp-warn)'],
  degraded: ['Degraded', 'var(--wp-warn)'],
  draining: ['Draining', 'var(--wp-warn)'],
  stopped: ['Stopped', 'var(--wp-danger)'],
  startup_failed: ['Failed to start', 'var(--wp-danger)'],
}

const PRESSURE: Record<string, string> = {
  ok: 'var(--wp-ok)',
  elevated: 'var(--wp-warn)',
  critical: 'var(--wp-danger)',
}

/** 168 → "7 days", 2 → "2 hours". */
const hoursText = (h: number) =>
  h >= 48 && h % 24 === 0
    ? plural(h / 24, 'day')
    : h >= 1
      ? plural(+h.toFixed(1), 'hour')
      : duration(h * 3600)

const gb = (bytes: number) => `${(bytes / 1024 ** 3).toFixed(1)} GB`

const level = (pct: number) =>
  pct >= 90 ? 'var(--wp-danger)' : pct >= 75 ? 'var(--wp-warn)' : 'var(--wp-ok)'

export function SystemSection({ id }: { id: number }) {
  const status = useQuery({
    ...hermesQuery<Status>(id, 'status', undefined, 5_000),
    refetchInterval: 10_000,
  })
  const stats = useQuery({
    ...hermesQuery<Stats>(id, 'system/stats', undefined, 10_000),
    refetchInterval: 15_000,
  })
  const restart = useGatewayRestart(id)
  const [confirming, setConfirming] = useState(false)
  const s = status.data
  const gw = s
    ? (GATEWAY[s.gateway_state ?? ''] ?? [s.gateway_state ?? 'Unknown', 'var(--wp-subtle)'])
    : null
  const platforms = s?.components.platforms
  return (
    <>
      <Section
        title="Hermes"
        actions={
          <Button
            size="sm"
            icon={<RotateCw className="size-3.5" />}
            loading={restart.busy}
            disabled={!s}
            onClick={() => setConfirming(true)}
          >
            Restart gateway
          </Button>
        }
      >
        {status.isPending ? (
          <div className="space-y-2 p-4">
            <Skeleton className="h-6" />
            <Skeleton className="h-6" />
            <Skeleton className="h-6" />
          </div>
        ) : status.isError ? (
          <LoadError error={status.error} what="the status" />
        ) : (
          s &&
          gw && (
            <Rows
              rows={[
                [
                  'Version',
                  <>
                    {s.version}
                    {s.release_date && (
                      <span className="text-subtle"> · released {s.release_date}</span>
                    )}
                  </>,
                ],
                [
                  'Gateway',
                  <span className="flex flex-wrap items-center gap-2">
                    <Badge color={gw[1]}>{gw[0]}</Badge>
                    {s.gateway_busy && <span className="text-muted">busy</span>}
                    {s.gateway_exit_reason && (
                      <span className="text-danger">{s.gateway_exit_reason}</span>
                    )}
                    {s.gateway_updated_at && (
                      <span className="text-subtle">
                        since {relativeTime(s.gateway_updated_at)}
                      </span>
                    )}
                  </span>,
                ],
                [
                  'Working on',
                  s.active_sessions || s.active_agents ? (
                    `${plural(s.active_sessions, 'conversation')} · ${plural(s.active_agents, 'agent')} running`
                  ) : (
                    <span className="text-muted">Nothing right now</span>
                  ),
                ],
                [
                  'Channels',
                  platforms?.configured ? (
                    `${platforms.connected ?? 0} of ${platforms.configured} connected`
                  ) : (
                    <span className="text-muted">None set up</span>
                  ),
                ],
                [
                  'Health',
                  s.overall === 'ok' ? (
                    <Badge color="var(--wp-ok)">OK</Badge>
                  ) : (
                    <span className="flex flex-wrap items-center gap-1.5">
                      <Badge color="var(--wp-warn)">Degraded</Badge>
                      {Object.entries(s.components)
                        .filter(([, c]) => c.status !== 'ok')
                        .map(([k]) => (
                          <span key={k} className="text-muted">
                            {k}
                          </span>
                        ))}
                    </span>
                  ),
                ],
                [
                  'Configuration',
                  s.config_version < s.latest_config_version ? (
                    <span className="text-warn">
                      version {s.config_version}; {s.latest_config_version} is current
                    </span>
                  ) : (
                    `version ${s.config_version}`
                  ),
                ],
              ]}
            />
          )
        )}
        {restart.phase !== 'idle' && (
          <div className="border-t border-line">
            <div className="px-4 pt-2 text-xs font-medium text-muted">
              {restart.busy
                ? 'Restarting…'
                : restart.phase === 'done'
                  ? 'Restarted'
                  : 'Restart failed'}
            </div>
            <LogTail lines={restart.lines} className="mt-2 rounded-b-xl" />
          </div>
        )}
      </Section>

      <Section title="Machine">
        {stats.isPending ? (
          <div className="grid gap-4 p-4 sm:grid-cols-3">
            <Skeleton className="h-14" />
            <Skeleton className="h-14" />
            <Skeleton className="h-14" />
          </div>
        ) : stats.isError ? (
          <LoadError error={stats.error} what="the machine's stats" />
        ) : (
          stats.data && <Machine st={stats.data} status={s} />
        )}
      </Section>

      <CuratorSection id={id} />

      <Confirm
        open={confirming}
        onClose={() => setConfirming(false)}
        title="Restart the gateway?"
        body="Conversations in progress stop, and channels reconnect."
        confirmLabel="Restart"
        onConfirm={restart.start}
      />
    </>
  )
}

function Meter({
  label,
  pct,
  detail,
  pressure,
}: {
  label: string
  pct: number
  detail: string
  pressure?: string
}) {
  return (
    <div className="min-w-0">
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-[13px] text-muted">{label}</span>
        <span className="text-[13px] font-medium tabular-nums">{Math.round(pct)}%</span>
      </div>
      <Progress value={pct} max={100} color={level(pct)} className="mt-1.5" />
      <div className="mt-1 flex items-center gap-1.5 truncate text-xs text-subtle">
        {detail}
        {pressure && pressure !== 'ok' && pressure !== 'unknown' && (
          <Badge color={PRESSURE[pressure]}>{pressure}</Badge>
        )}
      </div>
    </div>
  )
}

function Machine({ st, status }: { st: Stats; status?: Status }) {
  return (
    <>
      <div className="grid gap-4 p-4 sm:grid-cols-3">
        {st.cpu_percent !== undefined && (
          <Meter
            label="CPU"
            pct={st.cpu_percent}
            detail={
              (st.cpu_count ? plural(st.cpu_count, 'core') : '') +
              (st.load_avg ? ` · load ${st.load_avg.map((l) => l.toFixed(1)).join(' ')}` : '')
            }
          />
        )}
        {st.memory && (
          <Meter
            label="Memory"
            pct={st.memory.percent}
            detail={`${gb(st.memory.available)} free of ${gb(st.memory.total)}`}
            pressure={status?.memory.pressure}
          />
        )}
        {st.disk && (
          <Meter
            label="Disk"
            pct={st.disk.percent}
            detail={`${gb(st.disk.free)} free of ${gb(st.disk.total)}`}
            pressure={status?.disk.pressure}
          />
        )}
      </div>
      <div className="border-t border-line">
        <Rows
          rows={[
            ['System', `${st.os} ${st.arch} · Python ${st.python_version}`],
            ...(status?.memory.gateway_rss_mb
              ? ([['Gateway memory', `${status.memory.gateway_rss_mb} MB`]] as [string, string][])
              : []),
            ...(st.uptime_seconds
              ? ([['Up for', duration(st.uptime_seconds)]] as [string, string][])
              : []),
          ]}
        />
      </div>
    </>
  )
}

function CuratorSection({ id }: { id: number }) {
  const qc = useQueryClient()
  const cur = useQuery(hermesQuery<Curator>(id, 'curator', undefined, 30_000))
  const pause = useMutation({
    mutationFn: (paused: boolean) => hermes(id, 'PUT', 'curator/paused', { paused }),
    onMutate: (paused) => {
      const key = hermesQuery<Curator>(id, 'curator').queryKey
      const prev = qc.getQueryData<Curator>(key)
      if (prev) qc.setQueryData(key, { ...prev, paused })
      return prev
    },
    onError: (e, _, prev) => {
      if (prev) qc.setQueryData(hermesQuery<Curator>(id, 'curator').queryKey, prev)
      toastError(e)
    },
    onSettled: () => qc.invalidateQueries({ queryKey: ['assistant', id, 'curator'] }),
  })
  const c = cur.data
  return (
    <Section
      title="Curator"
      help="Hermes's curator reviews the agent's skills when it's idle and archives ones that go unused."
      actions={
        c?.enabled && (
          <label className="flex items-center gap-2 text-[13px] text-muted">
            {c.paused ? 'Paused' : 'On'}
            <Switch checked={!c.paused} onChange={(on) => pause.mutate(!on)} label="Curator on" />
          </label>
        )
      }
    >
      {cur.isPending ? (
        <div className="p-4">
          <Skeleton className="h-16" />
        </div>
      ) : cur.isError ? (
        <LoadError error={cur.error} what="the curator" />
      ) : (
        c && (
          <Rows
            rows={[
              ...(!c.enabled
                ? ([
                    [
                      'State',
                      <span className="text-muted">
                        Off in the configuration (curator.enabled)
                      </span>,
                    ],
                  ] as [string, ReactNode][])
                : []),
              [
                'Last run',
                c.last_run_at ? (
                  relativeTime(c.last_run_at)
                ) : (
                  <span className="text-muted">Never</span>
                ),
              ],
              ['Runs every', c.interval_hours ? hoursText(c.interval_hours) : '—'],
              ['After idle for', c.min_idle_hours ? hoursText(c.min_idle_hours) : '—'],
              [
                'Archives skills',
                c.archive_after_days
                  ? `unused for ${plural(c.archive_after_days, 'day')}` +
                    (c.stale_after_days ? `; stale after ${c.stale_after_days}` : '')
                  : '—',
              ],
            ]}
          />
        )
      )}
    </Section>
  )
}
