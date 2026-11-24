import { useEffect, useMemo, useState, type ReactNode } from 'react'
import { Link, useNavigate, useParams } from '@tanstack/react-router'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import clsx from 'clsx'
import {
  AlertTriangle,
  CheckCircle2,
  ChevronLeft,
  Eye,
  Loader2,
  Play,
  RefreshCw,
  Rocket,
  Square,
  Trash2,
  XCircle,
} from 'lucide-react'
import { api, enc } from '@/lib/api'
import {
  CHECK_INTERVALS,
  KIND_LABEL,
  LAG_ALERTS,
  NAME_FONT,
  ago,
  deployWho,
  envQuery,
  pct,
  shortSha,
  statusOf,
  useDeleteEnv,
  useEnvAction,
  useUpdateEnv,
  type Deployment,
  type EnvInput,
  type EnvKind,
  type EnvironmentDetail,
  type HistoryPoint,
  type UptimeDay,
} from '@/lib/environments'
import { dateTime, safeUrl } from '@/lib/format'
import { toast, toastError } from '@/components/ui/toast'
import { useProject } from './Project'
import { ExternalUrl, KindBadge, RunsLine, StatusDot, statusText } from '@/components/environments'
import { LineChart, fmtNum, vizClass, viz } from '@/components/charts'
import { Button } from '@/components/ui/button'
import { Confirm } from '@/components/ui/dialog'
import { Field, Input, Select, Switch } from '@/components/ui/input'
import { Empty, Section, Skeleton } from '@/components/ui/misc'

export function EnvironmentPage() {
  const { key, name } = useParams({ from: '/app/p/$key/environments/$name' })
  const project = useProject()
  const query = useQuery(envQuery(key, name))
  const env = query.data
  useEffect(() => {
    document.title = `${name} · Environments · ${project.name} · Waypoint`
  }, [name, project.name])
  const canEdit = project.role === 'owner' || project.role === 'editor'
  return (
    <div className="min-h-0 flex-1 overflow-y-auto pb-20">
      <div className="mx-auto w-full max-w-4xl px-4 pt-3 sm:px-6">
        <Link
          to="/p/$key/environments"
          params={{ key }}
          className="inline-flex h-8 items-center gap-1 rounded-md pr-2 text-[13px] text-muted hover:text-fg"
        >
          <ChevronLeft className="size-4" /> Environments
        </Link>
        {query.isPending ? (
          <div className="mt-2 space-y-3">
            <Skeleton className="h-16" />
            <Skeleton className="h-40" />
          </div>
        ) : !env ? (
          <Empty icon={<Rocket />} title="Environment not found">
            It may have been deleted, or dev environments are forgotten a week after they stop.
          </Empty>
        ) : (
          <EnvironmentView env={env} canEdit={canEdit} />
        )}
      </div>
    </div>
  )
}

