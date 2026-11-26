import { useMemo, useState, type ReactNode } from 'react'
import { Link, useNavigate } from '@tanstack/react-router'
import clsx from 'clsx'
import { AlertTriangle, ExternalLink, GitBranch, Rocket } from 'lucide-react'
import {
  CHECK_INTERVALS,
  KIND_LABEL,
  NAME_FONT,
  ago,
  deployWho,
  pct,
  shortSha,
  sinceTime,
  statusOf,
  useCreateEnv,
  type Deployment,
  type EnvKind,
  type Environment,
} from '@/lib/environments'
import { safeUrl } from '@/lib/format'
import { Sparkbars, vizClass } from '@/components/charts'
import { Button } from '@/components/ui/button'
import { Dialog } from '@/components/ui/dialog'
import { Badge } from '@/components/ui/misc'
import { Field, Input, Select } from '@/components/ui/input'
import { toast } from '@/components/ui/toast'

export function StatusDot({
  env,
  className,
}: {
  env: Pick<Environment, 'status' | 'stopped'>
  className?: string
}) {
  const s = statusOf(env)
  return (
    <span
      aria-hidden
      className={clsx(
        'inline-block size-2.5 shrink-0 rounded-full',
        env.status === 'down' && !env.stopped && 'ring-4 ring-danger/20',
        className,
      )}
      style={{ background: s.color }}
    />
  )
}

/** "Up since 14:02" / "Down since Oct 12, 09:30" / "Stopped". */
export function statusText(env: Environment): string {
  const s = statusOf(env)
  if (env.stopped || env.status === 'unknown' || !env.status_since) return s.label
  return `${s.label} since ${sinceTime(env.status_since)}`
}

export function KindBadge({ env }: { env: Pick<Environment, 'kind' | 'ephemeral'> }) {
  return (
    <Badge>
      {KIND_LABEL[env.kind]}
      {env.ephemeral && env.kind !== 'dev' && env.kind !== 'preview' ? ' · temporary' : ''}
    </Badge>
  )
}