function EnvironmentView({ env, canEdit }: { env: EnvironmentDetail; canEdit: boolean }) {
  const navigate = useNavigate()
  const qc = useQueryClient()
  const check = useEnvAction(env.id, 'check')
  const stop = useEnvAction(env.id, 'stop')
  const del = useDeleteEnv(env.id)
  const restart = useMutation({
    mutationFn: () =>
      api.post(`/projects/${enc(env.project)}/environments/${enc(env.name)}/up`, {}),
    onError: toastError,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['environments'] }),
  })
  const [confirm, setConfirm] = useState<'stop' | 'delete' | null>(null)
  const s = statusOf(env)
  const days = env.days
  const checked = days.reduce((n, d) => n + d.checks, 0)
  const okChecks = days.reduce((n, d) => n + d.ok, 0)
  return (
    <div className="mt-1 space-y-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="flex flex-wrap items-center gap-2">
            <StatusDot env={env} className="!size-3" />
            <h2 className={clsx(NAME_FONT, 'truncate text-xl font-semibold tracking-tight')}>
              {env.name}
            </h2>
            <KindBadge env={env} />
          </div>
          <div className="mt-1 text-[13px]" style={{ color: s.color }}>
            {statusText(env)}
          </div>
          <div className="mt-1 text-[13px]">
            <ExternalUrl url={env.url} />
          </div>
        </div>
        {canEdit && (
          <div className="flex flex-wrap items-center gap-2">
            {!env.stopped && env.url && env.interval_s > 0 && (
              <Button
                size="sm"
                icon={<RefreshCw className="size-3.5" />}
                loading={check.isPending}
                onClick={() => check.mutate()}
              >
                Check now
              </Button>
            )}
            {env.stopped && (
              <Button
                size="sm"
                variant="primary"
                icon={<Play className="size-3.5" />}
                loading={restart.isPending}
                onClick={() => restart.mutate()}
              >
                Start again
              </Button>
            )}
            {!env.stopped && (env.kind === 'dev' || env.kind === 'preview' || env.ephemeral) && (
              <Button
                size="sm"
                icon={<Square className="size-3.5" />}
                onClick={() => setConfirm('stop')}
              >
                Stop
              </Button>
            )}
          </div>
        )}
      </div>

      <WarningBar env={env} />

      <div className="grid gap-5 md:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
        <Section title="What it runs">
          <dl className="space-y-3 p-4 text-[13px]">
            <Row label="Release">
              <RunsLine env={env} />
            </Row>
            {env.deployed_at && (
              <Row label="Deployed">{`${ago(env.deployed_at)} · ${dateTime(env.deployed_at)}`}</Row>
            )}
            {env.behind && (
              <Row label="Its branch">
                Has moved on: the branch head is{' '}
                <span className="font-mono text-xs">{shortSha(env.head ?? '')}</span>
              </Row>
            )}
            {env.repo && <Row label="Repo">{env.repo}</Row>}
            {env.ephemeral && env.expires_at && (
              <Row label="Stops">{`${dateTime(env.expires_at)} unless a deploy or wp env up renews it`}</Row>
            )}
            {env.last_checked_at && !env.stopped && (
              <Row label="Last check">
                {ago(env.last_checked_at)}
                {env.http_code != null ? ` · HTTP ${env.http_code}` : ''}
                {env.latency_ms != null ? ` · ${env.latency_ms} ms` : ''}
              </Row>
            )}
            {env.tls_expires_at && (
              <Row label="Certificate">
                <span className={clsx(env.tls_warning && 'text-warn')}>
                  Expires {new Date(env.tls_expires_at).toLocaleDateString()}
                </span>
              </Row>
            )}
          </dl>
        </Section>

        <Section title="Uptime">
          <div className="grid grid-cols-3 gap-2 p-4 text-center">
            <Stat label="24 hours" value={pct(env.uptime_24h)} />
            <Stat label="30 days" value={pct(env.uptime_30d)} />
            <Stat label="90 days" value={checked ? pct((okChecks / checked) * 100) : '–'} />
          </div>
          <div className="border-t border-line px-4 pb-4 pt-3">
            <DayStrip days={days} />
          </div>
        </Section>
      </div>

      {env.interval_s > 0 && !env.stopped && (
        <Section title="Last 48 hours">
          <div className="space-y-3 p-4">
            <StatusStrip history={env.history} />
            <LatencyChart history={env.history} />
          </div>
        </Section>
      )}

      <Section title="Deployments">
        {env.deployments.length === 0 ? (
          <p className="px-4 py-6 text-center text-[13px] text-muted">
            None recorded yet. CI can report them with{' '}
            <code className="font-mono text-xs">wp env deploy {env.name} --commit SHA</code>
            {env.version_url
              ? '.'
              : ', or set a version address below and Waypoint will see new releases itself.'}
          </p>
        ) : (
          <ul className="divide-y divide-line">
            {env.deployments.map((d) => (
              <DeploymentRow key={d.id} d={d} />
            ))}
          </ul>
        )}
      </Section>

      <SettingsForm env={env} canEdit={canEdit} />

      {canEdit && (
        <Section title="Remove">
          <div className="flex flex-wrap items-center justify-between gap-3 p-4">
            <p className="max-w-md text-[13px] text-muted">
              Deleting an environment erases its deployments and uptime history. To keep them, set
              the check interval to “Not checked” instead.
            </p>
            <Button
              variant="danger"
              size="sm"
              icon={<Trash2 className="size-3.5" />}
              onClick={() => setConfirm('delete')}
            >
              Delete environment
            </Button>
          </div>
        </Section>
      )}

      <Confirm
        open={confirm === 'stop'}
        onClose={() => setConfirm(null)}
        title={`Stop ${env.name}?`}
        body="It is no longer checked and moves to the folded Stopped list. Start it again, or run wp env up, any time."
        confirmLabel="Stop"
        danger={false}
        onConfirm={() => stop.mutate()}
      />
      <Confirm
        open={confirm === 'delete'}
        onClose={() => setConfirm(null)}
        title={`Delete ${env.name}?`}
        body="Its deployments and history go with it. This can’t be undone."
        onConfirm={() =>
          del.mutate(undefined, {
            onSuccess: () => {
              toast(`Deleted ${env.name}`, { kind: 'success' })
              navigate({ to: '/p/$key/environments', params: { key: env.project } })
            },
          })
        }
      />
    </div>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="grid grid-cols-[6.5rem_minmax(0,1fr)] gap-x-3">
      <dt className="text-muted">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </div>
  )
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="text-lg font-semibold tabular-nums">{value}</div>
      <div className="text-xs text-subtle">{label}</div>
    </div>
  )
}

/** What needs a look, above the details. */
function WarningBar({ env }: { env: EnvironmentDetail }) {
  const items: ReactNode[] = []
  if (!env.stopped && env.status === 'down')
    items.push(
      <>
        Not responding{env.error ? `: ${env.error}` : ''}
        {env.http_code ? ` (HTTP ${env.http_code})` : ''}.
      </>,
    )
  if (!env.stopped && env.status === 'degraded' && !env.mismatch)
    items.push(<>Slow: answering in {env.latency_ms} ms.</>)
  if (!env.stopped && env.mismatch)
    items.push(<>Still shows the previous release; the new deployment has not arrived yet.</>)
  if (!env.stopped && env.tls_warning && env.tls_expires_at)
    items.push(<>Its certificate expires {new Date(env.tls_expires_at).toLocaleDateString()}.</>)
  if (!items.length) return null
  return (
    <div
      role="status"
      className={clsx(
        'flex items-start gap-2 rounded-xl border px-4 py-3 text-[13px]',
        env.status === 'down'
          ? 'border-danger/30 bg-danger/5 text-danger'
          : 'border-warn/30 bg-warn/5 text-warn',
      )}
    >
      <AlertTriangle className="mt-0.5 size-4 shrink-0" />
      <div className="space-y-0.5">
        {items.map((i, n) => (
          <div key={n}>{i}</div>
        ))}
      </div>
    </div>
  )
}

const okColor = 'var(--wp-ok)'
const warnColor = 'var(--wp-warn)'
const badColor = 'var(--wp-danger)'
const noneColor = 'var(--wp-line-strong)'

/** Each ten minutes of the last 48 hours, green when every check passed. */
function StatusStrip({ history }: { history: HistoryPoint[] }) {
  const step = 10 * 60_000
  const slots = 288
  const cells = useMemo(() => {
    const end = Math.floor(Date.now() / step) * step
    const by = new Map(history.map((h) => [Math.floor(new Date(h.at).getTime() / step) * step, h]))
    return Array.from({ length: slots }, (_, i) => {
      const at = end - (slots - 1 - i) * step
      return { at, h: by.get(at) }
    })
  }, [history, step])
  const down = history.filter((h) => h.ok < h.checks).length
  return (
    <div>
      <div
        role="img"
        aria-label={`Status over the last 48 hours: ${down ? `${down} ten-minute periods with failed checks` : 'no failed checks'}`}
        className="flex h-8 gap-px overflow-hidden rounded-md"
      >
        {cells.map(({ at, h }) => {
          const color = !h
            ? noneColor
            : h.ok === h.checks
              ? okColor
              : h.ok === 0
                ? badColor
                : warnColor
          const when = new Date(at).toLocaleString(undefined, {
            weekday: 'short',
            hour: '2-digit',
            minute: '2-digit',
          })
          return (
            <span
              key={at}
              title={!h ? `${when}: no checks` : `${when}: ${h.ok} of ${h.checks} checks passed`}
              className="min-w-0 flex-1"
              style={{ background: color, opacity: h ? 1 : 0.35 }}
            />
          )
        })}
      </div>
      <div className="mt-1 flex justify-between text-[11px] text-subtle">
        <span>48 hours ago</span>
        <span>24 hours ago</span>
        <span>Now</span>
      </div>
    </div>
  )
}