/** The address without its scheme: example.com/health. */
export const prettyUrl = (u: string) => u.replace(/^https?:\/\//, '').replace(/\/$/, '')

export function ExternalUrl({ url, className }: { url: string; className?: string }) {
  const href = safeUrl(url)
  if (!url) return <span className={clsx('text-subtle', className)}>No address</span>
  if (!href) return <span className={className}>{url}</span>
  return (
    <a
      href={href}
      target="_blank"
      rel="noreferrer"
      className={clsx(
        'relative z-[1] inline-flex max-w-full items-center gap-1 text-accent hover:underline',
        className,
      )}
    >
      <span className="truncate">{prettyUrl(url)}</span>
      <ExternalLink className="size-3 shrink-0" />
    </a>
  )
}

/** branch @ commit (linked to the forge), version. */
export function RunsLine({ env, className }: { env: Environment; className?: string }) {
  const link = safeUrl(env.commit_url)
  if (!env.commit && !env.version && !env.branch)
    return <span className={clsx('text-subtle', className)}>Nothing reported yet</span>
  return (
    <span
      className={clsx('inline-flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-0.5', className)}
    >
      {env.branch && (
        <span className="inline-flex min-w-0 items-center gap-1 font-mono text-xs">
          <GitBranch className="size-3 shrink-0 text-subtle" />
          <span className="truncate">{env.branch}</span>
        </span>
      )}
      {env.commit &&
        (link ? (
          <a
            href={link}
            target="_blank"
            rel="noreferrer"
            title={`Open ${env.commit} on the forge`}
            className="relative z-[1] font-mono text-xs text-accent hover:underline"
          >
            {env.branch ? '@ ' : ''}
            {shortSha(env.commit)}
          </a>
        ) : (
          <span className="font-mono text-xs">
            {env.branch ? '@ ' : ''}
            {shortSha(env.commit)}
          </span>
        ))}
      {env.version && <span className="text-xs text-muted">v{env.version.replace(/^v/, '')}</span>}
      {env.behind && (
        <Badge color="var(--wp-warn)" className="!h-4">
          behind its branch
        </Badge>
      )}
    </span>
  )
}

/** The last day, hour by hour: how much of each hour it was up. */
export function UptimeSpark({ env, height = 28 }: { env: Environment; height?: number }) {
  const spark = env.spark?.length ? env.spark : []
  const labels = useMemo(() => {
    const now = new Date()
    now.setMinutes(0, 0, 0)
    return spark.map((_, i) =>
      new Date(now.getTime() - (spark.length - 1 - i) * 3600_000).toLocaleTimeString(undefined, {
        hour: '2-digit',
        minute: '2-digit',
      }),
    )
  }, [spark])
  if (!spark.length || spark.every((v) => v < 0))
    return <div className="h-7 rounded bg-sunken/60" aria-hidden />
  return (
    <Sparkbars
      values={spark.map((v) => (v < 0 ? 0 : Math.max(v, 3)))}
      labels={labels}
      color={env.status === 'down' ? 'var(--wp-danger)' : 'var(--wp-ok)'}
      height={height}
      format={(v) => (v <= 0 ? 'no checks' : v <= 3 ? 'down' : `${Math.round(v)}% up`)}
      name="Uptime"
      ariaLabel={`Uptime in the last 24 hours, by hour${env.uptime_24h != null ? `: ${pct(env.uptime_24h)}` : ''}`}
      className={vizClass}
    />
  )
}

/** A notice under the card's title: what is wrong, in a few words. */
export function EnvNotes({ env }: { env: Environment }) {
  const notes: ReactNode[] = []
  if (!env.stopped && env.error && env.status !== 'up')
    notes.push(<span className="text-danger">{env.error}</span>)
  if (!env.stopped && env.mismatch)
    notes.push(<span className="text-warn">Still shows the previous release</span>)
  if (!env.stopped && env.tls_warning && env.tls_expires_at) {
    const days = Math.floor((new Date(env.tls_expires_at).getTime() - Date.now()) / 86400_000)
    notes.push(
      <span className="inline-flex items-center gap-1 text-warn">
        <AlertTriangle className="size-3" />
        {days < 0 ? 'Certificate expired' : `Certificate expires in ${days} d`}
      </span>,
    )
  }
  if (!notes.length) return null
  return (
    <div className="flex flex-wrap gap-x-3 gap-y-0.5 text-xs">
      {notes.map((n, i) => (
        <span key={i}>{n}</span>
      ))}
    </div>
  )
}

function LastDeploy({ d }: { d: Deployment }) {
  const verb =
    d.status === 'failed'
      ? 'Deploy failed'
      : d.status === 'started'
        ? 'Deploying'
        : d.status === 'detected'
          ? 'New release seen'
          : 'Deployed'
  return (
    <span
      className={clsx(
        'inline-flex min-w-0 items-center gap-1',
        d.status === 'failed' && 'text-danger',
      )}
    >
      <Rocket className="size-3 shrink-0" />
      <span className="truncate">
        {verb} {ago(d.finished_at ?? d.started_at)}
        {d.by_kind !== 'detected' ? ` · ${deployWho(d)}` : ''}
      </span>
    </span>
  )
}

/** One environment on the project's Environments tab. */
export function EnvCard({ env }: { env: Environment }) {
  return (
    <div
      className={clsx(
        'relative flex min-w-0 flex-col gap-2.5 rounded-xl border border-line bg-panel p-4 transition-colors hover:border-line-strong',
        env.stopped && 'opacity-70',
      )}
    >
      <div className="flex items-center gap-2">
        <StatusDot env={env} />
        <Link
          to="/p/$key/environments/$name"
          params={{ key: env.project, name: env.name }}
          className={clsx(
            NAME_FONT,
            'min-w-0 truncate text-[15px] font-semibold after:absolute after:inset-0 after:rounded-xl focus-visible:outline-none focus-visible:after:ring-2 focus-visible:after:ring-accent/40',
          )}
        >
          {env.name}
        </Link>
        <KindBadge env={env} />
        <span className="ml-auto shrink-0 text-xs text-muted">{statusText(env)}</span>
      </div>
      <ExternalUrl url={env.url} className="text-[13px]" />
      <EnvNotes env={env} />
      <RunsLine env={env} />
      {!env.stopped && env.interval_s > 0 && env.uptime_24h != null && (
        <div className="flex items-center gap-3">
          <div className="min-w-0 flex-1">
            <UptimeSpark env={env} />
          </div>
          <div className="shrink-0 text-right">
            <div className="text-[13px] font-medium tabular-nums">{pct(env.uptime_24h)}</div>
            <div className="text-[11px] text-subtle">24 h</div>
          </div>
        </div>
      )}
      <div className="flex min-h-4 items-center gap-2 text-xs text-muted">
        {env.last_deploy ? (
          <LastDeploy d={env.last_deploy} />
        ) : (
          <span className="text-subtle">No deployments recorded</span>
        )}
      </div>
    </div>
  )
}

/** One environment in the all-projects list: a single dense row. */
export function EnvRow({ env }: { env: Environment }) {
  return (
    <div
      className={clsx(
        'relative grid grid-cols-[auto_1fr_auto] items-center gap-x-3 gap-y-1 px-3 py-2.5 hover:bg-hover/50 sm:grid-cols-[auto_minmax(0,1.1fr)_minmax(0,1.4fr)_minmax(0,1.2fr)_auto]',
        env.stopped && 'opacity-60',
      )}
    >
      <span className="flex size-4 items-center justify-center">
        <StatusDot env={env} />
      </span>
      <div className="flex min-w-0 items-center gap-2">
        <Link
          to="/p/$key/environments/$name"
          params={{ key: env.project, name: env.name }}
          className={clsx(
            NAME_FONT,
            'min-w-0 truncate text-[13px] font-medium after:absolute after:inset-0 focus-visible:outline-none focus-visible:after:ring-2 focus-visible:after:ring-inset focus-visible:after:ring-accent/40',
          )}
        >
          {env.name}
        </Link>
        <KindBadge env={env} />
      </div>
      <span className="col-span-3 col-start-2 row-start-2 min-w-0 truncate sm:col-span-1 sm:col-start-3 sm:row-start-1">
        <ExternalUrl url={env.url} className="text-[13px]" />
      </span>
      <span className="col-span-3 col-start-2 row-start-3 min-w-0 sm:col-span-1 sm:col-start-4 sm:row-start-1">
        <RunsLine env={env} />
      </span>
      <span className="col-start-3 row-start-1 text-right text-xs text-muted sm:col-start-5">
        {statusText(env)}
      </span>
      {!env.stopped && env.status !== 'up' && env.error && (
        <span className="col-span-3 col-start-2 row-start-4 truncate text-xs text-danger sm:row-start-2">
          {env.error}
        </span>
      )}
    </div>
  )
}

/** Add an environment to a project. */
export function AddEnvironmentDialog({
  project,
  open,
  onClose,
  kind: initialKind = 'staging',
}: {
  project: string
  open: boolean
  onClose: () => void
  kind?: EnvKind
}) {
  const [name, setName] = useState('')
  const [kind, setKind] = useState<EnvKind>(initialKind)
  const [url, setUrl] = useState('')
  const [interval, setInterval_] = useState(60)
  const navigate = useNavigate()
  const create = useCreateEnv(project)
  const urlBad = url.trim() !== '' && !/^https?:\/\/[^\s/]+/i.test(url.trim())
  const submit = () => {
    if (!name.trim() || urlBad) return
    create.mutate(
      {
        name: name.trim(),
        kind,
        url: url.trim(),
        interval_s: url.trim() ? interval : 0,
      },
      {
        onSuccess: (e) => {
          toast(`Added ${e.name}`, { kind: 'success' })
          setName('')
          setUrl('')
          onClose()
          navigate({ to: '/p/$key/environments/$name', params: { key: project, name: e.name } })
        },
      },
    )
  }
  return (
    <Dialog
      open={open}
      onClose={onClose}
      title="Add environment"
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            loading={create.isPending}
            disabled={!name.trim() || urlBad}
            onClick={submit}
          >
            Add environment
          </Button>
        </>
      }
    >
      <form
        className="space-y-4 p-4"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <Field
          label="Name"
          note="Letters, digits, dots and dashes. CI uses it: wp record deploy staging …"
        >
          <Input
            autoFocus
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="staging"
            autoComplete="off"
            maxLength={63}
          />
        </Field>
        <Field label="Kind">
          <Select value={kind} onChange={(e) => setKind(e.target.value as EnvKind)}>
            {(Object.keys(KIND_LABEL) as EnvKind[]).map((k) => (
              <option key={k} value={k}>
                {KIND_LABEL[k]}
              </option>
            ))}
          </Select>
        </Field>
        <Field
          label="Address"
          error={urlBad ? 'Start with https:// or http://' : undefined}
          note="Waypoint requests this to see whether it is up. Leave it empty to only record deployments."
        >
          <Input
            type="url"
            inputMode="url"
            value={url}
            onChange={(e) => setUrl(e.target.value)}
            placeholder="https://staging.example.com"
            autoComplete="off"
          />
        </Field>
        {url.trim() !== '' && (
          <Field label="Check">
            <Select value={String(interval)} onChange={(e) => setInterval_(Number(e.target.value))}>
              {CHECK_INTERVALS.map((i) => (
                <option key={i.value} value={i.value}>
                  {i.label}
                </option>
              ))}
            </Select>
          </Field>
        )}
        <button type="submit" className="hidden" aria-hidden tabIndex={-1} />
      </form>
    </Dialog>
  )
}