function LatencyChart({ history }: { history: HistoryPoint[] }) {
  if (history.length < 2)
    return (
      <p className="text-xs text-subtle">Response times show here once a few checks have run.</p>
    )
  const labels = history.map((h) =>
    new Date(h.at).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' }),
  )
  const titles = history.map((h) =>
    new Date(h.at).toLocaleString(undefined, {
      weekday: 'short',
      hour: '2-digit',
      minute: '2-digit',
    }),
  )
  return (
    <div className={vizClass}>
      <div className="mb-1 text-xs text-muted">Response time</div>
      <LineChart
        ariaLabel="Response time over the last 48 hours"
        labels={labels}
        titles={titles}
        unit="ms"
        height={140}
        format={(v) => `${fmtNum(v, 0)} ms`}
        series={[
          {
            name: 'Average',
            color: viz.blue,
            area: true,
            values: history.map((h) => (h.ok > 0 ? h.latency_ms : null)),
          },
        ]}
      />
    </div>
  )
}

/** Ninety days, one cell each, colored by that day's uptime. */
function DayStrip({ days }: { days: UptimeDay[] }) {
  const color = (d: UptimeDay) =>
    d.uptime == null
      ? noneColor
      : d.uptime >= 99.9
        ? okColor
        : d.uptime >= 95
          ? warnColor
          : badColor
  const bad = days.filter((d) => d.uptime != null && d.uptime < 99.9).length
  return (
    <div>
      <div
        role="img"
        aria-label={`Uptime by day, last 90 days: ${bad ? `${bad} days below 99.9%` : 'no day below 99.9%'}`}
        className="flex h-9 gap-px overflow-hidden rounded-md"
      >
        {days.map((d) => (
          <span
            key={d.date}
            title={
              d.uptime == null
                ? `${d.date}: no checks`
                : `${d.date}: ${pct(d.uptime)} (${d.ok} of ${d.checks} checks)`
            }
            className="min-w-0 flex-1"
            style={{ background: color(d), opacity: d.uptime == null ? 0.35 : 1 }}
          />
        ))}
      </div>
      <div className="mt-1 flex justify-between text-[11px] text-subtle">
        <span>90 days ago</span>
        <span>Today</span>
      </div>
    </div>
  )
}

const DEPLOY: Record<Deployment['status'], { label: string; color: string; icon: ReactNode }> = {
  started: {
    label: 'Deploying',
    color: 'var(--wp-accent)',
    icon: <Loader2 className="animate-spin" />,
  },
  succeeded: { label: 'Succeeded', color: 'var(--wp-ok)', icon: <CheckCircle2 /> },
  failed: { label: 'Failed', color: 'var(--wp-danger)', icon: <XCircle /> },
  detected: { label: 'Seen running', color: 'var(--wp-subtle)', icon: <Eye /> },
}

function DeploymentRow({ d }: { d: Deployment }) {
  const st = DEPLOY[d.status]
  const commit = d.commit ? shortSha(d.commit) : ''
  const link = safeUrl(d.commit_url)
  const run = safeUrl(d.url)
  return (
    <li className="flex items-start gap-3 px-4 py-3 text-[13px]">
      <span
        className="mt-0.5 shrink-0 [&>svg]:size-4"
        style={{ color: st.color }}
        role="img"
        aria-label={st.label}
        title={st.label}
      >
        {st.icon}
      </span>
      <div className="min-w-0 flex-1">
        <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5">
          <span
            className="font-medium"
            style={{ color: d.status === 'failed' ? st.color : undefined }}
          >
            {st.label}
          </span>
          {commit &&
            (link ? (
              <a
                href={link}
                target="_blank"
                rel="noreferrer"
                className="font-mono text-xs text-accent hover:underline"
              >
                {commit}
              </a>
            ) : (
              <span className="font-mono text-xs">{commit}</span>
            ))}
          {d.branch && <span className="font-mono text-xs text-muted">{d.branch}</span>}
          {d.version && <span className="text-xs text-muted">v{d.version.replace(/^v/, '')}</span>}
        </div>
        <div className="mt-0.5 flex flex-wrap items-center gap-x-2 text-xs text-muted">
          <span title={dateTime(d.started_at)}>{ago(d.finished_at ?? d.started_at)}</span>
          {d.by_kind !== 'detected' && <span>· {deployWho(d)}</span>}
          {run && (
            <a href={run} target="_blank" rel="noreferrer" className="text-accent hover:underline">
              CI run
            </a>
          )}
        </div>
        {d.notes && d.by_kind !== 'detected' && (
          <p className="mt-1 text-xs text-muted">{d.notes}</p>
        )}
      </div>
    </li>
  )
}

function SettingsForm({ env, canEdit }: { env: EnvironmentDetail; canEdit: boolean }) {
  const navigate = useNavigate()
  const update = useUpdateEnv(env.id)
  const initial = useMemo(
    () => ({
      name: env.name,
      kind: env.kind,
      url: env.url,
      health_url: env.health_url,
      expected_status: env.expected_status ? String(env.expected_status) : '',
      interval_s: env.interval_s,
      version_url: env.version_url,
      commit_field: env.commit_field,
      branch_field: env.branch_field,
      version_field: env.version_field,
      ephemeral: env.ephemeral,
      lag_alert_m: env.lag_alert_m,
    }),
    [env],
  )
  const [f, setF] = useState(initial)
  const set = <K extends keyof typeof f>(k: K, v: (typeof f)[K]) => setF((x) => ({ ...x, [k]: v }))
  const dirty = JSON.stringify(f) !== JSON.stringify(initial)
  const status = Number(f.expected_status)
  const statusBad = f.expected_status !== '' && !(status >= 100 && status <= 599)
  const needsField =
    f.version_url.trim() !== '' && !f.commit_field.trim() && !f.version_field.trim()
  const intervals = CHECK_INTERVALS.some((i) => i.value === f.interval_s)
    ? CHECK_INTERVALS
    : [...CHECK_INTERVALS, { value: f.interval_s, label: `Every ${f.interval_s} seconds` }]
  const lags = LAG_ALERTS.some((i) => i.value === f.lag_alert_m)
    ? LAG_ALERTS
    : [...LAG_ALERTS, { value: f.lag_alert_m, label: `After ${f.lag_alert_m} minutes` }]
  const save = () => {
    const body: EnvInput = {
      name: f.name.trim(),
      kind: f.kind,
      url: f.url.trim(),
      health_url: f.health_url.trim(),
      expected_status: f.expected_status === '' ? 0 : status,
      interval_s: f.interval_s,
      version_url: f.version_url.trim(),
      commit_field: f.commit_field.trim(),
      branch_field: f.branch_field.trim(),
      version_field: f.version_field.trim(),
      ephemeral: f.ephemeral,
      // An unchanged lag limit is left out of the request, so the server moves it with a change of kind.
      lag_alert_m: f.lag_alert_m === env.lag_alert_m ? undefined : f.lag_alert_m,
    }
    update.mutate(body, {
      onSuccess: (e) => {
        toast('Settings saved', { kind: 'success' })
        if (e.name !== env.name)
          navigate({
            to: '/p/$key/environments/$name',
            params: { key: env.project, name: e.name },
            replace: true,
          })
      },
    })
  }
  return (
    <Section title="Settings">
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (dirty && !statusBad && !needsField) save()
        }}
      >
        <fieldset disabled={!canEdit} className="grid gap-4 sm:grid-cols-2">
          <Field label="Name" note="CI refers to the environment by this name.">
            <Input value={f.name} onChange={(e) => set('name', e.target.value)} maxLength={63} />
          </Field>
          <Field label="Kind">
            <Select value={f.kind} onChange={(e) => set('kind', e.target.value as EnvKind)}>
              {(Object.keys(KIND_LABEL) as EnvKind[]).map((k) => (
                <option key={k} value={k}>
                  {KIND_LABEL[k]}
                </option>
              ))}
            </Select>
          </Field>
          <Field label="Address" className="sm:col-span-2">
            <Input
              type="url"
              inputMode="url"
              value={f.url}
              onChange={(e) => set('url', e.target.value)}
              placeholder="https://staging.example.com"
            />
          </Field>
          <Field
            label="Health address"
            hint="Waypoint requests this instead of the address when the app has a lighter endpoint, like /healthz."
          >
            <Input
              type="url"
              inputMode="url"
              value={f.health_url}
              onChange={(e) => set('health_url', e.target.value)}
              placeholder="Same as the address"
            />
          </Field>
          <Field label="Check">
            <Select
              value={String(f.interval_s)}
              onChange={(e) => set('interval_s', Number(e.target.value))}
            >
              {intervals.map((i) => (
                <option key={i.value} value={i.value}>
                  {i.label}
                </option>
              ))}
            </Select>
          </Field>
          <Field
            label="Expected status"
            error={statusBad ? 'An HTTP code between 100 and 599' : undefined}
            hint="The HTTP code that counts as up. Empty: any 2xx."
          >
            <Input
              inputMode="numeric"
              value={f.expected_status}
              onChange={(e) =>
                set('expected_status', e.target.value.replace(/\D/g, '').slice(0, 3))
              }
              placeholder="Any 2xx"
            />
          </Field>
          <Field
            label="Not deployed alert"
            hint="Tell the project’s owners when it still runs an older commit than its branch this long after the branch moved: a release or deploy that didn’t happen."
            className="sm:col-span-2"
          >
            <Select
              value={String(f.lag_alert_m)}
              onChange={(e) => set('lag_alert_m', Number(e.target.value))}
            >
              {lags.map((i) => (
                <option key={i.value} value={i.value}>
                  {i.label}
                </option>
              ))}
            </Select>
          </Field>
          {(f.kind === 'dev' || f.kind === 'preview') && (
            <div className="flex items-center justify-between gap-3 sm:col-span-2">
              <div>
                <div className="text-[13px] font-medium">Temporary</div>
                <div className="text-xs text-subtle">
                  Stopped when it can’t be reached for its time to live (three days), and forgotten
                  a week later.
                </div>
              </div>
              <Switch
                checked={f.ephemeral}
                onChange={(v) => set('ephemeral', v)}
                label="Temporary"
                disabled={!canEdit}
              />
            </div>
          )}
          <div className="border-t border-line pt-4 sm:col-span-2">
            <div className="text-[13px] font-medium">Version endpoint</div>
            <p className="mt-0.5 text-xs text-subtle">
              An address that answers with JSON saying what runs, such as{' '}
              <code className="font-mono">{'{"version":"1.4.0","git":{"commit":"3fa9c1e"}}'}</code>.
              Waypoint records new releases it sees, and calls the environment degraded while it
              still shows the previous one after a deploy.
            </p>
          </div>
          <Field label="Version address" className="sm:col-span-2">
            <Input
              type="url"
              inputMode="url"
              value={f.version_url}
              onChange={(e) => set('version_url', e.target.value)}
              placeholder="https://staging.example.com/version"
            />
          </Field>
          <Field
            label="Commit field"
            hint="Where the commit is in the JSON, as a path with dots: git.commit"
            error={needsField ? 'Name the commit or the version field' : undefined}
          >
            <Input
              value={f.commit_field}
              onChange={(e) => set('commit_field', e.target.value)}
              placeholder="git.commit"
              autoCapitalize="off"
            />
          </Field>
          <Field label="Branch field">
            <Input
              value={f.branch_field}
              onChange={(e) => set('branch_field', e.target.value)}
              placeholder="git.branch"
              autoCapitalize="off"
            />
          </Field>
          <Field label="Version field">
            <Input
              value={f.version_field}
              onChange={(e) => set('version_field', e.target.value)}
              placeholder="version"
              autoCapitalize="off"
            />
          </Field>
        </fieldset>
        {canEdit && (
          <div className="flex items-center justify-end gap-2 border-t border-line pt-3">
            {dirty && (
              <Button variant="ghost" onClick={() => setF(initial)}>
                Discard
              </Button>
            )}
            <Button
              type="submit"
              variant="primary"
              loading={update.isPending}
              disabled={!dirty || statusBad || needsField}
            >
              Save settings
            </Button>
          </div>
        )}
      </form>
    </Section>
  )
}
